//go:build !windows

package tools

import "syscall"

func cacheDirWritable(path string) bool {
	// Access checks the effective filesystem permissions without creating a
	// probe file in a user-provided cache. It also detects read-only mounts.
	return syscall.Access(path, 2) == nil
}
