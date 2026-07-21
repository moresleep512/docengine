// Package recovery implements an append-only crash recovery journal.
package recovery

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
)

const (
	fileHeaderSize      = 96
	batchHeaderSize     = 64
	batchRecordSize     = 24
	maximumBatchSize    = 256
	maximumBatchPayload = 1 << 30
	journalVersion      = 2
)

var (
	fileMagic  = [8]byte{'D', 'O', 'C', 'L', 'O', 'G', '0', '2'}
	batchMagic = [8]byte{'D', 'O', 'C', 'J', 'N', 'L', '0', '2'}
	castagnoli = crc32.MakeTable(crc32.Castagnoli)

	ErrStaleJournal       = errors.New("recovery: journal belongs to another file version")
	ErrUnsupportedJournal = errors.New("recovery: unsupported journal header")
	ErrCorruptJournal     = errors.New("recovery: corrupt journal header")
	ErrClosed             = errors.New("recovery: journal closed")
	ErrInvalidBatch       = errors.New("recovery: invalid replacement batch")
)

// ReplaceOperation is one sequential byte-range replacement in an atomic batch.
type ReplaceOperation struct {
	Start        int64
	DeleteLength int64
	Inserted     []byte
}

// Operation describes a replayed replacement. Inserted bytes remain in the
// journal and can be read from PayloadOffset.
type Operation struct {
	Start         int64
	DeleteLength  int64
	InsertLength  int64
	PayloadOffset int64
}

// Batch is the only durable edit unit in journal v2.
type Batch struct {
	FirstRevision uint64
	Group         uint64
	Operations    []Operation
}

type BatchAppendResult struct {
	BatchOffset    int64
	PayloadOffsets []int64
}

// Fingerprint binds a journal to the complete bytes and normalized resolved
// path of its base file. Modification time is deliberately not part of it.
type Fingerprint struct {
	BaseSize    int64
	PathHash    [32]byte
	ContentHash [32]byte
}

type ReplayResult struct {
	Batches    []Batch
	ValidBytes int64
	Truncated  bool
}

type journalFile interface {
	io.ReaderAt
	io.Writer
	io.WriterAt
	io.Seeker
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(int64) error
	Close() error
}

type journalOpenOperations struct {
	mkdirAll func(string, os.FileMode) error
	openFile func(string, int, os.FileMode) (journalFile, error)
}

var systemJournalOpenOperations = journalOpenOperations{
	mkdirAll: os.MkdirAll,
	openFile: func(path string, flag int, permission os.FileMode) (journalFile, error) {
		return os.OpenFile(path, flag, permission)
	},
}

type Journal struct {
	mu   sync.Mutex
	file journalFile
	path string
}

// FingerprintFor constructs a strong identity for a fully scanned base file.
func FingerprintFor(path string, size int64, contentHash [32]byte) Fingerprint {
	return fingerprintFor(path, size, contentHash, filepath.Abs)
}

func fingerprintFor(path string, size int64, contentHash [32]byte, absolutePath func(string) (string, error)) Fingerprint {
	absolute, err := absolutePath(path)
	if err != nil {
		absolute = path
	}
	normalized := normalizeFingerprintPath(absolute)
	return Fingerprint{
		BaseSize:    size,
		PathHash:    sha256.Sum256([]byte(normalized)),
		ContentHash: contentHash,
	}
}

func Open(path string, fingerprint Fingerprint) (*Journal, ReplayResult, error) {
	return openJournal(path, fingerprint, systemJournalOpenOperations)
}

func openJournal(path string, fingerprint Fingerprint, operations journalOpenOperations) (*Journal, ReplayResult, error) {
	if fingerprint.BaseSize < 0 {
		return nil, ReplayResult{}, ErrStaleJournal
	}
	if err := operations.mkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ReplayResult{}, err
	}
	file, err := operations.openFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ReplayResult{}, err
	}
	journal := &Journal{file: file, path: path}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, ReplayResult{}, err
	}
	if info.Size() == 0 {
		if err := writeFileHeader(file, fingerprint); err != nil {
			_ = file.Close()
			return nil, ReplayResult{}, err
		}
		return journal, ReplayResult{ValidBytes: fileHeaderSize}, nil
	}
	stored, err := readFileHeader(file)
	if err != nil {
		_ = file.Close()
		return nil, ReplayResult{}, err
	}
	if stored != fingerprint {
		_ = file.Close()
		return nil, ReplayResult{}, ErrStaleJournal
	}
	replay, err := journal.Replay()
	if err != nil {
		_ = file.Close()
		return nil, ReplayResult{}, err
	}
	return journal, replay, nil
}

