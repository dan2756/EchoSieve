package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := OpenStore(path, fixedClock(testNow))
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
	if _, err := OpenStore(path, time.Now); !errors.Is(err, errNotState) {
		t.Fatalf("err = %v, want %v", err, errNotState)
	}
}
