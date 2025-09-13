package main

import (
	"testing"
	"time"
)

func TestParseTTL(t *testing.T) {
	good := map[string]time.Duration{
		"30s":   30 * time.Second,
		"90m":   90 * time.Minute,
		"36h":   36 * time.Hour,
		"7d":    7 * day,
		"2w":    14 * day,
		"1d12h": 36 * time.Hour,
		"1.5d":  36 * time.Hour,
		"1w2d":  9 * day,
	}
	for in, want := range good {
		got, err := parseTTL(in)
		if err != nil || got != want {
			t.Errorf("parseTTL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "7", "d", "7x", "-1d", "0d", "500ms", "1d 2h", "7D", "99999999999w"} {
		if got, err := parseTTL(in); err == nil {
			t.Errorf("parseTTL(%q) = %v, want an error", in, got)
		}
	}
}

func TestFormatTTL(t *testing.T) {
	for d, want := range map[time.Duration]string{
		14 * day:         "2w",
		7 * day:          "1w",
		3 * day:          "3d",
		36 * time.Hour:   "36h",
		90 * time.Minute: "1h30m",
		40 * time.Second: "40s",
	} {
		if got := formatTTL(d); got != want {
			t.Errorf("formatTTL(%v) = %q, want %q", d, got, want)
		}
	}
}
