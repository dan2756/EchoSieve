package main

import (
	"bytes"
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