func (j *Journal) ReadAt(p []byte, off int64) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return 0, ErrClosed
	}
	return j.file.ReadAt(p, off)
}

func (j *Journal) Path() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.path
}

// ReadFingerprint inspects a journal without opening it for mutation.
func ReadFingerprint(path string) (Fingerprint, error) {
	file, err := os.Open(path)
	if err != nil {
		return Fingerprint{}, err
	}
	defer file.Close()
	return readFileHeader(file)
}

// AppendBatch stores one complete logical edit batch in one checksummed unit.
func (j *Journal) AppendBatch(firstRevision, group uint64, operations []ReplaceOperation) (BatchAppendResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return BatchAppendResult{}, ErrClosed
	}
	payload, relativeOffsets, err := encodeBatchPayload(firstRevision, group, operations)
	if err != nil {
		return BatchAppendResult{}, err
	}
	batch := Batch{FirstRevision: firstRevision, Group: group, Operations: make([]Operation, len(operations))}
	batchOffset, payloadOffset, err := j.appendBatch(batch, payload)
	if err != nil {
		return BatchAppendResult{}, err
	}
	result := BatchAppendResult{
		BatchOffset:    batchOffset,
		PayloadOffsets: make([]int64, len(relativeOffsets)),
	}
	for index, relative := range relativeOffsets {
		result.PayloadOffsets[index] = payloadOffset + relative
	}
	return result, nil
}

func (j *Journal) appendBatch(batch Batch, payload []byte) (int64, int64, error) {
	end, err := j.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, err
	}
	rollback := func(cause error) (int64, int64, error) {
		return 0, 0, errors.Join(cause, j.file.Truncate(end))
	}
	header := encodeBatchHeader(batch, payload)
	if n, writeErr := j.file.Write(header); writeErr != nil {
		return rollback(writeErr)
	} else if n != len(header) {
		return rollback(io.ErrShortWrite)
	}
	payloadOffset := end + batchHeaderSize
	if len(payload) > 0 {
		if n, writeErr := j.file.Write(payload); writeErr != nil {
			return rollback(writeErr)
		} else if n != len(payload) {
			return rollback(io.ErrShortWrite)
		}
	}
	return end, payloadOffset, nil
}

func (j *Journal) Replay() (ReplayResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return ReplayResult{}, ErrClosed
	}
	info, err := j.file.Stat()
	if err != nil {
		return ReplayResult{}, err
	}
	result := ReplayResult{ValidBytes: fileHeaderSize}
	for offset := int64(fileHeaderSize); offset < info.Size(); {
		if info.Size()-offset < batchHeaderSize {
			result.Truncated = true
			break
		}
		header := make([]byte, batchHeaderSize)
		if _, err := j.file.ReadAt(header, offset); err != nil {
			return result, err
		}
		batch, payloadLength, storedCRC, ok := decodeBatchHeader(header)
		if !ok || payloadLength > maximumBatchPayload {
			result.Truncated = true
			break
		}
		payloadOffset := offset + batchHeaderSize
		if payloadLength > info.Size()-payloadOffset {
			result.Truncated = true
			break
		}
		end := payloadOffset + payloadLength
		crc := crc32.Update(0, castagnoli, header[:56])
		buffer := make([]byte, 64<<10)
		remaining, cursor := payloadLength, payloadOffset
		for remaining > 0 {
			want := min(int64(len(buffer)), remaining)
			n, readErr := j.file.ReadAt(buffer[:int(want)], cursor)
			crc = crc32.Update(crc, castagnoli, buffer[:n])
			cursor += int64(n)
			remaining -= int64(n)
			if readErr != nil && !(errors.Is(readErr, io.EOF) && remaining == 0) {
				return result, readErr
			}
			if n == 0 && remaining > 0 {
				return result, io.ErrUnexpectedEOF
			}
		}
		if crc != storedCRC {
			result.Truncated = true
			break
		}
		operations, valid, decodeErr := decodeBatchOperations(j.file, batch, payloadOffset, payloadLength)
		if decodeErr != nil {
			return result, decodeErr
		}
		if !valid {
			result.Truncated = true
			break
		}
		batch.Operations = operations
		result.Batches = append(result.Batches, batch)
		result.ValidBytes = end
		offset = end
	}
	return result, nil
}

