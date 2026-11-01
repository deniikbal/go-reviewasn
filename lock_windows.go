//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// kunciFile: kunci eksklusif file di Windows memakai LockFileEx.
// Gagal (return error) berarti instance lain sedang memegang lock.
func kunciFile(f *os.File) error {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	lockFileEx := kernel32.NewProc("LockFileEx")

	// LOCKFILE_EXCLUSIVE_LOCK (2) | LOCKFILE_FAIL_IMMEDIATELY (1)
	const flags = 2 | 1
	var overlapped syscall.Overlapped // wajib pointer OVERLAPPED yang valid
	ret, _, callErr := lockFileEx.Call(
		f.Fd(),
		flags,
		0,                            // reserved
		0xFFFFFFFF, 0x00000000,       // jumlah byte: 2^32-1 (low, high)
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if ret == 0 {
		return callErr
	}
	return nil
}
