package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"os"
	"runtime"
	"testing"
)

func TestAppendBatchRejectsInvalidInputWithoutWriting(t *testing.T) {
	tests := []struct {
		name       string
		revision   uint64
		group      uint64
		operations []ReplaceOperation
	}{
		{name: "no operations", revision: 1, group: 1},
		{name: "too many", revision: 1, group: 1, operations: make([]ReplaceOperation, maximumBatchSize+1)},
		{name: "zero revision", group: 1, operations: []ReplaceOperation{{}}},
		{name: "zero group", revision: 1, operations: []ReplaceOperation{{}}},
		{name: "revision overflow", revision: math.MaxUint64, group: 1, operations: []ReplaceOperation{{}, {}}},
		{name: "negative start", revision: 1, group: 1, operations: []ReplaceOperation{{Start: -1}}},
		{name: "negative delete", revision: 1, group: 1, operations: []ReplaceOperation{{DeleteLength: -1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal, path := openTestJournal(t)
			defer journal.Close()
			if _, err := journal.AppendBatch(test.revision, test.group, test.operations); !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("AppendBatch = %v", err)
			}
			info, _ := os.Stat(path)
			if info.Size() != fileHeaderSize {
				t.Fatalf("journal size = %d", info.Size())
			}
		})
	}
}

func TestMaximumBatchAndPayloadOffsets(t *testing.T) {
	journal, _ := openTestJournal(t)
	defer journal.Close()
	operations := make([]ReplaceOperation, maximumBatchSize)
	for index := range operations {
		operations[index] = ReplaceOperation{Start: int64(index), Inserted: []byte{byte(index)}}
	}
	appendResult, err := journal.AppendBatch(1, 99, operations)
	if err != nil || len(appendResult.PayloadOffsets) != maximumBatchSize {
		t.Fatalf("AppendBatch = (%+v, %v)", appendResult, err)
	}
	replay, err := journal.Replay()
	if err != nil || replay.Truncated || len(replay.Batches) != 1 || len(replay.Batches[0].Operations) != maximumBatchSize {
		t.Fatalf("Replay = (%+v, %v)", replay, err)
	}
	for index, operation := range replay.Batches[0].Operations {
		if operation.PayloadOffset != appendResult.PayloadOffsets[index] || !bytes.Equal(readInserted(t, journal, operation), []byte{byte(index)}) {
			t.Fatalf("operation %d = %+v", index, operation)
		}
	}
}

func TestBatchHeaderDecoderRejectsEveryInvalidField(t *testing.T) {
	payload, _, err := encodeBatchPayload(1, 1, []ReplaceOperation{{Inserted: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	valid := encodeBatchHeader(Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, 1)}, payload)
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "length", mutate: func(header []byte) { header = header[:len(header)-1] }},
		{name: "magic", mutate: func(header []byte) { header[0] ^= 1 }},
		{name: "version", mutate: func(header []byte) { binary.LittleEndian.PutUint16(header[8:10], 3) }},
		{name: "flags", mutate: func(header []byte) { binary.LittleEndian.PutUint16(header[10:12], 1) }},
		{name: "header size", mutate: func(header []byte) { binary.LittleEndian.PutUint32(header[12:16], 1) }},
		{name: "zero revision", mutate: func(header []byte) { binary.LittleEndian.PutUint64(header[16:24], 0) }},
		{name: "zero group", mutate: func(header []byte) { binary.LittleEndian.PutUint64(header[24:32], 0) }},
		{name: "zero count", mutate: func(header []byte) { binary.LittleEndian.PutUint32(header[32:36], 0) }},
		{name: "large count", mutate: func(header []byte) { binary.LittleEndian.PutUint32(header[32:36], maximumBatchSize+1) }},
		{name: "record size", mutate: func(header []byte) { binary.LittleEndian.PutUint32(header[36:40], 1) }},
		{name: "payload overflow", mutate: func(header []byte) { binary.LittleEndian.PutUint64(header[40:48], math.MaxUint64) }},
		{name: "reserved one", mutate: func(header []byte) { header[48] = 1 }},
		{name: "reserved two", mutate: func(header []byte) { header[60] = 1 }},
		{name: "revision overflow", mutate: func(header []byte) {
			binary.LittleEndian.PutUint64(header[16:24], math.MaxUint64)
			binary.LittleEndian.PutUint32(header[32:36], 2)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := append([]byte(nil), valid...)
			if test.name == "length" {
				header = header[:len(header)-1]
			} else {
				test.mutate(header)
			}
			if _, _, _, ok := decodeBatchHeader(header); ok {
				t.Fatal("invalid header accepted")
			}
		})
	}
}

