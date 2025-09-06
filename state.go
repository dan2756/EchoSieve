package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

const (
	defaultName = "default"
	stateExt    = ".seen"
)

// stateDir returns the directory holding named state files:
// $XDG_STATE_HOME/dedupe-stream, or ~/.local/state/dedupe-stream.
func stateDir(getenv func(string) string) (string, error) {
	// The XDG spec says relative paths must be ignored.
	if d := getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "dedupe-stream"), nil
	}
	home := getenv("HOME")
	if home == "" {
		return "", errors.New("cannot find a state directory: set HOME or XDG_STATE_HOME, or use --file")
	}
	return filepath.Join(home, ".local", "state", "dedupe-stream"), nil
}

// validName reports whether name is usable as a state name. Names become
// file names, so they are restricted to a safe character set.
func validName(name string) error {
	if name == "" {
		return errors.New("state name must not be empty")
	}
	if name[0] == '.' || name[0] == '-' {
		return fmt.Errorf("invalid state name %q: must not start with %q", name, name[0])
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("invalid state name %q: use letters, digits, '.', '_' and '-'", name)
		}
	}
	return nil
}
