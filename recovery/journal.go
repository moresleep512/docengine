// Package recovery implements an append-only crash recovery journal.
package recovery

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	fileHeaderSize  = 72
	frameHeaderSize = 64
	journalVersion  = 1
)

var (
	fileMagic  = [8]byte{'D', 'O', 'C', 'L', 'O', 'G', '0', '1'}
	frameMagic = [8]byte{'D', 'O', 'C', 'J', 'N', 'L', '0', '1'}
	castagnoli = crc32.MakeTable(crc32.Castagnoli)

	ErrStaleJournal = errors.New("recovery: journal belongs to another file version")
	ErrClosed       = errors.New("recovery: journal closed")
)

type FrameKind uint16

const (
	FrameReplace FrameKind = iota + 1
	FrameRoot
)

type Fingerprint struct {
	BaseSize     int64
	ModTimeNanos int64
	PathHash     [32]byte
}

type Frame struct {
	Kind           FrameKind
	Revision       uint64
	TargetRevision uint64
	Start          int64
	DeleteLength   int64
	InsertLength   int64
	PayloadOffset  int64
}

type ReplayResult struct {
	Frames     []Frame
	ValidBytes int64
	Truncated  bool
}

type Journal struct {
	mu   sync.Mutex
	file *os.File
	path string
}

func FingerprintFor(path string, info os.FileInfo) Fingerprint {
	absolute, _ := filepath.Abs(path)
	normalized := strings.ToLower(filepath.Clean(absolute))
	return Fingerprint{
		BaseSize:     info.Size(),
		ModTimeNanos: info.ModTime().UnixNano(),
		PathHash:     sha256.Sum256([]byte(normalized)),
	}
}

func Open(path string, fingerprint Fingerprint) (*Journal, ReplayResult, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, ReplayResult{}, err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
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

func (j *Journal) AppendReplace(revision uint64, start, deleteLength int64, inserted []byte) (int64, error) {
	return j.AppendReplaceGroup(revision, start, deleteLength, inserted, 0)
}

// AppendReplaceGroup records the origin of an undo group. A zero marker keeps
// compatibility with v1 journals that treated every replace as one history item.
// Non-zero values are encoded as origin revision + 1 so origin zero is representable.
func (j *Journal) AppendReplaceGroup(revision uint64, start, deleteLength int64, inserted []byte, groupMarker uint64) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return 0, ErrClosed
	}
	return j.appendFrame(Frame{
		Kind:           FrameReplace,
		Revision:       revision,
		TargetRevision: groupMarker,
		Start:          start,
		DeleteLength:   deleteLength,
		InsertLength:   int64(len(inserted)),
	}, inserted)
}

func (j *Journal) AppendRoot(revision, targetRevision uint64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return ErrClosed
	}
	_, err := j.appendFrame(Frame{Kind: FrameRoot, Revision: revision, TargetRevision: targetRevision}, nil)
	return err
}

