//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

import "os"

// Without flock, concurrent writers to one state file are not coordinated.
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
