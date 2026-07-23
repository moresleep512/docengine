package document

import "io"

const saveProgressQuantum = int64(4 << 20)
const compactionProgressQuantum = int64(4 << 20)

func (s *Session) beginPersistenceLocked(targetRevision uint64, totalBytes int64) (PersistenceProgress, Metadata) {
	s.nextPersistenceID++
	if s.nextPersistenceID == 0 {
		s.nextPersistenceID++
	}
	progress := PersistenceProgress{
		OperationID: s.nextPersistenceID, TargetRevision: targetRevision, TotalBytes: totalBytes,
	}
	metadata := s.metadataLocked()
	s.events.publish(SessionEvent{Kind: EventSaveStarted, Metadata: metadata, Persistence: progress})
	return progress, metadata
}

func (s *Session) publishPersistenceEvent(kind EventKind, metadata Metadata, progress PersistenceProgress, cause error) {
	s.events.publish(SessionEvent{Kind: kind, Metadata: metadata, Persistence: progress, Cause: cause})
}

type saveProgressWriter struct {
	destination io.Writer
	prefix      int64
	total       int64
	written     int64
	last        int64
	next        int64
	report      func(int64)
}

func newSaveProgressWriter(destination io.Writer, prefix, total int64, report func(int64)) *saveProgressWriter {
	next := prefix + saveProgressQuantum
	if next > total {
		next = total
	}
	return &saveProgressWriter{destination: destination, prefix: prefix, total: total, last: -1, next: next, report: report}
}

func (w *saveProgressWriter) Write(value []byte) (int, error) {
	n, err := w.destination.Write(value)
	w.written += int64(n)
	completed := w.prefix + w.written
	if completed > w.total {
		completed = w.total
	}
	if completed >= w.next || err != nil {
		w.publish(completed)
		w.next = completed + saveProgressQuantum
		if w.next > w.total {
			w.next = w.total
		}
	}
	return n, err
}

func (w *saveProgressWriter) finish() {
	w.publish(w.total)
}

func (w *saveProgressWriter) publish(completed int64) {
	if completed == w.last {
		return
	}
	w.last = completed
	w.report(completed)
}