func (j *Journal) appendFrame(frame Frame, payload []byte) (int64, error) {
	end, err := j.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	header := encodeFrameHeader(frame, payload)
	if _, err := j.file.Write(header); err != nil {
		return 0, err
	}
	payloadOffset := end + frameHeaderSize
	if len(payload) > 0 {
		if _, err := j.file.Write(payload); err != nil {
			return 0, err
		}
	}
	return payloadOffset, nil
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
		if info.Size()-offset < frameHeaderSize {
			result.Truncated = true
			break
		}
		header := make([]byte, frameHeaderSize)
		if _, err := j.file.ReadAt(header, offset); err != nil {
			return result, err
		}
		frame, storedCRC, ok := decodeFrameHeader(header)
		if !ok || frame.InsertLength < 0 || frame.InsertLength > 1<<30 {
			result.Truncated = true
			break
		}
		frame.PayloadOffset = offset + frameHeaderSize
		end := frame.PayloadOffset + frame.InsertLength
		if end > info.Size() {
			result.Truncated = true
			break
		}
		crc := crc32.Update(0, castagnoli, header[:56])
		buffer := make([]byte, 64<<10)
		remaining := frame.InsertLength
		cursor := frame.PayloadOffset
		for remaining > 0 {
			want := int64(len(buffer))
			if want > remaining {
				want = remaining
			}
			n, readErr := j.file.ReadAt(buffer[:want], cursor)
			crc = crc32.Update(crc, castagnoli, buffer[:n])
			cursor += int64(n)
			remaining -= int64(n)
			if readErr != nil && !(errors.Is(readErr, io.EOF) && remaining == 0) {
				return result, readErr
			}
		}
		if crc != storedCRC {
			result.Truncated = true
			break
		}
		result.Frames = append(result.Frames, frame)
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

func writeFileHeader(file *os.File, fingerprint Fingerprint) error {
	header := make([]byte, fileHeaderSize)
	copy(header[:8], fileMagic[:])
	binary.LittleEndian.PutUint32(header[8:12], journalVersion)
	binary.LittleEndian.PutUint64(header[16:24], uint64(fingerprint.BaseSize))
	binary.LittleEndian.PutUint64(header[24:32], uint64(fingerprint.ModTimeNanos))
	copy(header[32:64], fingerprint.PathHash[:])
	binary.LittleEndian.PutUint32(header[64:68], crc32.Checksum(header[:64], castagnoli))
	if _, err := file.WriteAt(header, 0); err != nil {
		return err
	}
	return file.Sync()
}

func readFileHeader(file *os.File) (Fingerprint, error) {
	header := make([]byte, fileHeaderSize)
	if _, err := file.ReadAt(header, 0); err != nil {
		return Fingerprint{}, err
	}
	if string(header[:8]) != string(fileMagic[:]) || binary.LittleEndian.Uint32(header[8:12]) != journalVersion {
		return Fingerprint{}, errors.New("recovery: unsupported journal header")
	}
	if binary.LittleEndian.Uint32(header[64:68]) != crc32.Checksum(header[:64], castagnoli) {
		return Fingerprint{}, errors.New("recovery: corrupt journal header")
	}
	result := Fingerprint{
		BaseSize:     int64(binary.LittleEndian.Uint64(header[16:24])),
		ModTimeNanos: int64(binary.LittleEndian.Uint64(header[24:32])),
	}
	copy(result.PathHash[:], header[32:64])
	return result, nil
}

func encodeFrameHeader(frame Frame, payload []byte) []byte {
	header := make([]byte, frameHeaderSize)
	copy(header[:8], frameMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], journalVersion)
	binary.LittleEndian.PutUint16(header[10:12], uint16(frame.Kind))
	binary.LittleEndian.PutUint32(header[12:16], frameHeaderSize)
	binary.LittleEndian.PutUint64(header[16:24], frame.Revision)
	binary.LittleEndian.PutUint64(header[24:32], frame.TargetRevision)
	binary.LittleEndian.PutUint64(header[32:40], uint64(frame.Start))
	binary.LittleEndian.PutUint64(header[40:48], uint64(frame.DeleteLength))
	binary.LittleEndian.PutUint64(header[48:56], uint64(len(payload)))
	crc := crc32.Update(0, castagnoli, header[:56])
	crc = crc32.Update(crc, castagnoli, payload)
	binary.LittleEndian.PutUint32(header[56:60], crc)
	return header
}

func decodeFrameHeader(header []byte) (Frame, uint32, bool) {
	if len(header) != frameHeaderSize || string(header[:8]) != string(frameMagic[:]) {
		return Frame{}, 0, false
	}
	if binary.LittleEndian.Uint16(header[8:10]) != journalVersion || binary.LittleEndian.Uint32(header[12:16]) != frameHeaderSize {
		return Frame{}, 0, false
	}
	frame := Frame{
		Kind:           FrameKind(binary.LittleEndian.Uint16(header[10:12])),
		Revision:       binary.LittleEndian.Uint64(header[16:24]),
		TargetRevision: binary.LittleEndian.Uint64(header[24:32]),
		Start:          int64(binary.LittleEndian.Uint64(header[32:40])),
		DeleteLength:   int64(binary.LittleEndian.Uint64(header[40:48])),
		InsertLength:   int64(binary.LittleEndian.Uint64(header[48:56])),
	}
	if frame.Kind != FrameReplace && frame.Kind != FrameRoot {
		return Frame{}, 0, false
	}
	return frame, binary.LittleEndian.Uint32(header[56:60]), true
}
