package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

func (a *app) reset(path string) error {
	existed, err := Reset(path)
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(a.stderr, "dedupe-stream: removed %s\n", path)
	} else {
		fmt.Fprintf(a.stderr, "dedupe-stream: nothing to reset at %s\n", path)
	}
	return nil
}

func (a *app) printStats(path, name string, ttl time.Duration) error {
	st, err := OpenStore(path, Options{ReadOnly: true, TTL: ttl, Now: a.now})
	if err != nil {
		return err
	}
	defer st.Close()
	s, err := st.Stats()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	if name != "" {
		fmt.Fprintf(w, "name\t%s\n", name)
	}
	fmt.Fprintf(w, "file\t%s\n", path)
	if !s.Exists {
		fmt.Fprintf(w, "entries\t0 (no state file yet)\n")
		return w.Flush()
	}
	if s.Exact {
		fmt.Fprintf(w, "format\texact keys\n")
	} else {
		fmt.Fprintf(w, "format\tsha256/64 hashes\n")
	}
	fmt.Fprintf(w, "entries\t%d\n", s.Entries)
	if ttl > 0 {
		fmt.Fprintf(w, "expired\t%d (ttl %s)\n", s.Expired, formatTTL(ttl))
	}
	fmt.Fprintf(w, "size\t%s\n", humanBytes(s.Size))
	if s.Entries > 0 {
		fmt.Fprintf(w, "oldest\t%s\n", a.when(s.Oldest))
		fmt.Fprintf(w, "newest\t%s\n", a.when(s.Newest))
	}
	return w.Flush()
}

func (a *app) listStates() error {
	dir, err := stateDir(a.getenv)
	if err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	header, failed := false, 0
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), stateExt)
		if !ok || !e.Type().IsRegular() || validName(name) != nil {
			continue
		}
		st, err := OpenStore(filepath.Join(dir, e.Name()), Options{ReadOnly: true, Now: a.now})
		if err != nil {
			fmt.Fprintf(a.stderr, "dedupe-stream: %v\n", err)
			failed++
			continue
		}
		s, err := st.Stats()
		st.Close()
		if err != nil {
			fmt.Fprintf(a.stderr, "dedupe-stream: %s: %v\n", e.Name(), err)
			failed++
			continue
		}
		if !header {
			fmt.Fprintln(w, "NAME\tENTRIES\tSIZE\tNEWEST")
			header = true
		}
		newest := "-"
		if s.Entries > 0 {
			newest = s.Newest.In(time.Local).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", name, s.Entries, humanBytes(s.Size), newest)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d state file(s) in %s could not be read", failed, dir)
	}
	return nil
}

// when formats t with its age relative to now, e.g.
// "2025-09-06 20:51:37 UTC (3d ago)".
func (a *app) when(t time.Time) string {
	return t.In(time.Local).Format("2006-01-02 15:04:05 MST") + " (" + humanAge(a.now().Sub(t)) + ")"
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", d/time.Minute)
	case d < 2*day:
		return fmt.Sprintf("%dh ago", d/time.Hour)
	default:
		return fmt.Sprintf("%dd ago", d/day)
	}
}

func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := -1
	for i+1 < len(units) && v >= 1024 {
		v /= 1024
		i++
	}
	// %.1f rounds 1023.95 up to "1024.0". Carry into the next unit so a
	// size just under 1 MiB is not shown as 1024.0 KiB.
	if i+1 < len(units) && fmt.Sprintf("%.1f", v) == "1024.0" {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
