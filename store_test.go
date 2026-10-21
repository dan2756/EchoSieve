package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// clock is a settable time source shared by stores in a test.
type clock struct{ t time.Time }

func (c *clock) now() time.Time                 { return c.t }
func (c *clock) advance(d time.Duration)        { c.t = c.t.Add(d) }
func (c *clock) opts(ttl time.Duration) Options { return Options{TTL: ttl, Now: c.now} }

func openWith(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	s, err := OpenStore(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path, Options{Now: fixedClock(testNow)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seen(t *testing.T, s *Store, key string) bool {
	t.Helper()
	ok, err := s.Seen([]byte(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestStoreReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := mustOpen(t, path)
	for _, k := range []string{"alpha", "beta", "alpha"} {
		seen(t, s, k)
	}
	if s.Len() != 2 {
		t.Fatalf("Len = %d, want 2", s.Len())
	}
	s.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(headerSize + 2*hashRecSize); fi.Size() != want {
		t.Errorf("file size = %d, want %d", fi.Size(), want)
	}

	s = mustOpen(t, path)
	if !seen(t, s, "alpha") || !seen(t, s, "beta") {
		t.Error("keys lost across reopen")
	}
	if seen(t, s, "gamma") {
		t.Error("unexpected key gamma")
	}
}

func TestStoreOnNewFailureDoesNotRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := mustOpen(t, path)
	boom := errors.New("broken pipe")
	if _, err := s.Seen([]byte("x"), func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if seen(t, s, "x") {
		t.Error("key was recorded although printing it failed")
	}
}

func TestStoreRejectsForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("2025-09-06 an ordinary log line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path, Options{}); !errors.Is(err, errNotState) {
		t.Fatalf("err = %v, want %v", err, errNotState)
	}
}

func TestTTLCompactsOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	s := openWith(t, path, c.opts(0))
	seen(t, s, "old-1")
	seen(t, s, "old-2")
	c.advance(5 * day)
	seen(t, s, "recent")
	s.Close()
	if got, want := fileSize(t, path), int64(headerSize+3*hashRecSize); got != want {
		t.Fatalf("size before compaction = %d, want %d", got, want)
	}

	// Nothing has expired yet: the file must be left alone.
	c.advance(1 * day)
	before, _ := os.Stat(path)
	s = openWith(t, path, c.opts(7*day))
	s.Close()
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Error("file was rewritten although nothing had expired")
	}

	// Two days later the first two keys are 8 days old.
	c.advance(2 * day)
	s = openWith(t, path, c.opts(7*day))
	if got, want := fileSize(t, path), int64(headerSize+hashRecSize); got != want {
		t.Errorf("size after compaction = %d, want %d", got, want)
	}
	if !seen(t, s, "recent") {
		t.Error("live key dropped by compaction")
	}
	if seen(t, s, "old-1") {
		t.Error("expired key still reported as seen")
	}
	if _, err := os.Stat(path + ".compact"); !os.IsNotExist(err) {
		t.Error("temporary compaction file left behind")
	}
}

func TestTTLDoesNotExpireEarly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	// Recorded near the end of a unix second. The stored timestamp is that
	// second, and a 1s TTL must not treat the key as already expired.
	c := &clock{t: testNow.Add(900 * time.Millisecond)}
	s := openWith(t, path, c.opts(time.Second))
	if seen(t, s, "x") {
		t.Fatal("x seen on first sight")
	}
	c.advance(200 * time.Millisecond)
	if !seen(t, s, "x") {
		t.Fatal("x forgotten after 200ms with a 1s TTL")
	}
	if got, want := fileSize(t, path), int64(headerSize+hashRecSize); got != want {
		t.Errorf("size = %d, want %d (key was recorded twice)", got, want)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != 1 || st.Expired != 0 {
		t.Errorf("entries/expired = %d/%d, want 1/0", st.Entries, st.Expired)
	}
}

func TestTTLExactBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	s := openWith(t, path, c.opts(time.Hour))
	if seen(t, s, "x") {
		t.Fatal("x seen on first sight")
	}

	// Exactly one TTL later the key is not yet older than the TTL.
	c.advance(time.Hour)
	if !seen(t, s, "x") {
		t.Fatal("x forgotten at exactly its TTL")
	}
	s.Close()

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s = openWith(t, path, c.opts(time.Hour))
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("file rewritten although the key is exactly TTL old")
	}
	if !seen(t, s, "x") {
		t.Error("x forgotten by reopen at exactly its TTL")
	}
	s.Close()

	// One second past the TTL it expires, and compaction drops it.
	c.advance(time.Second)
	s = openWith(t, path, c.opts(time.Hour))
	if got, want := fileSize(t, path), int64(headerSize); got != want {
		t.Errorf("size after compaction = %d, want %d", got, want)
	}
	if seen(t, s, "x") {
		t.Error("x still remembered after its TTL")
	}
}

func TestTTLExpiresWhileRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	s := openWith(t, path, c.opts(time.Hour))
	if seen(t, s, "x") {
		t.Fatal("x reported seen on first sight")
	}
	c.advance(59 * time.Minute)
	if !seen(t, s, "x") {
		t.Fatal("x forgotten before its TTL")
	}
	c.advance(2 * time.Minute)
	if seen(t, s, "x") {
		t.Fatal("x still remembered after its TTL")
	}
	// x is recorded again with a fresh timestamp.
	c.advance(30 * time.Minute)
	if !seen(t, s, "x") {
		t.Fatal("re-recorded x was not remembered")
	}
	s.Close()

	// Reopening drops the superseded first record.
	s = openWith(t, path, c.opts(time.Hour))
	if got, want := fileSize(t, path), int64(headerSize+hashRecSize); got != want {
		t.Errorf("size after compaction = %d, want %d", got, want)
	}
	if !seen(t, s, "x") {
		t.Error("x lost by compaction")
	}
}

func TestStoresShareOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	a := mustOpen(t, path)
	b := mustOpen(t, path)

	if seen(t, a, "x") {
		t.Fatal("x seen on first sight")
	}
	if !seen(t, b, "x") {
		t.Error("b did not pick up the key a recorded")
	}
	if seen(t, b, "y") {
		t.Fatal("y seen on first sight")
	}
	if !seen(t, a, "y") {
		t.Error("a did not pick up the key b recorded")
	}
	if got, want := fileSize(t, path), int64(headerSize+2*hashRecSize); got != want {
		t.Errorf("size = %d, want %d (a key was recorded twice?)", got, want)
	}
}

func TestWriterWaitsForLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := mustOpen(t, path)

	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := lockFile(holder); err != nil {
		t.Fatal(err)
	}

	var finished atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := s.Seen([]byte("new"), nil)
		finished.Store(true)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if finished.Load() {
		t.Fatal("Seen recorded a new key while another process held the lock")
	}
	unlockFile(holder)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Seen still blocked after the lock was released")
	}
}

func TestFollowsCompactionByAnotherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	long := openWith(t, path, c.opts(0))
	seen(t, long, "old")
	c.advance(10 * day)

	// Another process starts with a TTL and compacts the file away.
	other := openWith(t, path, c.opts(7*day))
	other.Close()

	// The long-running store must append to the new file, not the old one.
	seen(t, long, "fresh")
	again := openWith(t, path, c.opts(0))
	if !seen(t, again, "fresh") {
		t.Error("key recorded after compaction went to the replaced file")
	}
	if seen(t, again, "old") {
		t.Error("compacted key came back")
	}
}

func TestReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := mustOpen(t, path)
	seen(t, s, "x")

	existed, err := Reset(path)
	if err != nil || !existed {
		t.Fatalf("Reset = %v, %v; want true, nil", existed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("state file still exists after Reset")
	}
	if existed, err := Reset(path); err != nil || existed {
		t.Errorf("second Reset = %v, %v; want false, nil", existed, err)
	}

	// A store that was open during the reset starts a new file.
	seen(t, s, "y")
	fresh := mustOpen(t, path)
	if !seen(t, fresh, "y") {
		t.Error("key recorded after reset was lost")
	}
}

func TestResetRefusesForeignFile(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"app.log":   "precious log data\n",
		"empty.log": "",              // e.g. a log that was just rotated
		"short.txt": magic[:2],       // a prefix of the magic is not enough
		"almost":    magic[:7] + "X", // nor is a near miss
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Reset(path); !errors.Is(err, errNotState) {
			t.Errorf("%s: err = %v, want %v", name, err, errNotState)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s: Reset deleted a file that was not a state file", name)
		}
	}

	// A device is never a state file. Going through a link keeps the test
	// harmless: at worst the link itself would be removed.
	link := filepath.Join(dir, "null")
	if err := os.Symlink(os.DevNull, link); err != nil {
		t.Skip(err)
	}
	if _, err := Reset(link); !errors.Is(err, errNotState) {
		t.Errorf("device: err = %v, want %v", err, errNotState)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Error("Reset deleted a device")
	}
}

func TestStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	s := openWith(t, path, c.opts(0))
	seen(t, s, "a")
	c.advance(3 * day)
	seen(t, s, "b")
	seen(t, s, "c")
	s.Close()

	ro := openWith(t, path, Options{ReadOnly: true, TTL: 2 * day, Now: c.now})
	st, err := ro.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != 2 || st.Expired != 1 || st.Records != 3 {
		t.Errorf("entries/expired/records = %d/%d/%d, want 2/1/3", st.Entries, st.Expired, st.Records)
	}
	if !st.Oldest.Equal(c.t) || !st.Newest.Equal(c.t) {
		t.Errorf("oldest/newest = %v/%v, want %v", st.Oldest, st.Newest, c.t)
	}
	if st.Size != headerSize+3*hashRecSize {
		t.Errorf("size = %d", st.Size)
	}
}

func TestExactStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := openWith(t, path, Options{Exact: true, Now: fixedClock(testNow)})
	keys := []string{"disk full on /var", "", "dup", "dup", "naïve ünïcode"}
	for _, k := range keys {
		seen(t, s, k)
	}
	s.Close()

	want := int64(headerSize)
	for _, k := range []string{"disk full on /var", "", "dup", "naïve ünïcode"} {
		want += exactRecHead + int64(len(k))
	}
	if got := fileSize(t, path); got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("disk full on /var")) {
		t.Error("exact record does not contain the key itself")
	}

	// Reopening without Exact keeps using the file's own kind.
	s = mustOpen(t, path)
	for _, k := range keys {
		if !seen(t, s, k) {
			t.Errorf("%q lost across reopen", k)
		}
	}
	if seen(t, s, "disk full on /home") {
		t.Error("unexpected key")
	}
	st, _ := s.Stats()
	if !st.Exact || st.Entries != 5 {
		t.Errorf("stats = %+v, want exact with 5 entries", st)
	}
}

func TestExactRefusesHashState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := mustOpen(t, path)
	seen(t, s, "x")
	s.Close()
	if _, err := OpenStore(path, Options{Exact: true}); !errors.Is(err, errHashState) {
		t.Fatalf("err = %v, want %v", err, errHashState)
	}
}

func TestExactCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	s := openWith(t, path, Options{Exact: true, Now: c.now})
	seen(t, s, "old")
	c.advance(3 * day)
	seen(t, s, "new")
	s.Close()

	s = openWith(t, path, Options{TTL: 2 * day, Now: c.now})
	if got, want := fileSize(t, path), int64(headerSize+exactRecHead+len("new")); got != want {
		t.Errorf("size after compaction = %d, want %d", got, want)
	}
	if !seen(t, s, "new") || seen(t, s, "old") {
		t.Error("compaction kept the wrong keys")
	}
	st, _ := s.Stats()
	if !st.Exact {
		t.Error("compaction changed the record kind")
	}
}

