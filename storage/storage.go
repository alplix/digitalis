// Package storage manages piece state and on-disk storage for a torrent.
package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/alplix/digitalis/metainfo"
)

// PieceState tracks whether a piece is present/verified and in-flight.
type PieceState struct {
	Present   bool
	Verifying bool
	InFlight  int // number of outstanding requests
	Pending   int32 // used by picker
}

// Storage manages in-memory piece state and file layout on disk.
type Storage struct {
	meta    *metainfo.MetaInfo
	dir     string // root directory for files
	pieces  []PieceState
	mu      sync.Mutex
	files   []*fileHandle
}

type fileHandle struct {
	path   string
	length int64
	f      *os.File
}

// New creates storage for a torrent.
func New(meta *metainfo.MetaInfo, saveDir string) (*Storage, error) {
	s := &Storage{
		meta: meta,
		dir:  saveDir,
		pieces: make([]PieceState, meta.PieceCount()),
	}

	if meta.IsSingleFile() {
		path := filepath.Join(saveDir, meta.Info.Name)
		if err := ensureFile(path, meta.Info.Length); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0644)
		if err != nil {
			return nil, err
		}
		s.files = append(s.files, &fileHandle{path: path, length: meta.Info.Length, f: f})
	} else {
		root := filepath.Join(saveDir, meta.Info.Name)
		for _, file := range meta.Info.Files {
			rel := filepath.Join(file.Path...)
			path := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return nil, err
			}
			if err := ensureFile(path, file.Length); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(path, os.O_RDWR, 0644)
			if err != nil {
				return nil, err
			}
			s.files = append(s.files, &fileHandle{path: path, length: file.Length, f: f})
		}
	}

	// Detect existing pieces (if files were partially downloaded before).
	s.scanExistingPieces()
	return s, nil
}

func ensureFile(path string, length int64) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		if length > 0 {
			if err := f.Truncate(length); err != nil {
				f.Close()
				return err
			}
		}
		f.Sync()
		f.Close()
		return nil
	}
	return nil
}

// scanExistingPieces reads existing data and verifies pieces.
func (s *Storage) scanExistingPieces() {
	total := s.meta.TotalLength()
	if total <= 0 {
		return
	}
	for i := 0; i < len(s.pieces); i++ {
		ok := s.verifyPiece(i)
		if ok {
			s.pieces[i].Present = true
		}
	}
}

// PieceCount returns number of pieces.
func (s *Storage) PieceCount() int { return len(s.pieces) }

// PiecePresent reports whether piece i is present & verified.
func (s *Storage) PiecePresent(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.pieces) {
		return false
	}
	return s.pieces[i].Present
}

// Bitfield returns the current bitfield (present pieces).
func (s *Storage) Bitfield() []byte {
	count := s.PieceCount()
	bf := make([]byte, (count+7)/8)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < count; i++ {
		if s.pieces[i].Present {
			bf[i/8] |= 1 << (7 - uint(i%8))
		}
	}
	return bf
}

// SetPresent marks a piece as present/verified.
func (s *Storage) SetPresent(i int, b bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= 0 && i < len(s.pieces) {
		s.pieces[i].Present = b
	}
}

// MarkInFlight increments in-flight counter for a piece.
func (s *Storage) MarkInFlight(i int, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= 0 && i < len(s.pieces) {
		s.pieces[i].InFlight += delta
	}
}

// InFlight returns number of outstanding blocks for piece i.
func (s *Storage) InFlight(i int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= 0 && i < len(s.pieces) {
		return s.pieces[i].InFlight
	}
	return 0
}

// PieceLength returns the length of piece i (may be short for last piece).
func (s *Storage) PieceLength(i int) int64 {
	pieceLength := s.meta.Info.PieceLength
	total := s.meta.TotalLength()
	start := int64(i) * pieceLength
	remain := total - start
	if remain < pieceLength {
		return remain
	}
	return pieceLength
}