func (j *Journal) RepairTail(validBytes int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return ErrClosed
	}
	if validBytes < fileHeaderSize {
		return errors.New("recovery: invalid repair offset")
	}
	return j.file.Truncate(validBytes)
}

func (j *Journal) Sync() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return ErrClosed
	}
	return j.file.Sync()
}

func (j *Journal) Reset(fingerprint Fingerprint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return ErrClosed
	}
	if fingerprint.BaseSize < 0 {
		return ErrStaleJournal
	}
	if err := j.file.Truncate(0); err != nil {
		return err
	}
	if _, err := j.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return writeFileHeader(j.file, fingerprint)
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return nil
	}
	err := j.file.Close()
	j.file = nil
	return err
}

func writeFileHeader(file journalFile, fingerprint Fingerprint) error {
	header := make([]byte, fileHeaderSize)
	copy(header[:8], fileMagic[:])
	binary.LittleEndian.PutUint32(header[8:12], journalVersion)
	binary.LittleEndian.PutUint32(header[12:16], fileHeaderSize)
	binary.LittleEndian.PutUint64(header[16:24], uint64(fingerprint.BaseSize))
	copy(header[24:56], fingerprint.PathHash[:])
	copy(header[56:88], fingerprint.ContentHash[:])
	binary.LittleEndian.PutUint32(header[92:96], crc32.Checksum(header[:92], castagnoli))
	if _, err := file.WriteAt(header, 0); err != nil {
		return err
	}
	return file.Sync()
}

func readFileHeader(file io.ReaderAt) (Fingerprint, error) {
	header := make([]byte, fileHeaderSize)
	if _, err := file.ReadAt(header, 0); err != nil {
		return Fingerprint{}, err
	}
	if string(header[:8]) != string(fileMagic[:]) || binary.LittleEndian.Uint32(header[8:12]) != journalVersion || binary.LittleEndian.Uint32(header[12:16]) != fileHeaderSize {
		return Fingerprint{}, ErrUnsupportedJournal
	}
	if binary.LittleEndian.Uint32(header[88:92]) != 0 || binary.LittleEndian.Uint32(header[92:96]) != crc32.Checksum(header[:92], castagnoli) {
		return Fingerprint{}, ErrCorruptJournal
	}
	result := Fingerprint{BaseSize: int64(binary.LittleEndian.Uint64(header[16:24]))}
	if result.BaseSize < 0 {
		return Fingerprint{}, ErrCorruptJournal
	}
	copy(result.PathHash[:], header[24:56])
	copy(result.ContentHash[:], header[56:88])
	return result, nil
}

func encodeBatchHeader(batch Batch, payload []byte) []byte {
	header := make([]byte, batchHeaderSize)
	copy(header[:8], batchMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], journalVersion)
	binary.LittleEndian.PutUint32(header[12:16], batchHeaderSize)
	binary.LittleEndian.PutUint64(header[16:24], batch.FirstRevision)
	binary.LittleEndian.PutUint64(header[24:32], batch.Group)
	binary.LittleEndian.PutUint32(header[32:36], uint32(len(batch.Operations)))
	binary.LittleEndian.PutUint32(header[36:40], batchRecordSize)
	binary.LittleEndian.PutUint64(header[40:48], uint64(len(payload)))
	crc := crc32.Update(0, castagnoli, header[:56])
	crc = crc32.Update(crc, castagnoli, payload)
	binary.LittleEndian.PutUint32(header[56:60], crc)
	return header
}

