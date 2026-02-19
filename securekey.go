package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// secureAlloc allocates a page-aligned memory region outside the Go heap,
// locks it into RAM (no swap), and returns a slice backed by that region.
// The caller must call secureFree when done.
func secureAlloc(size int) ([]byte, error) {
	pageSize := os.Getpagesize()
	// Round up to page boundary
	pages := (size + pageSize - 1) / pageSize
	allocSize := pages * pageSize

	mem, err := unix.Mmap(-1, 0, allocSize,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap: %w", err)
	}

	if err := unix.Mlock(mem); err != nil {
		unix.Munmap(mem)
		return nil, fmt.Errorf("mlock: %w", err)
	}

	return mem[:size], nil
}

// secureReadOnly marks a secure allocation as read-only.
// Any subsequent write to this memory will cause a SIGSEGV.
func secureReadOnly(mem []byte) error {
	pageSize := os.Getpagesize()
	pages := (len(mem) + pageSize - 1) / pageSize
	allocSize := pages * pageSize
	// Recover the full page-aligned slice for mprotect
	fullMem := unsafe.Slice(&mem[0], allocSize)
	return unix.Mprotect(fullMem, unix.PROT_READ)
}

// secureFree zeros, unlocks, and unmaps a secure allocation.
func secureFree(mem []byte) {
	if len(mem) == 0 {
		return
	}
	pageSize := os.Getpagesize()
	pages := (len(mem) + pageSize - 1) / pageSize
	allocSize := pages * pageSize
	fullMem := unsafe.Slice(&mem[0], allocSize)

	// Make writable again so we can zero it
	_ = unix.Mprotect(fullMem, unix.PROT_READ|unix.PROT_WRITE)

	// Zero the memory
	for i := range fullMem {
		fullMem[i] = 0
	}

	_ = unix.Munlock(fullMem)
	_ = unix.Munmap(fullMem)
}