// ReadPiece reads the full contents of piece i.
func (s *Storage) ReadPiece(i int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	length := s.PieceLength(i)
	if length == 0 {
		return nil, fmt.Errorf("piece %d has zero length", i)
	}
	start := int64(i) * s.meta.Info.PieceLength
	data := make([]byte, length)
	if err := s.readAt(data, start); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadBlock reads a block (sub-piece) of data.
func (s *Storage) ReadBlock(piece int, begin int64, length int) ([]byte, error) {
	start := int64(piece)*s.meta.Info.PieceLength + begin
	data := make([]byte, length)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readAt(data, start); err != nil {
		return nil, err
	}
	return data, nil
}

// WriteBlock writes a block of data for a piece.
func (s *Storage) WriteBlock(piece int, begin int64, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := int64(piece)*s.meta.Info.PieceLength + begin
	return s.writeAt(data, start)
}

// verifyPiece verifies the SHA-1 hash of piece i against the stored data.
func (s *Storage) verifyPiece(i int) bool {
	data, err := s.ReadPiece(i)
	if err != nil {
		return false
	}
	if len(data) == 0 {
		return false
	}
	if len(data) != int(s.PieceLength(i)) {
		return false
	}
	sum := sha1Sum(data)
	expected := s.meta.PieceHash(i)
	return byteEqual(sum, expected)
}

// VerifyPiece checks piece i and marks it present accordingly.
// Returns true if the piece is valid.
func (s *Storage) VerifyPiece(i int) bool {
	ok := s.verifyPiece(i)
	s.SetPresent(i, ok)
	return ok
}

func byteEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readAt reads at a global file offset, spanning multiple files if needed.
func (s *Storage) readAt(data []byte, offset int64) error {
	remaining := int64(len(data))
	cur := offset
	for _, fh := range s.files {
		if remaining <= 0 {
			break
		}
		if cur >= fh.length {
			cur -= fh.length
			continue
		}
		n := remaining
		if n > fh.length-cur {
			n = fh.length - cur
		}
		if _, err := fh.f.ReadAt(data[:n], cur); err != nil {
			return err
		}
		data = data[n:]
		remaining -= n
		cur = 0
	}
	if remaining > 0 {
		return fmt.Errorf("data extends beyond files (missing %d bytes)", remaining)
	}
	return nil
}

// writeAt writes at a global file offset, spanning multiple files.
func (s *Storage) writeAt(data []byte, offset int64) error {
	cur := offset
	for _, fh := range s.files {
		if len(data) == 0 {
			break
		}
		if cur >= fh.length {
			cur -= fh.length
			continue
		}
		n := int64(len(data))
		if n > fh.length-cur {
			n = fh.length - cur
		}
		if _, err := fh.f.WriteAt(data[:n], cur); err != nil {
			return err
		}
		data = data[n:]
		cur = 0
	}
	if len(data) > 0 {
		return fmt.Errorf("write extends beyond files")
	}
	return nil
}

// Move relocates all managed files to a new root directory.
// The destination is expected to be on the same filesystem; open file
// descriptors stay valid across renames on POSIX systems.
func (s *Storage) Move(newDir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if newDir == "" {
		return fmt.Errorf("empty destination dir")
	}
	if filepath.Clean(newDir) == filepath.Clean(s.dir) {
		s.rebuildPaths(newDir)
		return nil
	}
	if s.meta.IsSingleFile() {
		newPath := filepath.Join(newDir, s.meta.Info.Name)
		if err := os.MkdirAll(newDir, 0755); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(s.dir, s.meta.Info.Name)); err == nil {
			if err := os.Rename(filepath.Join(s.dir, s.meta.Info.Name), newPath); err != nil {
				return err
			}
		}
	} else {
		oldRoot := filepath.Join(s.dir, s.meta.Info.Name)
		newRoot := filepath.Join(newDir, s.meta.Info.Name)
		if err := os.MkdirAll(filepath.Dir(newRoot), 0755); err != nil {
			return err
		}
		if _, err := os.Stat(oldRoot); err == nil {
			if err := os.Rename(oldRoot, newRoot); err != nil {
				return err
			}
		}
	}
	s.dir = newDir
	s.rebuildPaths(newDir)
	return nil
}

// rebuildPaths recomputes the on-disk path for every open file handle.
func (s *Storage) rebuildPaths(newDir string) {
	if s.meta.IsSingleFile() {
		p := filepath.Join(newDir, s.meta.Info.Name)
		if len(s.files) == 1 {
			s.files[0].path = p
		}
		return
	}
	root := filepath.Join(newDir, s.meta.Info.Name)
	for i, f := range s.meta.Info.Files {
		if i < len(s.files) {
			s.files[i].path = filepath.Join(root, filepath.Join(f.Path...))
		}
	}
}

// Close closes all file handles.
func (s *Storage) Close() error {
	for _, fh := range s.files {
		if fh.f != nil {
			fh.f.Close()
		}
	}
	return nil
}

// TotalLength returns the total byte length.
func (s *Storage) TotalLength() int64 { return s.meta.TotalLength() }
