package coordinate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestIndexMapsEveryUTF8Coordinate(t *testing.T) {
	body := []byte("aé\n世界\r\n🙂")
	source := &testSource{body: body}
	index, err := Build(context.Background(), source, 7, Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	stats := index.Stats()
	if stats.Revision != 7 || stats.ByteLength != int64(len(body)) || stats.RuneCount != int64(utf8.RuneCount(body)) || stats.LineCount != 3 || stats.CheckpointBytes != 4 || stats.CheckpointCount < 3 {
		t.Fatalf("Stats = %+v", stats)
	}
	source.mu.Lock()
	source.maximumRead = 0
	source.mu.Unlock()

	positions := referencePositions(body)
	byByte := make(map[int64]Position, len(positions))
	for _, position := range positions {
		byByte[position.ByteOffset] = position
		got, err := index.RuneToByte(context.Background(), position.RuneOffset)
		if err != nil || got != position.ByteOffset {
			t.Fatalf("RuneToByte(%d) = (%d, %v), want %d", position.RuneOffset, got, err, position.ByteOffset)
		}
		got, err = index.PositionToByte(context.Background(), position.Line, position.Column)
		if err != nil || got != position.ByteOffset {
			t.Fatalf("PositionToByte(%d,%d) = (%d, %v), want %d", position.Line, position.Column, got, err, position.ByteOffset)
		}
	}
	for offset := int64(0); offset <= int64(len(body)); offset++ {
		got, err := index.ByteToPosition(context.Background(), offset)
		want, boundary := byByte[offset]
		if !boundary {
			if !errors.Is(err, ErrNotRuneBoundary) {
				t.Fatalf("ByteToPosition(%d) = (%+v, %v), want boundary error", offset, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Fatalf("ByteToPosition(%d) = (%+v, %v), want %+v", offset, got, err, want)
		}
	}
	for line, want := range []int64{0, 4, 12} {
		got, err := index.LineStart(context.Background(), int64(line))
		if err != nil || got != want {
			t.Fatalf("LineStart(%d) = (%d, %v), want %d", line, got, err, want)
		}
	}
	if source.maximumRead > 4+utf8.UTFMax {
		t.Fatalf("maximum query read = %d", source.maximumRead)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ByteToPosition(context.Background(), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed ByteToPosition = %v", err)
	}
	if _, err := index.RuneToByte(context.Background(), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed RuneToByte = %v", err)
	}
	if _, err := index.PositionToByte(context.Background(), 0, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed PositionToByte = %v", err)
	}
}

func TestIndexRejectsInvalidCoordinatesAndCancellation(t *testing.T) {
	index, err := Build(context.Background(), &testSource{body: []byte("ab\n")}, 1, Options{CheckpointBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	for _, offset := range []int64{-1, 4} {
		if _, err := index.ByteToPosition(context.Background(), offset); !errors.Is(err, ErrInvalidOffset) {
			t.Fatalf("ByteToPosition(%d) = %v", offset, err)
		}
		if _, err := index.RuneToByte(context.Background(), offset); !errors.Is(err, ErrInvalidOffset) {
			t.Fatalf("RuneToByte(%d) = %v", offset, err)
		}
	}
	for _, value := range [][2]int64{{-1, 0}, {2, 0}, {0, -1}, {0, 3}} {
		if _, err := index.PositionToByte(context.Background(), value[0], value[1]); !errors.Is(err, ErrInvalidPosition) {
			t.Fatalf("PositionToByte%v = %v", value, err)
		}
	}
	if _, err := index.ByteToPosition(nil, 0); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil ByteToPosition = %v", err)
	}
	if _, err := index.RuneToByte(nil, 0); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil RuneToByte = %v", err)
	}
	if _, err := index.PositionToByte(nil, 0, 0); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil PositionToByte = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.ByteToPosition(canceled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ByteToPosition = %v", err)
	}
	if _, err := index.RuneToByte(canceled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RuneToByte = %v", err)
	}
	if _, err := index.PositionToByte(canceled, 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled PositionToByte = %v", err)
	}
}

func TestBuildAndOwnedLifetimeBoundaries(t *testing.T) {
	if _, err := Build(nil, &testSource{}, 0, Options{}); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil context = %v", err)
	}
	if _, err := Build(context.Background(), nil, 0, Options{}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil source = %v", err)
	}
	if _, err := BuildOwned(context.Background(), nil, 0, Options{}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil owned source = %v", err)
	}
	if _, err := Build(context.Background(), &testSource{length: -1, overrideLength: true}, 0, Options{}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("negative length = %v", err)
	}
	for _, size := range []int64{-1, MaximumCheckpointBytes + 1} {
		if _, err := Build(context.Background(), &testSource{}, 0, Options{CheckpointBytes: size}); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("checkpoint %d = %v", size, err)
		}
	}
	for _, body := range [][]byte{{0xff}, {0xe2, 0x82}} {
		if _, err := Build(context.Background(), &testSource{body: body}, 0, Options{CheckpointBytes: 1}); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("invalid UTF-8 %x = %v", body, err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(canceled, &testSource{body: []byte("abc")}, 0, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled build = %v", err)
	}

	owned := &testSource{body: []byte("hello")}
	index, err := BuildOwned(context.Background(), owned, 9, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if owned.closeCalls != 0 {
		t.Fatalf("close calls before Close = %d", owned.closeCalls)
	}
	if err := index.Close(); err != nil || owned.closeCalls != 1 {
		t.Fatalf("Close = (%v, calls=%d)", err, owned.closeCalls)
	}
	if err := index.Close(); err != nil || owned.closeCalls != 1 {
		t.Fatalf("second Close = (%v, calls=%d)", err, owned.closeCalls)
	}

	sentinel := errors.New("close")
	invalidOwned := &testSource{body: []byte{0xff}, closeErr: sentinel}
	if _, err := BuildOwned(context.Background(), invalidOwned, 0, Options{}); !errors.Is(err, ErrInvalidUTF8) || !errors.Is(err, sentinel) || invalidOwned.closeCalls != 1 {
		t.Fatalf("failed owned build = (%v, calls=%d)", err, invalidOwned.closeCalls)
	}
}

func TestSourceFaultBoundaries(t *testing.T) {
	sentinel := errors.New("read")
	tests := []struct {
		name string
		src  *testSource
		want error
	}{
		{name: "read error", src: &testSource{body: []byte("abc"), readErr: sentinel}, want: sentinel},
		{name: "zero read", src: &testSource{body: []byte("abc"), zeroRead: true}, want: io.ErrUnexpectedEOF},
		{name: "negative count", src: &testSource{body: []byte("abc"), countAdjustment: -4}, want: ErrSourceInconsistent},
		{name: "oversized count", src: &testSource{body: []byte("abc"), countAdjustment: 1}, want: ErrSourceInconsistent},
		{name: "early EOF", src: &testSource{body: []byte("abc"), length: 4, overrideLength: true}, want: io.EOF},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Build(context.Background(), test.src, 0, Options{}); !errors.Is(err, test.want) {
				t.Fatalf("Build = %v", err)
			}
		})
	}

	source := &testSource{body: []byte("abcdef")}
	index, err := Build(context.Background(), source, 0, Options{CheckpointBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	source.readErr = sentinel
	if _, err := index.ByteToPosition(context.Background(), 1); !errors.Is(err, sentinel) {
		t.Fatalf("query read error = %v", err)
	}
	source.readErr = nil
	source.zeroRead = true
	if _, err := index.RuneToByte(context.Background(), 1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("query zero read = %v", err)
	}
	source.zeroRead = false
	source.countAdjustment = 1
	if _, err := index.PositionToByte(context.Background(), 0, 1); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("query count = %v", err)
	}
	source.countAdjustment = -10
	if _, err := index.ByteToPosition(context.Background(), 1); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("query negative count = %v", err)
	}
}

func TestCancellationAndMutatedSourceDuringScanning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &testSource{body: []byte("abc"), afterRead: cancel}
	if _, err := Build(ctx, source, 0, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during scan = %v", err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := Build(canceled, &testSource{}, 0, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel after empty scan = %v", err)
	}
	empty, err := Build(context.Background(), &testSource{}, 0, Options{})
	if err != nil || empty.Stats().LineCount != 1 {
		t.Fatalf("empty index = (%+v, %v)", empty, err)
	}

	for _, query := range []func(*Index, context.Context) error{
		func(index *Index, ctx context.Context) error { _, err := index.ByteToPosition(ctx, 1); return err },
		func(index *Index, ctx context.Context) error { _, err := index.RuneToByte(ctx, 1); return err },
		func(index *Index, ctx context.Context) error { _, err := index.PositionToByte(ctx, 0, 1); return err },
	} {
		source := &testSource{body: []byte("abcdef")}
		index, err := Build(context.Background(), source, 0, Options{})
		if err != nil {
			t.Fatal(err)
		}
		queryCtx, queryCancel := context.WithCancel(context.Background())
		source.afterRead = queryCancel
		if err := query(index, queryCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel during query = %v", err)
		}
	}

	for _, query := range []func(*Index) error{
		func(index *Index) error { _, err := index.ByteToPosition(context.Background(), 1); return err },
		func(index *Index) error { _, err := index.RuneToByte(context.Background(), 1); return err },
		func(index *Index) error { _, err := index.PositionToByte(context.Background(), 0, 1); return err },
	} {
		source := &testSource{body: []byte("abcd")}
		index, err := Build(context.Background(), source, 0, Options{})
		if err != nil {
			t.Fatal(err)
		}
		source.body[0] = 0xff
		if err := query(index); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("mutated UTF-8 query = %v", err)
		}
	}
}

