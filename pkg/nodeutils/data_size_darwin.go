//go:build darwin

package nodeutils

import "syscall"

func filesystemFragmentSize(*syscall.Statfs_t) int64 {
	return 0
}
