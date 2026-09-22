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
	return usedBytesFromBlocks(
		uint64(stats.Blocks),
		uint64(stats.Bfree),
		int64(stats.Bsize),
		filesystemFragmentSize(&stats),
	)
}

func usedBytesFromBlocks(blocks, freeBlocks uint64, blockSize, fragmentSize int64) (int64, error) {
	if fragmentSize == 0 {
		fragmentSize = blockSize
	}
	if fragmentSize <= 0 {
		return 0, fmt.Errorf("invalid filesystem fragment size: %d", fragmentSize)
	}
	if freeBlocks > blocks {
		return 0, fmt.Errorf("invalid filesystem counters: %d free blocks exceed %d total blocks", freeBlocks, blocks)
	}

	usedBlocks := blocks - freeBlocks
	bytesPerBlock := uint64(fragmentSize)
	const maxInt64 = uint64(1<<63 - 1)
	if usedBlocks > maxInt64/bytesPerBlock {
		return 0, fmt.Errorf("filesystem used bytes overflow int64")
	}

	return int64(usedBlocks * bytesPerBlock), nil
}