func TestMalformedIndexQueriesRemainBounded(t *testing.T) {
	source := &testSource{body: bytes.Repeat([]byte{'a'}, 10)}
	index := &Index{
		source: source, byteLength: 10, runeCount: 10, lineCount: 1,
		checkpointBytes: 1, checkpoints: []checkpoint{{}},
	}
	if _, err := index.ByteToPosition(context.Background(), 10); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("bounded byte query = %v", err)
	}
	if _, err := index.RuneToByte(context.Background(), 10); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("bounded rune query = %v", err)
	}
	if _, err := index.PositionToByte(context.Background(), 0, 10); !errors.Is(err, ErrInvalidPosition) {
		t.Fatalf("bounded position query = %v", err)
	}
}

func TestInternalWindowAndDecodeBoundaries(t *testing.T) {
	if nextCheckpointAfter(math.MaxInt64-1, 4) != math.MaxInt64 || nextCheckpointAfter(8, 4) != 12 {
		t.Fatal("checkpoint saturation failed")
	}
	source := &testSource{body: []byte("x"), baseOffset: math.MaxInt64 - 1}
	index := &Index{source: source, byteLength: math.MaxInt64, checkpointBytes: 4}
	data, err := index.readWindow(context.Background(), math.MaxInt64-1)
	if err != nil || len(data) != 1 {
		t.Fatalf("overflow-safe window = (%d, %v)", len(data), err)
	}
	if _, _, err := decodeRune(nil); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("empty decode = %v", err)
	}
	if _, _, err := decodeRune([]byte{0xe2}); !errors.Is(err, ErrSourceInconsistent) {
		t.Fatalf("short decode = %v", err)
	}
	if _, _, err := decodeRune([]byte{0xff}); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid decode = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.readWindow(canceled, math.MaxInt64-1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled window = %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	partial := &testSource{body: []byte("xy"), maximumPerRead: 1, afterRead: stop}
	partialIndex := &Index{source: partial, byteLength: 2, checkpointBytes: 4}
	if _, err := partialIndex.readWindow(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("window canceled between reads = %v", err)
	}
}

