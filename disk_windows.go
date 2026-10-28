//go:build windows

package main

// diskSpace is not measured on Windows; the page then skips the free-space check.
func diskSpace(string) (free, total uint64) { return 0, 0 }
