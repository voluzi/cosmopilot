package nodeutils

import (
	"fmt"
	"syscall"
)

type statfsFunc func(string, *syscall.Statfs_t) error

func filesystemUsedBytes(path string, statfs statfsFunc) (int64, error) {
	if statfs == nil {
		return 0, fmt.Errorf("statfs is not configured")
	}

	var stats syscall.Statfs_t
	if err := statfs(path, &stats); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	if stats.Bsize <= 0 {
		return 0, fmt.Errorf("invalid filesystem block size: %d", stats.Bsize)
	}

	blocks := uint64(stats.Blocks)
	freeBlocks := uint64(stats.Bfree)
	if freeBlocks > blocks {
		return 0, fmt.Errorf("invalid filesystem counters: %d free blocks exceed %d total blocks", freeBlocks, blocks)
	}

	usedBlocks := blocks - freeBlocks
	blockSize := uint64(stats.Bsize)
	const maxInt64 = uint64(1<<63 - 1)
	if usedBlocks > maxInt64/blockSize {
		return 0, fmt.Errorf("filesystem used bytes overflow int64")
	}

	return int64(usedBlocks * blockSize), nil
}
