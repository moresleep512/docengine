package recovery

import (
	"bytes"
	"testing"
)

func FuzzJournalDecoders(f *testing.F) {
	payload, _, err := encodeBatchPayload(1, []ReplaceOperation{{Start: 2, DeleteLength: 1, Inserted: []byte("seed")}})
	if err != nil {
		f.Fatal(err)
	}
	validHeader := encodeFrameHeader(Frame{Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize}, payload)
	f.Add(payload, uint16(1), uint64(1), uint16(batchRecordSize))
	f.Add(validHeader, uint16(0), uint64(0), uint16(0))
	f.Add([]byte{}, uint16(maximumBatchSize+1), uint64(1), uint16(batchRecordSize))

	f.Fuzz(func(t *testing.T, data []byte, count uint16, revision uint64, recordSize uint16) {
		header := make([]byte, frameHeaderSize)
		copy(header, data)
		frame, _, ok := decodeFrameHeader(header)
		if ok && frame.Kind != FrameReplace && frame.Kind != FrameRoot && frame.Kind != FrameBatch {
			t.Fatalf("accepted unknown frame kind %d", frame.Kind)
		}

		batch := Frame{
			Kind: FrameBatch, Revision: revision, Start: int64(count), DeleteLength: int64(recordSize),
			InsertLength: int64(len(data)), PayloadOffset: 0,
		}
		frames, valid, decodeErr := decodeBatchFrames(bytes.NewReader(data), batch)
		if decodeErr != nil || !valid {
			return
		}
		if len(frames) != int(count) || count == 0 || count > maximumBatchSize || revision == 0 || recordSize != batchRecordSize {
			t.Fatalf("invalid batch accepted: count=%d revision=%d recordSize=%d frames=%d", count, revision, recordSize, len(frames))
		}
		cursor := int64(count) * batchRecordSize
		for index, frame := range frames {
			if frame.Kind != FrameReplace || frame.Revision != revision+uint64(index) || frame.PayloadOffset != cursor || frame.InsertLength < 0 {
				t.Fatalf("frame %d violates batch invariants: %+v, cursor=%d", index, frame, cursor)
			}
			cursor += frame.InsertLength
		}
		if cursor != int64(len(data)) {
			t.Fatalf("accepted batch consumed %d of %d bytes", cursor, len(data))
		}
	})
}