func TestMalformedBatchPayloadIsTruncated(t *testing.T) {
	record := make([]byte, batchRecordSize)
	validBatch := Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, 1)}
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "short metadata", payload: record[:len(record)-1]},
		{name: "negative start", payload: func() []byte {
			value := append([]byte(nil), record...)
			binary.LittleEndian.PutUint64(value[0:8], math.MaxUint64)
			return value
		}()},
		{name: "negative delete", payload: func() []byte {
			value := append([]byte(nil), record...)
			binary.LittleEndian.PutUint64(value[8:16], math.MaxUint64)
			return value
		}()},
		{name: "negative insert", payload: func() []byte {
			value := append([]byte(nil), record...)
			binary.LittleEndian.PutUint64(value[16:24], math.MaxUint64)
			return value
		}()},
		{name: "missing insert", payload: func() []byte {
			value := append([]byte(nil), record...)
			binary.LittleEndian.PutUint64(value[16:24], 1)
			return value
		}()},
		{name: "trailing byte", payload: append(append([]byte(nil), record...), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal, path := openTestJournal(t)
			fingerprint, _ := ReadFingerprint(path)
			_ = journal.Close()
			header := encodeBatchHeader(validBatch, test.payload)
			content, _ := os.ReadFile(path)
			content = append(content, header...)
			content = append(content, test.payload...)
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			opened, replay, err := Open(path, fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			if !replay.Truncated || replay.ValidBytes != fileHeaderSize || len(replay.Batches) != 0 {
				t.Fatalf("Replay = %+v", replay)
			}
		})
	}
}

func TestFingerprintPathNormalizationAndHeaderIntegrity(t *testing.T) {
	body := []byte("body")
	left := testFingerprint(filepathForCaseTest("MixedCase"), body)
	right := testFingerprint(filepathForCaseTest("mixedcase"), body)
	if runtime.GOOS == "windows" && left.PathHash != right.PathHash {
		t.Fatal("Windows path hash is case-sensitive")
	}
	if runtime.GOOS != "windows" && left.PathHash == right.PathHash {
		t.Fatal("POSIX path hash is case-insensitive")
	}

	journal, path := openTestJournal(t)
	fingerprint, _ := ReadFingerprint(path)
	_ = journal.Close()
	content, _ := os.ReadFile(path)
	mutations := []func([]byte){
		func(value []byte) { value[0] ^= 1 },
		func(value []byte) { binary.LittleEndian.PutUint32(value[8:12], 3) },
		func(value []byte) { binary.LittleEndian.PutUint32(value[12:16], 1) },
		func(value []byte) {
			binary.LittleEndian.PutUint64(value[16:24], math.MaxUint64)
			binary.LittleEndian.PutUint32(value[92:96], crc32.Checksum(value[:92], castagnoli))
		},
		func(value []byte) {
			value[88] = 1
			binary.LittleEndian.PutUint32(value[92:96], crc32.Checksum(value[:92], castagnoli))
		},
		func(value []byte) { value[24] ^= 1 },
	}
	for index, mutate := range mutations {
		value := append([]byte(nil), content...)
		mutate(value)
		if index != 4 && index != 0 && index != 1 && index != 2 && index != 3 {
			// Preserve the checksum for a structurally valid but stale fingerprint.
			binary.LittleEndian.PutUint32(value[92:96], crc32.Checksum(value[:92], castagnoli))
		}
		if index == 5 {
			if got, err := readFileHeader(bytes.NewReader(value)); err != nil || got == fingerprint {
				t.Fatalf("stale fingerprint = (%+v, %v)", got, err)
			}
		} else if _, err := readFileHeader(bytes.NewReader(value)); err == nil {
			t.Fatalf("mutation %d accepted", index)
		}
	}
	if _, err := ReadFingerprint(filepathForCaseTest("missing")); err == nil {
		t.Fatal("missing fingerprint accepted")
	}
	fallback := fingerprintFor("relative", 4, left.ContentHash, func(string) (string, error) { return "", errors.New("abs") })
	if fallback.BaseSize != 4 {
		t.Fatalf("fallback fingerprint = %+v", fallback)
	}
}

func filepathForCaseTest(name string) string {
	return string(os.PathSeparator) + "tmp" + string(os.PathSeparator) + name
}
