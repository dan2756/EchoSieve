package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// keyer turns an input line into the key used for deduplication. By default
// the key is the whole line; --key or --field narrow it to part of the line,
// and the normalization flags make near-identical keys compare equal.
type keyer struct {
	re           *regexp.Regexp // --key
	field        int            // --field, 1-based; 0 means unset
	delim        []byte         // --delimiter; nil splits on runs of whitespace
	ignoreCase   bool
	trim         bool
	stripNumbers bool
}

type keyOptions struct {
	pattern      string
	field        int
	fieldSet     bool
	delim        string
	delimSet     bool
	ignoreCase   bool
	trim         bool
	stripNumbers bool
}

func newKeyer(o keyOptions) (*keyer, error) {
	k := &keyer{
		field:        o.field,
		ignoreCase:   o.ignoreCase,
		trim:         o.trim,
		stripNumbers: o.stripNumbers,
	}
	if o.pattern != "" {
		if o.field != 0 {
			return nil, errors.New("--key and --field cannot be combined")
		}
		re, err := regexp.Compile(o.pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid --key: %w", err)
		}
		k.re = re
	}
	if o.field < 0 || (o.fieldSet && o.field == 0) {
		return nil, fmt.Errorf("invalid --field %d: fields are numbered from 1", o.field)
	}
	if o.delimSet {
		if o.field == 0 {
			return nil, errors.New("--delimiter needs --field")
		}
		if o.delim == "" {
			return nil, errors.New("--delimiter must not be empty")
		}
		k.delim = []byte(o.delim)
	}
	return k, nil
}

// key returns the key for line, which must not include its line ending.
// When --key does not match or the line has too few fields, the whole line
// is used. The result may share memory with line.
func (k *keyer) key(line []byte) []byte {
	key := line
	switch {
	case k.re != nil:
		key = k.match(line)
	case k.field > 0:
		key = k.pickField(line)
	}
	if k.trim {
		key = bytes.TrimSpace(key)
	}
	if k.ignoreCase {
		key = toLower(key)
	}
	if k.stripNumbers {
		key = collapseDigits(key)
	}
	return key
}

// match returns the first capture group of the --key pattern, or the whole
// match when the pattern has no groups or the group did not participate.
func (k *keyer) match(line []byte) []byte {
	loc := k.re.FindSubmatchIndex(line)
	switch {
	case loc == nil:
		return line
	case len(loc) >= 4 && loc[2] >= 0:
		return line[loc[2]:loc[3]]
	default:
		return line[loc[0]:loc[1]]
	}
}

func (k *keyer) pickField(line []byte) []byte {
	if k.delim == nil {
		fields := bytes.Fields(line)
		if k.field > len(fields) {
			return line
		}
		return fields[k.field-1]
	}
	rest := line
	for i := 1; ; i++ {
		j := bytes.Index(rest, k.delim)
		if i == k.field {
			if j < 0 {
				return rest
			}
			return rest[:j]
		}
		if j < 0 {
			return line
		}
		rest = rest[j+len(k.delim):]
	}
}

// toLower is bytes.ToLower, except that bytes which are not valid UTF-8 are
// kept as they are rather than all turned into U+FFFD, so lines in another
// encoding (say, Latin-1 "Jos\xe9" and "Jos\xe8") do not become equal.
func toLower(b []byte) []byte {
	if utf8.Valid(b) {
		return bytes.ToLower(b)
	}
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			out = append(out, b[0])
		} else {
			out = utf8.AppendRune(out, unicode.ToLower(r))
		}
		b = b[n:]
	}
	return out
}

// collapseDigits replaces every run of ASCII digits with a single "0", so
// "retry 3 of 10" and "retry 7 of 10" produce the same key.
func collapseDigits(b []byte) []byte {
	if bytes.IndexAny(b, "0123456789") < 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	inRun := false
	for _, c := range b {
		if c >= '0' && c <= '9' {
			if !inRun {
				out = append(out, '0')
				inRun = true
			}
			continue
		}
		inRun = false
		out = append(out, c)
	}
	return out
}
