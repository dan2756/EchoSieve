package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2025, 9, 6, 12, 0, 0, 0, time.UTC)

// runAt runs the command once with the given environment, clock and stdin.
func runAt(t *testing.T, now time.Time, env map[string]string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	a := &app{
		stdin:  strings.NewReader(stdin),
		stdout: &out,
		stderr: &errOut,
		getenv: func(k string) string { return env[k] },
		now:    func() time.Time { return now },
	}
	code := a.run(args)
	return out.String(), errOut.String(), code
}

func runApp(t *testing.T, env map[string]string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	return runAt(t, testNow, env, stdin, args...)
}

func TestPersistsAcrossRuns(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")

	out, errOut, code := runApp(t, nil, "a\nb\na\nb\nc\n", "--file", file)
	if code != 0 {
		t.Fatalf("run 1: exit %d: %s", code, errOut)
	}
	if want := "a\nb\nc\n"; out != want {
		t.Errorf("run 1 output = %q, want %q", out, want)
	}

	out, errOut, code = runApp(t, nil, "c\nd\na\nd\ne\n", "--file", file)
	if code != 0 {
		t.Fatalf("run 2: exit %d: %s", code, errOut)
	}
	if want := "d\ne\n"; out != want {
		t.Errorf("run 2 output = %q, want %q", out, want)
	}
}

func TestNamedStateUsesXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"XDG_STATE_HOME": dir, "HOME": "/nonexistent"}
	if _, errOut, code := runApp(t, env, "hello\n", "--name", "app-errors"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "dedupe-stream", "app-errors.seen")); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultStateUnderHome(t *testing.T) {
	home := t.TempDir()
	// A relative XDG_STATE_HOME is ignored, per the XDG spec.
	env := map[string]string{"HOME": home, "XDG_STATE_HOME": "relative/dir"}
	if _, errOut, code := runApp(t, env, "hello\n"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := filepath.Join(home, ".local", "state", "dedupe-stream", "default.seen")
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}

func TestNamesAreIndependent(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": t.TempDir()}
	runApp(t, env, "a\n", "--name", "one")
	out, _, _ := runApp(t, env, "a\n", "--name", "two")
	if out != "a\n" {
		t.Errorf("state %q leaked into %q: got %q", "one", "two", out)
	}
}

func TestLineEndings(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	out, _, _ := runApp(t, nil, "a\r\na\nb", "--file", file)
	if want := "a\r\nb\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestLongLines(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	long := strings.Repeat("x", 300<<10)
	in := long + "\n" + long + "y\n" + long + "\n"
	out, errOut, code := runApp(t, nil, in, "--file", file)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if want := long + "\n" + long + "y\n"; out != want {
		t.Errorf("got %d bytes of output, want %d", len(out), len(want))
	}
}

func TestUsageErrors(t *testing.T) {
	env := map[string]string{"HOME": t.TempDir()}
	for _, args := range [][]string{
		{"--name", "../escape"},
		{"--name", ".hidden"},
		{"--name", "a", "--file", "b"},
		{"--bogus"},
		{"stray-arg"},
		{"--key", "("},
		{"--key", "x", "-f", "2"},
		{"-d", ","},
		{"-f", "0", "-d", ""},
		{"-f", "0"},
		{"--field", "0", "-d", ","},
		{"--field", "abc"},
	} {
		if _, _, code := runApp(t, env, "", args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	out, _, code := runApp(t, nil, "", "--help")
	if code != 0 || !strings.Contains(out, "Usage: dedupe-stream") {
		t.Errorf("--help: exit %d, output %q", code, out)
	}
	out, _, code = runApp(t, nil, "", "--version")
	if code != 0 || !strings.HasPrefix(out, "dedupe-stream ") {
		t.Errorf("--version: exit %d, output %q", code, out)
	}
}

func TestKeyFlagsEndToEnd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	in := "" +
		"10:00:01 ERROR db: connection refused (attempt 1)\n" +
		"10:00:02 ERROR db: connection refused (attempt 2)\n" +
		"10:00:03 WARN cache: miss rate 12%\n" +
		"10:00:04 error DB: Connection refused (attempt 3)\n"
	out, errOut, code := runApp(t, nil, in, "--file", file, "--key", `(?:ERROR|WARN|error) (.*)`, "-i", "--strip-numbers")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := "" +
		"10:00:01 ERROR db: connection refused (attempt 1)\n" +
		"10:00:03 WARN cache: miss rate 12%\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestFieldFlagEndToEnd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	in := "1,alice,login\n2,bob,login\n3,alice,logout\n"
	out, _, _ := runApp(t, nil, in, "--file", file, "-f", "2", "-d", ",")
	if want := "1,alice,login\n2,bob,login\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestExactKeyTooLongPassesThrough(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	long := strings.Repeat("x", maxExactKey+1) + "\n"
	in := long + "small\n" + long + "small\n"
	out, errOut, code := runApp(t, nil, in, "--file", file, "--exact")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// The long line cannot be recorded, so it passes every time, and the
	// stream goes on past it.
	if want := long + "small\n" + long; out != want {
		t.Errorf("got %d bytes of output, want %d", len(out), len(want))
	}
	if !strings.Contains(errOut, "too long") {
		t.Errorf("no warning on stderr: %q", errOut)
	}
	if got, want := fileSize(t, file), int64(headerSize+exactRecHead+len("small")); got != want {
		t.Errorf("state size = %d, want %d", got, want)
	}
}

func TestDryRunRecordsNothing(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	runApp(t, nil, "a\n", "--file", file)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	out, _, code := runApp(t, nil, "a\nb\nb\nc\n", "--file", file, "--dry-run")
	if code != 0 || out != "b\nc\n" {
		t.Errorf("dry run: exit %d, output %q", code, out)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Error("--dry-run modified the state file")
	}

	missing := filepath.Join(t.TempDir(), "never.seen")
	runApp(t, nil, "a\n", "--file", missing, "--dry-run")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("--dry-run created %s", missing)
	}
}

func TestInvert(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	runApp(t, nil, "a\nb\n", "--file", file)
	out, _, _ := runApp(t, nil, "a\nc\nc\nb\nd\n", "--file", file, "--invert")
	if want := "a\nc\nb\n"; out != want {
		t.Errorf("--invert output = %q, want %q", out, want)
	}
	// --invert still records new lines, exactly like a normal run.
	out, _, _ = runApp(t, nil, "c\nd\ne\n", "--file", file)
	if want := "e\n"; out != want {
		t.Errorf("after --invert, output = %q, want %q", out, want)
	}
}

func TestTTLFlag(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	runAt(t, testNow, nil, "a\nb\n", "--file", file)
	runAt(t, testNow.Add(6*day), nil, "c\n", "--file", file)

	out, errOut, code := runAt(t, testNow.Add(8*day), nil, "a\nb\nc\nd\n", "--file", file, "--ttl", "7d")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if want := "a\nb\nd\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestStatsFlag(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": t.TempDir()}
	runAt(t, testNow, env, "a\nb\nc\n", "--name", "web")

	out, errOut, code := runAt(t, testNow.Add(3*day), env, "", "--name", "web", "--stats")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{"name     web\n", "entries  3\n", "size     64 B\n", "(3d ago)"} {
		if !strings.Contains(out, want) {
			t.Errorf("--stats output missing %q:\n%s", want, out)
		}
	}

	out, _, _ = runAt(t, testNow.Add(3*day), env, "", "--name", "web", "--stats", "--ttl", "2d")
	if !strings.Contains(out, "entries  0\n") || !strings.Contains(out, "expired  3 (ttl 2d") {
		t.Errorf("--stats --ttl output:\n%s", out)
	}

	out, _, code = runApp(t, env, "", "--name", "nothing-yet", "--stats")
	if code != 0 || !strings.Contains(out, "no state file yet") {
		t.Errorf("--stats on a missing state: exit %d, output %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(env["XDG_STATE_HOME"], "dedupe-stream", "nothing-yet.seen")); !os.IsNotExist(err) {
		t.Error("--stats created a state file")
	}
}

func TestListFlag(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": t.TempDir()}
	out, _, code := runApp(t, env, "", "--list")
	if code != 0 || out != "" {
		t.Errorf("--list with no state: exit %d, output %q", code, out)
	}

	runApp(t, env, "a\nb\n", "--name", "app-errors")
	runApp(t, env, "a\n")
	dir := filepath.Join(env["XDG_STATE_HOME"], "dedupe-stream")
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o600)

	out, errOut, code := runApp(t, env, "", "--list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") ||
		!strings.HasPrefix(lines[1], "app-errors  2 ") || !strings.HasPrefix(lines[2], "default     1 ") {
		t.Errorf("--list output:\n%s", out)
	}
}

