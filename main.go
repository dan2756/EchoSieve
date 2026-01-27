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

State:
      --name NAME          state to use (default "default"), kept in
                           $XDG_STATE_HOME/dedupe-stream/NAME.seen
      --file PATH          use the state file at PATH instead of a named state
      --ttl DURATION       forget lines first seen longer ago than this,
                           e.g. 12h, 7d, 2w (default: remember forever)
      --exact              store whole keys instead of 64-bit hashes, ruling
                           out collisions at the cost of space (new state only)

Keys (what counts as "the same line"):
      --key REGEX          compare only the first capture group of REGEX, or
                           the whole match if it has no groups
  -f, --field N            compare only field N (counting from 1)
  -d, --delimiter SEP      split fields on SEP (default: runs of whitespace)
  -i, --ignore-case        ignore case differences
      --trim               ignore leading and trailing whitespace
      --strip-numbers      treat all runs of digits as equal, so
                           "retry 3" and "retry 7" are the same line
  Lines that --key does not match, or that have fewer than N fields, are
  compared whole.

Output:
      --dry-run            show what would pass, but record nothing
      --invert             print only the lines that have been seen before

Managing state (these read no input):
      --reset              delete the state for --name or --file
      --stats              show entry count, file size and oldest entry
      --list               list the named states and their sizes

  -h, --help               show this help
      --version            print the version and exit

Exit status is 0 on success, 1 on errors, and 2 for invalid usage.
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
		keyOpts     keyOptions
		ttl         ttlFlag
		exact       = fs.Bool("exact", false, "")
		dryRun      = fs.Bool("dry-run", false, "")
		invert      = fs.Bool("invert", false, "")
		reset       = fs.Bool("reset", false, "")
		stats       = fs.Bool("stats", false, "")
		list        = fs.Bool("list", false, "")
		showVersion = fs.Bool("version", false, "")
	)
	fs.Var(&ttl, "ttl", "")
	fs.StringVar(&keyOpts.pattern, "key", "", "")
	fs.IntVar(&keyOpts.field, "f", 0, "")
	fs.IntVar(&keyOpts.field, "field", 0, "")
	fs.StringVar(&keyOpts.delim, "d", "", "")
	fs.StringVar(&keyOpts.delim, "delimiter", "", "")
	fs.BoolVar(&keyOpts.ignoreCase, "i", false, "")
	fs.BoolVar(&keyOpts.ignoreCase, "ignore-case", false, "")
	fs.BoolVar(&keyOpts.trim, "trim", false, "")
	fs.BoolVar(&keyOpts.stripNumbers, "strip-numbers", false, "")

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
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "d" || f.Name == "delimiter" {
			keyOpts.delimSet = true
		}
	})
	keys, err := newKeyer(keyOpts)
	if err != nil {
		return a.usageError(err.Error())
	}

	switch n := countTrue(*reset, *stats, *list); {
	case n > 1:
		return a.usageError("use only one of --reset, --stats and --list")
	case n == 1 && *dryRun && *reset:
		return a.usageError("--dry-run cannot be combined with --reset")
	}
	if *list {
		if *name != "" || *file != "" {
			return a.usageError("--list shows every named state; it takes no --name or --file")
		}
		return a.exit(a.listStates())
	}

	path, err := a.statePath(*name, *file)
	if err != nil {
		return a.usageError(err.Error())
	}
	switch {
	case *reset:
		return a.exit(a.reset(path))
	case *stats:
		label := *name
		if label == "" && *file == "" {
			label = defaultName
		}
		return a.exit(a.printStats(path, label, time.Duration(ttl)))
	}

	st, err := OpenStore(path, Options{
		Exact:    *exact,
		ReadOnly: *dryRun,
		TTL:      time.Duration(ttl),
		Now:      a.now,
	})
	if err != nil {
		return a.fail(err)
	}
	defer st.Close()
	return a.exit(a.filter(st, keys, *invert))
}

// filter copies never-seen lines from stdin to stdout, or with invert only
// the already-seen ones. Each line is written with its own call so output is
// never held back waiting for more input.
func (a *app) filter(st *Store, keys *keyer, invert bool) error {
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
			if err := a.filterLine(st, keys.key(trimEOL(line)), line, invert); err != nil {
				return err
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

func (a *app) filterLine(st *Store, key, line []byte, invert bool) error {
	write := func() error {
		_, err := a.stdout.Write(line)
		return err
	}
	if !invert {
		_, err := st.Seen(key, write)
		return err
	}
	seen, err := st.Seen(key, nil)
	if err != nil || !seen {
		return err
	}
	return write()
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

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func (a *app) exit(err error) int {
	if err != nil {
		return a.fail(err)
	}
	return exitOK
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
