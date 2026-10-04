//go:build darwin

package console

import "syscall"

// fsBlockUnit is the byte unit of Statfs Blocks/Bavail on macOS:
// Bsize is the fundamental size there (no separate fragment size).
func fsBlockUnit(fs *syscall.Statfs_t) uint64 {
	return uint64(fs.Bsize)
}
