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
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("precious log data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reset(path); !errors.Is(err, errNotState) {
		t.Fatalf("err = %v, want %v", err, errNotState)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Reset deleted a file that was not a state file")
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
