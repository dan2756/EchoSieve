package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
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
	// only, a missing file is treated as empty, and no lock is taken.
	ReadOnly bool
	// TTL forgets keys first seen longer ago than this; zero keeps them
	// forever. A key seen again after it expired is recorded afresh.
	TTL time.Duration
	Now func() time.Time
}

// Store is the set of keys seen so far, backed by an append-only file.
//
// Several processes may share one file. Keys already in memory are answered
// without touching the file. For any other key a writer takes an exclusive
// flock, reads whatever other processes appended since it last looked, and
// only then decides the key is new and appends it, so two processes never
// both pass the same line. Compaction and --reset replace or remove the
// file; whoever takes the lock checks that its descriptor still refers to
// the file at path, and if not starts over on the current file.
type Store struct {
	path string
	opts Options

	f   *os.File // nil for a read-only store whose file does not exist
	off int64    // bytes of f read so far; 0 means the header is unread

	hashes  map[uint64]int64 // key hash -> unix time first seen
	records int              // records read or written, superseded ones included
}

// OpenStore opens the state file at path, creating it unless opts.ReadOnly
// is set, and loads every recorded key into memory. With a TTL, a writable
// store first compacts the file if it holds any expired records.
func OpenStore(path string, opts Options) (*Store, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Store{path: path, opts: opts}
	if !opts.ReadOnly {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
	}
	if err := s.open(); err != nil {
		return nil, err
	}
	err := s.locked(func() error {
		if err := s.refresh(); err != nil {
			return err
		}
		if !opts.ReadOnly && opts.TTL > 0 && s.records > s.live() {
			return s.compact()
		}
		return nil
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// open (re)opens the file at s.path, forgetting everything read so far.
func (s *Store) open() error {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	s.off, s.records = 0, 0
	s.hashes = make(map[uint64]int64)

	var err error
	if s.opts.ReadOnly {
		s.f, err = os.Open(s.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
	} else {
		s.f, err = os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	}
	return err
}

// locked runs fn holding the write lock on a descriptor that refers to the
// current file at s.path. Read-only stores take no lock, but still follow
// the file if it was replaced.
func (s *Store) locked(fn func() error) error {
	if s.opts.ReadOnly {
		if !sameFile(s.f, s.path) {
			if err := s.open(); err != nil {
				return err
			}
		}
		return fn()
	}
	for {
		if err := lockFile(s.f); err != nil {
			return fmt.Errorf("locking state: %w", err)
		}
		if sameFile(s.f, s.path) {
			break
		}
		// Compacted or reset by another process while we waited.
		// Closing the old descriptor drops its lock.
		if err := s.open(); err != nil {
			return err
		}
	}
	// fn may swap s.f (compaction), so unlock whatever is current.
	defer func() { unlockFile(s.f) }()
	return fn()
}

// sameFile reports whether f (which may be nil) is the file now at path.
func sameFile(f *os.File, path string) bool {
	cur, err := os.Stat(path)
	if f == nil {
		return err != nil
	}
	if err != nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && os.SameFile(fi, cur)
}

// refresh reads any records appended since the last call. Writers call it
// with the lock held.
func (s *Store) refresh() error {
	if s.f == nil {
		return nil
	}
	fi, err := s.f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	if s.off == 0 {
		switch {
		case size == 0 && s.opts.ReadOnly:
			return nil
		case size == 0:
			if _, err := s.f.Write(header(kindHash)); err != nil {
				return err
			}
			s.off = headerSize
			return nil
		case size < headerSize:
			return errNotState
		}
		if err := s.readHeader(); err != nil {
			return err
		}
		s.off = headerSize
	}
	if size <= s.off {
		return nil
	}

	r := bufio.NewReaderSize(io.NewSectionReader(s.f, s.off, size-s.off), 64<<10)
	var rec [hashRecSize]byte
	for {
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil // a partial record is completed on a later read
			}
			return err
		}
		s.put(be.Uint64(rec[:8]), int64(be.Uint64(rec[8:])))
		s.off += hashRecSize
	}
}

func (s *Store) readHeader() error {
	var hdr [headerSize]byte
	if _, err := s.f.ReadAt(hdr[:], 0); err != nil {
		return err
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
	return nil
}

func header(kind byte) []byte {
	hdr := make([]byte, headerSize)
	copy(hdr, magic)
	hdr[8] = formatVersion
	hdr[9] = kind
	return hdr
}

func (s *Store) put(h uint64, ts int64) {
	s.records++
	if old, ok := s.hashes[h]; !ok || ts > old {
		s.hashes[h] = ts
	}
}

// cutoff returns the unix time at or before which entries have expired.
func (s *Store) cutoff() int64 {
	if s.opts.TTL <= 0 {
		return math.MinInt64
	}
	return s.opts.Now().Add(-s.opts.TTL).Unix()
}

func (s *Store) has(h uint64) bool {
	ts, ok := s.hashes[h]
	return ok && ts > s.cutoff()
}

// live counts the keys that have not expired.
func (s *Store) live() int {
	cutoff, n := s.cutoff(), 0
	for _, ts := range s.hashes {
		if ts > cutoff {
			n++
		}
	}
	return n
}

// Len reports the number of distinct keys known, expired ones included.
func (s *Store) Len() int { return len(s.hashes) }

// Seen reports whether key has been seen before. If it has not, Seen calls
// onNew (when non-nil) and then records the key. The record is written only
// after onNew succeeds, so a crash or a closed pipe means the line is shown
// again next time rather than silently lost.
func (s *Store) Seen(key []byte, onNew func() error) (bool, error) {
	h := hashKey(key)
	if s.has(h) {
		return true, nil
	}
	seen := false
	err := s.locked(func() error {
		if err := s.refresh(); err != nil {
			return err
		}
		if s.has(h) {
			seen = true
			return nil
		}
		if onNew != nil {
			if err := onNew(); err != nil {
				return err
			}
		}
		return s.add(h, s.opts.Now().Unix())
	})
	return seen, err
}

func (s *Store) add(h uint64, ts int64) error {
	if !s.opts.ReadOnly {
		var rec [hashRecSize]byte
		be.PutUint64(rec[:8], h)
		be.PutUint64(rec[8:], uint64(ts))
		if _, err := s.f.Write(rec[:]); err != nil {
			return fmt.Errorf("recording key: %w", err)
		}
		s.off += hashRecSize
	}
	s.put(h, ts)
	return nil
}

// compact rewrites the file with only the live entries, dropping expired
// and superseded records. It runs with the lock held and returns with the
// lock held on the new file.
func (s *Store) compact() error {
	type entry struct {
		h  uint64
		ts int64
	}
	cutoff := s.cutoff()
	keep := make([]entry, 0, len(s.hashes))
	for h, ts := range s.hashes {
		if ts > cutoff {
			keep = append(keep, entry{h, ts})
		} else {
			delete(s.hashes, h)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].ts < keep[j].ts })

	tmpPath := s.path + ".compact"
	tmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()
	// Lock the new file before it appears at s.path, so no other writer
	// can get in between the rename and our next append.
	if err := lockFile(tmp); err != nil {
		return err
	}
	w := bufio.NewWriterSize(tmp, 64<<10)
	w.Write(header(kindHash))
	var rec [hashRecSize]byte
	for _, e := range keep {
		be.PutUint64(rec[:8], e.h)
		be.PutUint64(rec[8:], uint64(e.ts))
		w.Write(rec[:])
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	syncDir(filepath.Dir(s.path))

	s.f.Close() // drops the old lock; waiters see the file was replaced
	s.f = tmp
	s.off = headerSize + int64(len(keep))*hashRecSize
	s.records = len(keep)
	done = true
	return nil
}

// syncDir makes a rename durable. Not every platform supports syncing a
// directory, so errors are ignored.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Stats describes a state file.
type Stats struct {
	Exists  bool
	Size    int64
	Records int // records in the file, superseded ones included
	Entries int // distinct live keys
	Expired int // distinct keys past the TTL
	Oldest  time.Time
	Newest  time.Time
}

// Stats summarizes the store as last read from disk.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	if s.f == nil {
		return st, nil
	}
	fi, err := s.f.Stat()
	if err != nil {
		return st, err
	}
	st.Exists, st.Size, st.Records = true, fi.Size(), s.records
	cutoff := s.cutoff()
	var oldest, newest int64
	for _, ts := range s.hashes {
		if ts <= cutoff {
			st.Expired++
			continue
		}
		if st.Entries == 0 || ts < oldest {
			oldest = ts
		}
		if st.Entries == 0 || ts > newest {
			newest = ts
		}
		st.Entries++
	}
	if st.Entries > 0 {
		st.Oldest, st.Newest = time.Unix(oldest, 0), time.Unix(newest, 0)
	}
	return st, nil
}

// Close closes the state file.
func (s *Store) Close() error {
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

// Reset deletes the state file at path, reporting whether there was one.
// It waits for the lock so it never removes the file in the middle of
// another process's append, and it refuses to delete anything that is not
// a state file.
func Reset(path string) (bool, error) {
	for {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := lockFile(f); err != nil {
			f.Close()
			return false, err
		}
		if !sameFile(f, path) {
			f.Close()
			continue
		}
		var hdr [len(magic)]byte
		n, err := f.ReadAt(hdr[:], 0)
		if err != nil && err != io.EOF {
			f.Close()
			return false, err
		}
		if string(hdr[:n]) != magic[:n] {
			f.Close()
			return false, fmt.Errorf("%s: %w", path, errNotState)
		}
		err = os.Remove(path)
		os.Remove(path + ".compact")
		f.Close()
		return err == nil, err
	}
}

// hashKey returns the first 8 bytes of SHA-256(key). With n keys stored, the
// chance of any two colliding is roughly n²/2⁶⁵: about 1 in 37 million at a
// million keys.
func hashKey(key []byte) uint64 {
	sum := sha256.Sum256(key)
	return be.Uint64(sum[:8])
}
