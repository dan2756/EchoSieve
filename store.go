package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// A state file starts with a 16-byte header:
//
//	0..8   magic "DDUPSEEN"
//	8      format version
//	9      record kind
//	10..16 reserved, zero
//
// followed by append-only records. A hash record is a fixed 16 bytes: the
// first 8 bytes of SHA-256(key) and the unix time the key was first seen,
// both big-endian.
const (
	magic         = "DDUPSEEN"
	formatVersion = 1
	headerSize    = 16
	hashRecSize   = 16

	kindHash byte = 1
)

var errNotState = errors.New("not a dedupe-stream state file")

var be = binary.BigEndian

// Options control how a Store is opened.
type Options struct {
	// ReadOnly never touches the file: new keys are remembered in memory
	// only, and a missing file is treated as empty rather than created.
	ReadOnly bool
	Now      func() time.Time
}

// Store is the set of keys seen so far, backed by an append-only file.
type Store struct {
	opts   Options
	f      *os.File         // nil for a read-only store with no file yet
	hashes map[uint64]int64 // key hash -> unix time first seen
}

// OpenStore opens the state file at path, creating it unless opts.ReadOnly
// is set, and loads every recorded key into memory.
func OpenStore(path string, opts Options) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Store{opts: opts, hashes: make(map[uint64]int64)}
	var err error
	if opts.ReadOnly {
		s.f, err = os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		s.f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	}
	if err != nil {
		return nil, err
	}
	if err := s.load(); err != nil {
		s.f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) load() error {
	r := bufio.NewReaderSize(s.f, 64<<10)
	var hdr [headerSize]byte
	n, err := io.ReadFull(r, hdr[:])
	switch {
	case n == 0 && err == io.EOF:
		if s.opts.ReadOnly {
			return nil
		}
		return s.writeHeader()
	case err != nil:
		return errNotState
	}
	if string(hdr[:len(magic)]) != magic {
		return errNotState
	}
	if hdr[8] != formatVersion {
		return fmt.Errorf("unsupported state format version %d", hdr[8])
	}
	if hdr[9] != kindHash {
		return fmt.Errorf("unsupported record kind %d", hdr[9])
	}

	var rec [hashRecSize]byte
	for {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		s.put(be.Uint64(rec[:8]), int64(be.Uint64(rec[8:])))
	}
}

func (s *Store) writeHeader() error {
	var hdr [headerSize]byte
	copy(hdr[:], magic)
	hdr[8] = formatVersion
	hdr[9] = kindHash
	_, err := s.f.Write(hdr[:])
	return err
}

func (s *Store) put(h uint64, ts int64) {
	if old, ok := s.hashes[h]; !ok || ts > old {
		s.hashes[h] = ts
	}
}

// Len reports the number of distinct keys known.
func (s *Store) Len() int { return len(s.hashes) }

// Seen reports whether key has been seen before. If it has not, Seen calls
// onNew (when non-nil) and then records the key. The record is written only
// after onNew succeeds, so a crash or a closed pipe means the line is shown
// again next time rather than silently lost.
func (s *Store) Seen(key []byte, onNew func() error) (bool, error) {
	h := hashKey(key)
	if _, ok := s.hashes[h]; ok {
		return true, nil
	}
	if onNew != nil {
		if err := onNew(); err != nil {
			return false, err
		}
	}
	ts := s.opts.Now().Unix()
	if !s.opts.ReadOnly {
		var rec [hashRecSize]byte
		be.PutUint64(rec[:8], h)
		be.PutUint64(rec[8:], uint64(ts))
		if _, err := s.f.Write(rec[:]); err != nil {
			return false, fmt.Errorf("recording key: %w", err)
		}
	}
	s.put(h, ts)
	return false, nil
}

// Close closes the state file.
func (s *Store) Close() error {
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

// hashKey returns the first 8 bytes of SHA-256(key). With n keys stored, the
// chance of any two colliding is roughly n²/2⁶⁵: about 1 in 37 million at a
// million keys.
func hashKey(key []byte) uint64 {
	sum := sha256.Sum256(key)
	return be.Uint64(sum[:8])
}
