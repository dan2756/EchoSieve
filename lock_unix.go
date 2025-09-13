//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on f, waiting as long as it takes.
// flock locks belong to the open file description, so they are released
// automatically if the process dies.
func lockFile(f *os.File) error { return flock(f, syscall.LOCK_EX) }

func unlockFile(f *os.File) error { return flock(f, syscall.LOCK_UN) }

func flock(f *os.File, how int) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	err = conn.Control(func(fd uintptr) {
		for {
			ferr = syscall.Flock(int(fd), how)
			if ferr != syscall.EINTR {
				return
			}
		}
	})
	if err != nil {
		return err
	}
	return ferr
}
