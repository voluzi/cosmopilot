//go:build linux

package nodeutils

import "syscall"

func filesystemFragmentSize(stats *syscall.Statfs_t) int64 {
	return int64(stats.Frsize)
}