func decodeBatchHeader(header []byte) (Batch, int64, uint32, bool) {
	if len(header) != batchHeaderSize || string(header[:8]) != string(batchMagic[:]) ||
		binary.LittleEndian.Uint16(header[8:10]) != journalVersion || binary.LittleEndian.Uint16(header[10:12]) != 0 ||
		binary.LittleEndian.Uint32(header[12:16]) != batchHeaderSize || binary.LittleEndian.Uint64(header[48:56]) != 0 ||
		binary.LittleEndian.Uint32(header[60:64]) != 0 {
		return Batch{}, 0, 0, false
	}
	count := binary.LittleEndian.Uint32(header[32:36])
	payloadRaw := binary.LittleEndian.Uint64(header[40:48])
	if count == 0 || count > maximumBatchSize || binary.LittleEndian.Uint32(header[36:40]) != batchRecordSize || payloadRaw > math.MaxInt64 {
		return Batch{}, 0, 0, false
	}
	batch := Batch{
		FirstRevision: binary.LittleEndian.Uint64(header[16:24]),
		Group:         binary.LittleEndian.Uint64(header[24:32]),
		Operations:    make([]Operation, int(count)),
	}
	if batch.FirstRevision == 0 || batch.Group == 0 || batch.FirstRevision > math.MaxUint64-uint64(count-1) {
		return Batch{}, 0, 0, false
	}
	return batch, int64(payloadRaw), binary.LittleEndian.Uint32(header[56:60]), true
}

func encodeBatchPayload(firstRevision, group uint64, operations []ReplaceOperation) ([]byte, []int64, error) {
	if len(operations) == 0 || len(operations) > maximumBatchSize || firstRevision == 0 || group == 0 || firstRevision > math.MaxUint64-uint64(len(operations)-1) {
		return nil, nil, ErrInvalidBatch
	}
	metadataLength := int64(len(operations) * batchRecordSize)
	total := metadataLength
	for _, operation := range operations {
		if operation.Start < 0 || operation.DeleteLength < 0 || int64(len(operation.Inserted)) > maximumBatchPayload-total {
			return nil, nil, ErrInvalidBatch
		}
		total += int64(len(operation.Inserted))
	}
	payload := make([]byte, int(total))
	relativeOffsets := make([]int64, len(operations))
	cursor := metadataLength
	for index, operation := range operations {
		record := payload[index*batchRecordSize : (index+1)*batchRecordSize]
		binary.LittleEndian.PutUint64(record[0:8], uint64(operation.Start))
		binary.LittleEndian.PutUint64(record[8:16], uint64(operation.DeleteLength))
		binary.LittleEndian.PutUint64(record[16:24], uint64(len(operation.Inserted)))
		relativeOffsets[index] = cursor
		copy(payload[int(cursor):], operation.Inserted)
		cursor += int64(len(operation.Inserted))
	}
	return payload, relativeOffsets, nil
}

func decodeBatchOperations(file io.ReaderAt, batch Batch, payloadOffset, payloadLength int64) ([]Operation, bool, error) {
	count := len(batch.Operations)
	metadataLength := int64(count * batchRecordSize)
	if metadataLength > payloadLength {
		return nil, false, nil
	}
	metadata := make([]byte, int(metadataLength))
	if _, err := file.ReadAt(metadata, payloadOffset); err != nil {
		return nil, false, err
	}
	payloadEnd := payloadOffset + payloadLength
	cursor := payloadOffset + metadataLength
	result := make([]Operation, 0, count)
	for index := 0; index < count; index++ {
		record := metadata[index*batchRecordSize : (index+1)*batchRecordSize]
		startRaw := binary.LittleEndian.Uint64(record[0:8])
		deleteRaw := binary.LittleEndian.Uint64(record[8:16])
		insertRaw := binary.LittleEndian.Uint64(record[16:24])
		if startRaw > math.MaxInt64 || deleteRaw > math.MaxInt64 || insertRaw > math.MaxInt64 {
			return nil, false, nil
		}
		start, deleteLength, insertLength := int64(startRaw), int64(deleteRaw), int64(insertRaw)
		if insertLength > payloadEnd-cursor {
			return nil, false, nil
		}
		result = append(result, Operation{Start: start, DeleteLength: deleteLength, InsertLength: insertLength, PayloadOffset: cursor})
		cursor += insertLength
	}
	if cursor != payloadEnd {
		return nil, false, nil
	}
	return result, true, nil
}
