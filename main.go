// Command dedupe-stream passes through only the lines it has never seen
// before, remembering them across runs in a small state file.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"
)

const usage = `Usage: dedupe-stream [flags] < input

Print each input line only the first time it has ever been seen. Seen lines
are remembered in a state file, so they stay suppressed in later runs too.
Output is written line by line, so it works in the middle of a pipeline
such as "tail -f app.log | dedupe-stream | ...".

Flags:
      --name NAME   state to use (default "default"), kept in
                    $XDG_STATE_HOME/dedupe-stream/NAME.seen
      --file PATH   use the state file at PATH instead of a named state
  -h, --help        show this help
      --version     print the version and exit
`

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// version is set at build time with -ldflags "-X main.version=..."; when
// empty, the module version recorded by "go install" is used.
var version = ""

type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	getenv func(string) string
	now    func() time.Time
}

func main() {
	a := &app{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
		now:    time.Now,
	}
	os.Exit(a.run(os.Args[1:]))
}

func (a *app) run(args []string) int {
	fs := flag.NewFlagSet("dedupe-stream", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		name        = fs.String("name", "", "")
		file        = fs.String("file", "", "")
		showVersion = fs.Bool("version", false, "")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, usage)
			return exitOK
		}
		return a.usageError(err.Error())
	}
	if *showVersion {
		fmt.Fprintln(a.stdout, "dedupe-stream", versionString())
		return exitOK
	}
	if fs.NArg() > 0 {
		return a.usageError(fmt.Sprintf("unexpected argument %q (input is read from stdin)", fs.Arg(0)))
	}

	path, err := a.statePath(*name, *file)
	if err != nil {
		return a.usageError(err.Error())
	}
	st, err := OpenStore(path, a.now)
	if err != nil {
		return a.fail(err)
	}
	defer st.Close()

	if err := a.filter(st); err != nil {
		return a.fail(err)
	}
	return exitOK
}

// filter copies never-seen lines from stdin to stdout. Each line is written
// with its own call so output is never held back waiting for more input.
func (a *app) filter(st *Store) error {
	r := bufio.NewReaderSize(a.stdin, 64<<10)
	var buf []byte
	for {
		line, err := readLine(r, buf[:0])
		buf = line
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				line = append(line, '\n') // unterminated last line
				buf = line
			}
			_, serr := st.Seen(trimEOL(line), func() error {
				_, err := a.stdout.Write(line)
				return err
			})
			if serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading input: %w", err)
		}
	}
}

// readLine reads one line, including its newline, into buf. Unlike
// bufio.Scanner it has no maximum line length.
func readLine(r *bufio.Reader, buf []byte) ([]byte, error) {
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err != bufio.ErrBufferFull {
			return buf, err
		}
	}
}

// trimEOL strips a trailing "\n" or "\r\n", so files with either line
// ending produce the same keys.
func trimEOL(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

func (a *app) statePath(name, file string) (string, error) {
	if file != "" {
		if name != "" {
			return "", errors.New("--name and --file cannot be combined")
		}
		return filepath.Clean(file), nil
	}
	if name == "" {
		name = defaultName
	}
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := stateDir(a.getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+stateExt), nil
}

func (a *app) usageError(msg string) int {
	fmt.Fprintf(a.stderr, "dedupe-stream: %s\nRun 'dedupe-stream --help' for usage.\n", msg)
	return exitUsage
}

func (a *app) fail(err error) int {
	fmt.Fprintf(a.stderr, "dedupe-stream: %v\n", err)
	return exitError
}

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