func referencePositions(body []byte) []Position {
	result := []Position{{}}
	state := checkpoint{}
	for offset := 0; offset < len(body); {
		r, size := utf8.DecodeRune(body[offset:])
		advance(&state, r, size)
		result = append(result, positionFor(state))
		offset += size
	}
	return result
}

type testSource struct {
	mu              sync.Mutex
	body            []byte
	length          int64
	overrideLength  bool
	readErr         error
	zeroRead        bool
	countAdjustment int
	maximumRead     int
	closeCalls      int
	closeErr        error
	baseOffset      int64
	maximumPerRead  int
	afterRead       func()
}

func (s *testSource) Len() int64 {
	if s.overrideLength {
		return s.length
	}
	return int64(len(s.body))
}

func (s *testSource) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > s.maximumRead {
		s.maximumRead = len(p)
	}
	if s.readErr != nil {
		return 0, s.readErr
	}
	if s.zeroRead {
		return 0, nil
	}
	if s.maximumPerRead > 0 && len(p) > s.maximumPerRead {
		p = p[:s.maximumPerRead]
	}
	n, err := bytes.NewReader(s.body).ReadAt(p, off-s.baseOffset)
	if s.afterRead != nil {
		s.afterRead()
	}
	return n + s.countAdjustment, err
}

func (s *testSource) Close() error {
	s.closeCalls++
	return s.closeErr
}
