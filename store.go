package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
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
// followed by append-only records of that kind, all integers big-endian:
//
//	kindHash   8-byte key hash (first 8 bytes of SHA-256), 8-byte unix time
//	kindExact  8-byte unix time, 4-byte key length, the key itself
//
// The unix time is when the key was first seen.
const (
	magic         = "DDUPSEEN"
	formatVersion = 1
	headerSize    = 16
	hashRecSize   = 16
	exactRecHead  = 12
	maxExactKey   = 16 << 20

	kindHash  byte = 1
	kindExact byte = 2
)

var (
	errNotState  = errors.New("not a dedupe-stream state file")
	errHashState = errors.New("this state stores hashes, so --exact cannot be used with it; " +
		"pick another --name, or --reset it first")
	errKeyTooLong = errors.New("key too long for an --exact state")
)

var be = binary.BigEndian

// Options control how a Store is opened.
type Options struct {
	// Exact stores keys verbatim instead of as 64-bit hashes. It only
	// matters when the file is created: an existing file keeps its kind.
	Exact bool
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

	kind    byte
	hashes  map[uint64]int64 // kindHash: key hash -> unix time first seen
	exact   map[string]int64 // kindExact: key -> unix time first seen
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
// A read-only store keeps its index instead: the keys it would have
// recorded exist only in memory, and forgetting them would let a line
// pass twice in one run.
func (s *Store) open() error {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	s.off, s.records = 0, 0
	if !s.opts.ReadOnly || (s.hashes == nil && s.exact == nil) {
		s.setKind(kindHash)
		if s.opts.Exact {
			s.setKind(kindExact)
		}
	}

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

// setKind switches the record kind and empties the in-memory index.
func (s *Store) setKind(kind byte) {
	s.kind = kind
	s.hashes, s.exact = nil, nil
	if kind == kindExact {
		s.exact = make(map[string]int64)
	} else {
		s.hashes = make(map[uint64]int64)
	}
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
			if _, err := s.f.Write(header(s.kind)); err != nil {
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
	var buf []byte
	for {
		n, err := s.readRecord(r, &buf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err == errCorrupt {
			return fmt.Errorf("corrupt record at offset %d", s.off)
		}
		if err != nil {
			return err
		}
		s.off += int64(n)
	}
	if s.off < size && !s.opts.ReadOnly {
		// Writers append whole records while holding the lock, so a partial
		// record seen under the lock was left by one that died mid-append
		// (kill -9, power loss, a full disk). Cut it off: appending after
		// it would misalign every record that follows. Read-only stores
		// just skip it, since it may be an append still in progress.
		if err := s.f.Truncate(s.off); err != nil {
			return fmt.Errorf("removing torn record: %w", err)
		}
	}
	return nil
}

var errCorrupt = errors.New("corrupt record")

// readRecord decodes one record from r into the index and returns its
// size. buf is scratch space for exact keys.
func (s *Store) readRecord(r *bufio.Reader, buf *[]byte) (int, error) {
	if s.kind == kindHash {
		var rec [hashRecSize]byte
		if _, err := io.ReadFull(r, rec[:]); err != nil {
			return 0, err
		}
		s.putHash(be.Uint64(rec[:8]), int64(be.Uint64(rec[8:])))
		return hashRecSize, nil
	}
	var head [exactRecHead]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, err
	}
	ts, l := int64(be.Uint64(head[:8])), int(be.Uint32(head[8:]))
	if l > maxExactKey {
		return 0, errCorrupt
	}
	if cap(*buf) < l {
		*buf = make([]byte, l)
	}
	key := (*buf)[:l]
	if _, err := io.ReadFull(r, key); err != nil {
		return 0, err
	}
	s.putExact(string(key), ts)
	return exactRecHead + l, nil
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
	switch kind := hdr[9]; {
	case kind == kindHash && s.opts.Exact:
		return errHashState
	case kind == kindHash || kind == kindExact:
		if kind != s.kind {
			s.setKind(kind)
		}
	default:
		return fmt.Errorf("unsupported record kind %d", kind)
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

func (s *Store) putHash(h uint64, ts int64) {
	s.records++
	if old, ok := s.hashes[h]; !ok || ts > old {
		s.hashes[h] = ts
	}
}

func (s *Store) putExact(key string, ts int64) {
	s.records++
	if old, ok := s.exact[key]; !ok || ts > old {
		s.exact[key] = ts
	}
}

// times yields the first-seen time of every known key.
func (s *Store) times() iter.Seq[int64] {
	if s.kind == kindExact {
		return maps.Values(s.exact)
	}
	return maps.Values(s.hashes)
}

// cutoff returns the unix time at or before which entries have expired.
func (s *Store) cutoff() int64 {
	if s.opts.TTL <= 0 {
		return math.MinInt64
	}
	return s.opts.Now().Add(-s.opts.TTL).Unix()
}

func (s *Store) has(key []byte) bool {
	var ts int64
	var ok bool
	if s.kind == kindExact {
		ts, ok = s.exact[string(key)]
	} else {
		ts, ok = s.hashes[hashKey(key)]
	}
	return ok && ts > s.cutoff()
}

// live counts the keys that have not expired.
func (s *Store) live() int {
	cutoff, n := s.cutoff(), 0
	for ts := range s.times() {
		if ts > cutoff {
			n++
		}
	}
	return n
}

// Len reports the number of distinct keys known, expired ones included.
func (s *Store) Len() int { return len(s.hashes) + len(s.exact) }

// Seen reports whether key has been seen before. If it has not, Seen calls
// onNew (when non-nil) and then records the key. The record is written only
// after onNew succeeds, so a crash or a closed pipe means the line is shown
// again next time rather than silently lost.
func (s *Store) Seen(key []byte, onNew func() error) (bool, error) {
	if s.has(key) {
		return true, nil
	}
	if s.kind == kindExact && len(key) > maxExactKey {
		return false, fmt.Errorf("%w (%d bytes, limit %d)", errKeyTooLong, len(key), maxExactKey)
	}
	seen := false
	err := s.locked(func() error {
		if err := s.refresh(); err != nil {
			return err
		}
		if s.has(key) {
			seen = true
			return nil
		}
		if onNew != nil {
			if err := onNew(); err != nil {
				return err
			}
		}
		return s.add(key, s.opts.Now().Unix())
	})
	return seen, err
}

func (s *Store) add(key []byte, ts int64) error {
	var h uint64
	if s.kind == kindHash {
		h = hashKey(key)
	}
	if !s.opts.ReadOnly {
		var rec []byte
		if s.kind == kindHash {
			rec = appendHashRecord(nil, h, ts)
		} else {
			rec = appendExactRecord(nil, key, ts)
		}
		if _, err := s.f.Write(rec); err != nil {
			return fmt.Errorf("recording key: %w", err)
		}
		s.off += int64(len(rec))
	}
	if s.kind == kindHash {
		s.putHash(h, ts)
	} else {
		s.putExact(string(key), ts)
	}
	return nil
}

func appendHashRecord(b []byte, h uint64, ts int64) []byte {
	b = be.AppendUint64(b, h)
	return be.AppendUint64(b, uint64(ts))
}

func appendExactRecord[K string | []byte](b []byte, key K, ts int64) []byte {
	b = be.AppendUint64(b, uint64(ts))
	b = be.AppendUint32(b, uint32(len(key)))
	return append(b, key...)
}

// compact rewrites the file with only the live entries, dropping expired
// and superseded records. It runs with the lock held and returns with the
// lock held on the new file.
func (s *Store) compact() error {
	type entry struct {
		ts  int64
		h   uint64
		key string
	}
	cutoff := s.cutoff()
	keep := make([]entry, 0, s.Len())
	for h, ts := range s.hashes {
		if ts > cutoff {
			keep = append(keep, entry{ts: ts, h: h})
		} else {
			delete(s.hashes, h)
		}
	}
	for key, ts := range s.exact {
		if ts > cutoff {
			keep = append(keep, entry{ts: ts, key: key})
		} else {
			delete(s.exact, key)
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
	w.Write(header(s.kind))
	size := int64(headerSize)
	var rec []byte
	for _, e := range keep {
		if s.kind == kindHash {
			rec = appendHashRecord(rec[:0], e.h, e.ts)
		} else {
			rec = appendExactRecord(rec[:0], e.key, e.ts)
		}
		w.Write(rec)
		size += int64(len(rec))
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
	s.off = size
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
	Exact   bool
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
	st.Exists, st.Exact, st.Size, st.Records = true, s.kind == kindExact, fi.Size(), s.records
	cutoff := s.cutoff()
	var oldest, newest int64
	for ts := range s.times() {
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
// a state file: a device or FIFO is not even opened, and a regular file
// (an empty one included) must start with the magic.
func Reset(path string) (bool, error) {
	for {
		fi, err := os.Stat(path)
		if err == nil && !fi.Mode().IsRegular() {
			return false, fmt.Errorf("%s: %w", path, errNotState)
		}
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
		// A short read leaves the rest of hdr zero, so it cannot match.
		var hdr [len(magic)]byte
		if _, err := f.ReadAt(hdr[:], 0); err != nil && err != io.EOF {
			f.Close()
			return false, err
		}
		if string(hdr[:]) != magic {
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
// million keys. Use --exact if even that is too much.
func hashKey(key []byte) uint64 {
	sum := sha256.Sum256(key)
	return be.Uint64(sum[:8])
}
