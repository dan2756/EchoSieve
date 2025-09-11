package main

import "testing"

func TestKeyExtraction(t *testing.T) {
	tests := []struct {
		name string
		opts keyOptions
		line string
		want string
	}{
		{"whole line", keyOptions{}, "  Some Line 42 ", "  Some Line 42 "},

		{"regex group", keyOptions{pattern: `code=(\d+)`}, "ERROR code=503 path=/x", "503"},
		{"regex first group only", keyOptions{pattern: `(\w+)=(\w+)`}, "user=bob id=7", "user"},
		{"regex whole match", keyOptions{pattern: `ERROR \S+`}, "12:00 ERROR timeout after 3s", "ERROR timeout"},
		{"regex optional group unmatched", keyOptions{pattern: `ERROR( \d+)?`}, "x ERROR y", "ERROR"},
		{"regex no match", keyOptions{pattern: `code=(\d+)`}, "all good", "all good"},

		{"field whitespace", keyOptions{field: 3}, "2025-09-11  12:00:01\tWARN disk low", "WARN"},
		{"field first", keyOptions{field: 1}, "  lead space", "lead"},
		{"field out of range", keyOptions{field: 9}, "a b c", "a b c"},
		{"field delimiter", keyOptions{field: 2, delim: ",", delimSet: true}, "1,alice,admin", "alice"},
		{"field last", keyOptions{field: 3, delim: ",", delimSet: true}, "1,alice,admin", "admin"},
		{"field empty", keyOptions{field: 2, delim: ",", delimSet: true}, "1,,admin", ""},
		{"field multi-byte delimiter", keyOptions{field: 2, delim: " | ", delimSet: true}, "a | b | c", "b"},
		{"field delimiter out of range", keyOptions{field: 4, delim: ",", delimSet: true}, "1,2,3", "1,2,3"},

		{"ignore case", keyOptions{ignoreCase: true}, "Disk FULL on /Var", "disk full on /var"},
		{"trim", keyOptions{trim: true}, " \t padded \t", "padded"},
		{"strip numbers", keyOptions{stripNumbers: true}, "retry 3 of 10 after 250ms", "retry 0 of 0 after 0ms"},
		{"strip numbers keeps zero", keyOptions{stripNumbers: true}, "0", "0"},
		{"strip numbers no digits", keyOptions{stripNumbers: true}, "nothing here", "nothing here"},

		{"field then normalize", keyOptions{field: 2, ignoreCase: true, stripNumbers: true}, "x Worker-12 y", "worker-0"},
		{"regex then trim", keyOptions{pattern: `msg=([^;]*)`, trim: true}, "msg=  spaced  ;", "spaced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := newKeyer(tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(k.key([]byte(tt.line))); got != tt.want {
				t.Errorf("key(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestKeyOptionErrors(t *testing.T) {
	for name, o := range map[string]keyOptions{
		"bad regex":       {pattern: `(`},
		"key and field":   {pattern: `x`, field: 1},
		"negative field":  {field: -1},
		"delimiter alone": {delim: ",", delimSet: true},
		"empty delimiter": {field: 1, delim: "", delimSet: true},
	} {
		if _, err := newKeyer(o); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNormalizationSameKeys(t *testing.T) {
	k, err := newKeyer(keyOptions{ignoreCase: true, trim: true, stripNumbers: true})
	if err != nil {
		t.Fatal(err)
	}
	same := [][2]string{
		{"retry 3", "retry 7"},
		{"Connection reset by peer 10.0.0.12:443", "connection reset by PEER 10.0.0.9:8443"},
		{"  timeout ", "TIMEOUT"},
	}
	for _, p := range same {
		a, b := string(k.key([]byte(p[0]))), string(k.key([]byte(p[1])))
		if a != b {
			t.Errorf("%q -> %q and %q -> %q should match", p[0], a, p[1], b)
		}
	}
	if a, b := k.key([]byte("retry 3")), k.key([]byte("retry")); string(a) == string(b) {
		t.Errorf("%q and %q should differ", "retry 3", "retry")
	}
}
