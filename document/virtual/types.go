// Package virtual provides format-neutral, revision-bound document
// virtualization. It understands UTF-8 byte ranges, logical pages, opaque
// fragments, and host-defined fixed-point measures; it does not understand
// document formats or layout units.
package virtual

import (
	"context"
	"errors"
	"io"
)

const (
	DefaultTargetPageBytes  int64 = 64 << 10
	DefaultMaximumPageBytes int64 = 256 << 10
	MaximumPageBytes        int64 = 64 << 20

	DefaultMaximumFragments             = 1 << 20
	MaximumFragments                    = 16 << 20
	DefaultMaximumConcurrentTasks       = 4
	MaximumConcurrentTasks              = 1024
	DefaultCacheBytes             int64 = 8 << 20
	MaximumCacheBytes             int64 = 1 << 30
	DefaultMaximumKeyBytes        int64 = 16 << 20
	MaximumKeyBytes               int64 = 256 << 20

	DefaultWindowBytes     int64   = 4 << 20
	MaximumWindowBytes     int64   = 1 << 30
	DefaultWindowPages             = 128
	MaximumWindowPages             = 1 << 20
	DefaultWindowFragments         = 4096
	MaximumWindowFragments         = MaximumFragments
	DefaultWindowMeasure   Measure = 1 << 50
	MaximumWindowMeasure   Measure = 1<<63 - 1

	DefaultMaximumInflightBytes int64 = 16 << 20
	MaximumInflightBytes        int64 = 4 << 30
)

var (
	ErrInvalidContext     = errors.New("virtual: nil context")
	ErrInvalidSource      = errors.New("virtual: invalid source")
	ErrInvalidOptions     = errors.New("virtual: invalid options")
	ErrInvalidRequest     = errors.New("virtual: invalid request")
	ErrInvalidPublication = errors.New("virtual: invalid fragment publication")
	ErrInvalidFragment    = errors.New("virtual: invalid fragment")
	ErrInvalidUTF8        = errors.New("virtual: source is not UTF-8")
	ErrSourceInconsistent = errors.New("virtual: source length or content changed")
	ErrRevisionMismatch   = errors.New("virtual: revision mismatch")
	ErrStaleGeneration    = errors.New("virtual: stale fragment generation")
	ErrGenerationOverflow = errors.New("virtual: fragment generation overflow")
	ErrBudgetExceeded     = errors.New("virtual: anchor does not fit request budget")
	ErrNotFound           = errors.New("virtual: fragment not found")
	ErrMeasureUnavailable = errors.New("virtual: measure index is unavailable")
	ErrBusy               = errors.New("virtual: task limit reached")
	ErrClosed             = errors.New("virtual: pager closed")
	ErrInvalidPager       = errors.New("virtual: invalid previous pager")
	ErrLineageMismatch    = errors.New("virtual: pager lineage mismatch")
	ErrOperationOverflow  = errors.New("virtual: operation identifier exhausted")
)

// Source is an immutable UTF-8 byte source.
type Source interface {
	io.ReaderAt
	Len() int64
}

// OwnedSource transfers its lifetime to BuildOwned. Its Close method must not
// re-enter the owning Pager.
type OwnedSource interface {
	Source
	io.Closer
}

// Lineage is an opaque identity shared by Pagers built from one trusted Source
// history. Pointer identity is intentional.
type Lineage struct{ marker byte }

// NewLineage creates a unique Pager lineage token.
func NewLineage() *Lineage { return &Lineage{} }

// Measure is a non-negative fixed-point quantity whose unit and scale are
// defined exclusively by the host.
type Measure int64

// Budget is a hard upper bound for one result. A zero field selects the
// corresponding limit resolved from Options.
type Budget struct {
	Bytes     int64
	Pages     int
	Fragments int
	Measure   Measure
}

// Options controls page partitioning and bounded resource use.
type Options struct {
	TargetPageBytes  int64
	MaximumPageBytes int64
	MaximumFragments int
	MaximumTasks     int
	MaximumKeyBytes  int64
	CacheBytes       int64
	// MaximumInflightBytes bounds bytes reserved by concurrent Window and
	// ReadPage operations before their results transfer to the caller.
	MaximumInflightBytes int64
	Window               Budget
	DisableCache         bool
	// Lineage is inherited by Rebuild and overridden by Session.
	Lineage *Lineage
	// Observer receives synchronous lifecycle progress without a Pager lock.
	// It may inspect Stats, but must not block indefinitely or synchronously
	// call a task-bearing operation or Close on the Pager that invoked it.
	Observer ProgressObserver
}

// Fragment is a format-neutral, host-defined range. ID and DataKey are opaque
// to the core. Ranges are ordered and non-overlapping; analyzed gaps continue
// to use logical Page fallback.
type Fragment struct {
	ID      string
	Start   int64
	End     int64
	Measure Measure
	DataKey string
}

// Publication atomically replaces all Fragments known below IndexedThrough.
// BaseGeneration is a compare-and-swap guard. Complete requires
// IndexedThrough to equal the document byte length.
type Publication struct {
	Revision       uint64
	BaseGeneration uint64
	IndexedThrough int64
	Complete       bool
	Fragments      []Fragment
}

