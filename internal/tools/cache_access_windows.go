//go:build windows

package tools

import "os"

func cacheDirWritable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir() && info.Mode().Perm()&0o222 != 0
}