// appendBytes simulates a writer that died part-way through an append.
func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

func TestTornRecordIsTruncated(t *testing.T) {
	for _, exact := range []bool{false, true} {
		name := "hash"
		if exact {
			name = "exact"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.seen")
			opts := Options{Exact: exact, Now: fixedClock(testNow)}
			s := openWith(t, path, opts)
			seen(t, s, "a")
			seen(t, s, "b")
			s.Close()
			intact := fileSize(t, path)

			// Half of a record for a long key.
			var torn []byte
			if exact {
				torn = appendExactRecord(nil, "a key that was never fully written", testNow.Unix())
			} else {
				torn = appendHashRecord(nil, hashKey([]byte("never written")), testNow.Unix())
			}
			appendBytes(t, path, torn[:len(torn)/2])

			s = openWith(t, path, opts)
			if got := fileSize(t, path); got != intact {
				t.Errorf("size after reopen = %d, want %d (torn record not removed)", got, intact)
			}
			if seen(t, s, "c") {
				t.Fatal("c seen on first sight")
			}
			s.Close()

			s = openWith(t, path, opts)
			for _, k := range []string{"a", "b", "c"} {
				if !seen(t, s, k) {
					t.Errorf("%q lost after recovering from a torn record", k)
				}
			}
			if s.Len() != 3 {
				t.Errorf("Len = %d, want 3", s.Len())
			}
		})
	}
}

func TestReadOnlyKeepsKeysAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	c := &clock{t: testNow}
	ro := openWith(t, path, Options{ReadOnly: true, Now: c.now})
	if seen(t, ro, "dup-1") {
		t.Fatal("dup-1 seen on first sight")
	}

	// Another process creates the file. The next new key makes the
	// read-only store switch to it, and it must not forget dup-1.
	w := openWith(t, path, c.opts(0))
	seen(t, w, "unrelated")
	w.Close()
	if seen(t, ro, "other-1") {
		t.Fatal("other-1 seen on first sight")
	}
	if !seen(t, ro, "dup-1") {
		t.Error("dup-1 forgotten when the state file was created")
	}
	if !seen(t, ro, "unrelated") {
		t.Error("key recorded by another process not picked up")
	}

	// The same when another process compacts the file.
	c.advance(10 * day)
	if seen(t, ro, "dup-2") {
		t.Fatal("dup-2 seen on first sight")
	}
	w = openWith(t, path, c.opts(7*day))
	w.Close()
	if seen(t, ro, "other-2") {
		t.Fatal("other-2 seen on first sight")
	}
	if !seen(t, ro, "dup-2") {
		t.Error("dup-2 forgotten when the state file was compacted")
	}
}

func TestExactStoreRejectsLongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := openWith(t, path, Options{Exact: true, Now: fixedClock(testNow)})
	called := false
	_, err := s.Seen(make([]byte, maxExactKey+1), func() error { called = true; return nil })
	if !errors.Is(err, errKeyTooLong) {
		t.Fatalf("err = %v, want %v", err, errKeyTooLong)
	}
	if called || fileSize(t, path) != headerSize {
		t.Error("an oversized key was passed on or recorded by the store")
	}
}

func TestReadOnlyLeavesPartialRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.seen")
	s := mustOpen(t, path)
	seen(t, s, "a")
	s.Close()
	appendBytes(t, path, []byte{1, 2, 3})
	size := fileSize(t, path)

	ro := openWith(t, path, Options{ReadOnly: true})
	if !seen(t, ro, "a") {
		t.Error("a not seen")
	}
	if got := fileSize(t, path); got != size {
		t.Errorf("read-only store changed the file size from %d to %d", size, got)
	}
}