// FragmentRequest is passed to a host FragmentProvider without holding a
// Pager or Session lock.
type FragmentRequest struct {
	Revision           uint64
	BaseGeneration     uint64
	ByteLength         int64
	MaxFragments       int
	MaxKeyBytes        int64
	MaxFragmentMeasure Measure
	// Report publishes a monotonic candidate watermark and Fragment count.
	// It is valid only during the Fragments call.
	Report func(FragmentProgress) error
}

// FragmentProgress is provider-reported progress toward one replacement.
type FragmentProgress struct {
	IndexedThrough int64
	Fragments      int
}

// FragmentResult is a provider-built replacement below one indexed watermark.
type FragmentResult struct {
	IndexedThrough int64
	Complete       bool
	Fragments      []Fragment
}

// FragmentProvider derives format-neutral fragments for one immutable
// revision. Calls must honor Context cancellation. A provider may inspect
// read-only metadata such as Stats, but it must not synchronously invoke a
// task-bearing operation or Close on the Pager that invoked it.
type FragmentProvider interface {
	Fragments(context.Context, FragmentRequest) (FragmentResult, error)
}

// ProgressKind identifies the operation that produced virtual progress.
type ProgressKind uint8

const (
	ProgressPublish ProgressKind = iota + 1
	ProgressRefresh
)

// ProgressStage identifies one operation lifecycle transition.
type ProgressStage uint8

const (
	ProgressStarted ProgressStage = iota + 1
	ProgressAdvanced
	ProgressCompleted
	ProgressFailed
)

// Progress is an immutable operation update. IndexedThrough and Fragments are
// monotonic within one OperationID. PublishedGeneration is nonzero only after
// an atomic publication succeeds.
type Progress struct {
	OperationID         uint64
	Kind                ProgressKind
	Stage               ProgressStage
	Revision            uint64
	BaseGeneration      uint64
	PublishedGeneration uint64
	ByteLength          int64
	IndexedThrough      int64
	Fragments           int
	Complete            bool
	Published           bool
	Cause               error
}

// ProgressObserver receives synchronous progress without a Pager lock. It may
// inspect Stats, but must not synchronously invoke a task-bearing operation or
// Close on the Pager that invoked it.
type ProgressObserver interface {
	ObserveVirtualProgress(Progress)
}

// ProgressObserverFunc adapts a function to ProgressObserver.
type ProgressObserverFunc func(Progress)

func (f ProgressObserverFunc) ObserveVirtualProgress(progress Progress) {
	if f != nil {
		f(progress)
	}
}

// Affinity resolves an anchor exactly on a Measure boundary.
type Affinity uint8

const (
	AffinityBefore Affinity = iota + 1
	AffinityAfter
)

type pagerIdentity struct {
	marker byte
}

// PageKey identifies one page issued by one exact Pager, revision, and
// Fragment generation. Its zero value and keys issued by another Pager are
// invalid.
type PageKey struct {
	Revision   uint64
	Generation uint64
	Index      int
	Start      int64
	End        int64
	identity   *pagerIdentity
}

// Page is a bounded content result. MeasureStart/MeasureEnd describe the
// parent Fragment's atomic interval and are intentionally repeated on every
// continuation page instead of being guessed from byte proportions.
type Page struct {
	Key                   PageKey
	StartLine             int64
	EndLine               int64
	ContinuesFromPrevious bool
	ContinuesToNext       bool
	FragmentID            string
	DataKey               string
	FragmentIndex         int
	ContinuationIndex     int
	ContinuationCount     int
	MeasureStart          Measure
	MeasureEnd            Measure
	// Indexed reports that the provider has analyzed this byte range. It does
	// not imply that the Page belongs to a Fragment.
	Indexed bool
	Content []byte
}

// Window is an immutable query result for one exact state.
type Window struct {
	Revision        uint64
	Generation      uint64
	Pages           []Page
	Bytes           int64
	Fragments       int
	Measure         Measure
	IndexedThrough  int64
	Complete        bool
	TruncatedBefore bool
	TruncatedAfter  bool
}

type ByteWindowRequest struct {
	Revision   uint64
	Generation uint64
	Offset     int64
	Before     int64
	After      int64
	Budget     Budget
}

type FragmentWindowRequest struct {
	Revision   uint64
	Generation uint64
	ID         string
	// Continuation selects the Fragment page that acts as the anchor. Zero
	// selects the first page.
	Continuation int
	Before       int
	After        int
	Budget       Budget
}

type MeasureWindowRequest struct {
	Revision   uint64
	Generation uint64
	Offset     Measure
	Affinity   Affinity
	Before     Measure
	After      Measure
	Budget     Budget
}

type Stats struct {
	Revision             uint64
	Generation           uint64
	ByteLength           int64
	LogicalPages         int
	Pages                int
	Fragments            int
	IndexedThrough       int64
	Complete             bool
	TotalMeasure         Measure
	TargetPageBytes      int64
	MaximumPageBytes     int64
	CacheBytes           int64
	CacheEntries         int
	MaximumCacheBytes    int64
	ActiveTasks          int
	MaximumTasks         int
	MaximumKeyBytes      int64
	KeyBytes             int64
	WindowBytes          int64
	WindowPages          int
	WindowFragments      int
	WindowMeasure        Measure
	ActiveInflightBytes  int64
	MaximumInflightBytes int64
	Closing              bool
	Closed               bool
}
