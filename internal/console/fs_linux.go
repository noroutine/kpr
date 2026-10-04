//go:build linux

package console

import "syscall"

// fsBlockUnit is the byte unit of Statfs Blocks/Bavail on Linux:
// the fragment size, not Bsize (the preferred I/O size, which
// virtiofs inflates to 1MB over 4K fragments).
func fsBlockUnit(fs *syscall.Statfs_t) uint64 {
	return uint64(fs.Frsize)
}