func TestResetFlag(t *testing.T) {
	env := map[string]string{"XDG_STATE_HOME": t.TempDir()}
	runApp(t, env, "a\n", "--name", "web")
	runApp(t, env, "a\n", "--name", "api")

	_, errOut, code := runApp(t, env, "", "--name", "web", "--reset")
	if code != 0 || !strings.Contains(errOut, "removed") {
		t.Fatalf("--reset: exit %d, stderr %q", code, errOut)
	}
	if out, _, _ := runApp(t, env, "a\n", "--name", "web"); out != "a\n" {
		t.Errorf("after --reset, output = %q, want %q", out, "a\n")
	}
	if out, _, _ := runApp(t, env, "a\n", "--name", "api"); out != "" {
		t.Errorf("--reset of web affected api: output = %q", out)
	}

	log := filepath.Join(t.TempDir(), "app.log")
	os.WriteFile(log, []byte("not a state file\n"), 0o644)
	if _, _, code := runApp(t, env, "", "--file", log, "--reset"); code != exitError {
		t.Errorf("--reset on a foreign file: exit %d, want %d", code, exitError)
	}
	if _, err := os.Stat(log); err != nil {
		t.Error("--reset deleted a foreign file")
	}
}

// chunkReader returns each chunk from its own Read, then a final error.
type chunkReader struct {
	chunks [][]byte
	err    error
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i < len(r.chunks) {
		n := copy(p, r.chunks[r.i])
		r.i++
		return n, nil
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func TestReadErrorDoesNotRecordPartialLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	readErr := errors.New("disk read failed")
	var out, errOut bytes.Buffer
	a := &app{
		stdin: &chunkReader{
			chunks: [][]byte{[]byte("keep\npartial")},
			err:    readErr,
		},
		stdout: &out,
		stderr: &errOut,
		getenv: func(string) string { return "" },
		now:    func() time.Time { return testNow },
	}
	code := a.run([]string{"--file", file})
	if code != exitError || !strings.Contains(errOut.String(), readErr.Error()) {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	if got := out.String(); got != "keep\n" {
		t.Errorf("stdout = %q, want %q", got, "keep\n")
	}

	// The torn prefix was not recorded, so both it and a new line pass.
	out2, errOut2, code := runApp(t, nil, "partial\nhello\n", "--file", file)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut2)
	}
	if out2 != "partial\nhello\n" {
		t.Errorf("after read error, output = %q, want %q", out2, "partial\nhello\n")
	}
}

func TestEmptyFileIsNotAdopted(t *testing.T) {
	log := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := runApp(t, nil, "hello\n", "--file", log)
	if code != exitError || !strings.Contains(errOut, "not a dedupe-stream state file") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty log was modified: %q", got)
	}
	out, errOut, code := runApp(t, nil, "hello\n", "--file", log, "--dry-run")
	if code != exitError || out != "" || !strings.Contains(errOut, "not a dedupe-stream state file") {
		t.Fatalf("dry-run: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestActionConflicts(t *testing.T) {
	env := map[string]string{"HOME": t.TempDir()}
	for _, args := range [][]string{
		{"--reset", "--stats"},
		{"--stats", "--list"},
		{"--reset", "--dry-run"},
		{"--list", "--name", "x"},
		{"--ttl", "soon"},
		{"--ttl", "0s"},
	} {
		if _, _, code := runApp(t, env, "", args...); code != exitUsage {
			t.Errorf("%q: exit %d, want %d", args, code, exitUsage)
		}
	}
}

func TestExactFlag(t *testing.T) {
	file := filepath.Join(t.TempDir(), "x.seen")
	out, errOut, code := runApp(t, nil, "a\nb\na\n", "--file", file, "--exact")
	if code != 0 || out != "a\nb\n" {
		t.Fatalf("exit %d, output %q, stderr %q", code, out, errOut)
	}
	// Later runs need not repeat --exact.
	if out, _, _ := runApp(t, nil, "a\nc\n", "--file", file); out != "c\n" {
		t.Errorf("output = %q, want %q", out, "c\n")
	}
	out, _, _ = runApp(t, nil, "", "--file", file, "--stats")
	if !strings.Contains(out, "format   exact keys\n") || !strings.Contains(out, "entries  3\n") {
		t.Errorf("--stats output:\n%s", out)
	}

	hashed := filepath.Join(t.TempDir(), "h.seen")
	runApp(t, nil, "a\n", "--file", hashed)
	if _, errOut, code := runApp(t, nil, "a\n", "--file", hashed, "--exact"); code != exitError || !strings.Contains(errOut, "--exact") {
		t.Errorf("--exact on a hash state: exit %d, stderr %q", code, errOut)
	}
}
