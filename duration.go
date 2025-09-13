package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const day = 24 * time.Hour

var ttlUnits = map[string]time.Duration{
	"s": time.Second,
	"m": time.Minute,
	"h": time.Hour,
	"d": day,
	"w": 7 * day,
}

// parseTTL parses a duration such as "90m", "36h", "7d", "2w" or "1d12h".
// Unlike time.ParseDuration it accepts days and weeks.
func parseTTL(s string) (time.Duration, error) {
	invalid := fmt.Errorf("invalid duration %q (try 90m, 36h, 7d or 2w)", s)
	if s == "" {
		return 0, invalid
	}
	var total float64
	for rest := s; rest != ""; {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		j := i
		for j < len(rest) && rest[j] >= 'a' && rest[j] <= 'z' {
			j++
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		unit, ok := ttlUnits[rest[i:j]]
		if i == 0 || err != nil || !ok {
			return 0, invalid
		}
		total += n * float64(unit)
		rest = rest[j:]
	}
	if total < float64(time.Second) {
		return 0, fmt.Errorf("duration %q is too short (minimum 1s)", s)
	}
	if total >= math.MaxInt64 {
		return 0, fmt.Errorf("duration %q is too long", s)
	}
	return time.Duration(total), nil
}

// formatTTL is the inverse of parseTTL, preferring whole weeks or days.
func formatTTL(d time.Duration) string {
	switch {
	case d%(7*day) == 0:
		return strconv.FormatInt(int64(d/(7*day)), 10) + "w"
	case d%day == 0:
		return strconv.FormatInt(int64(d/day), 10) + "d"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// ttlFlag is a flag.Value for --ttl.
type ttlFlag time.Duration

func (t *ttlFlag) String() string {
	if *t == 0 {
		return ""
	}
	return formatTTL(time.Duration(*t))
}

func (t *ttlFlag) Set(s string) error {
	d, err := parseTTL(s)
	if err != nil {
		return err
	}
	*t = ttlFlag(d)
	return nil
}
