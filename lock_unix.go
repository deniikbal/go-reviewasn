//go:build !windows

package main

import (
	"os"
	"syscall"
)

// kunciFile: kunci eksklusif file di Linux/macOS memakai syscall.Flock.
// Gagal (return error) berarti instance lain sedang memegang lock.
func kunciFile(f *os.File) error {
	// LOCK_EX (eksklusif) | LOCK_NB (tidak menunggu, langsung gagal)
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
