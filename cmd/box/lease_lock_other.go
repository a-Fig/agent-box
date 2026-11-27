//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func tryLeaseLockFile(f *os.File) (bool, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK || err == unix.EINTR {
		return false, nil
	}
	return err == nil, err
}
