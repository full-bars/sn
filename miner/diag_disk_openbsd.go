//go:build openbsd

package miner

import "syscall"

func diskFreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return openbsdAvailableBytes(st.F_bavail, uint64(st.F_bsize)), nil
}

func openbsdAvailableBytes(blocks int64, blockSize uint64) uint64 {
	if blocks < 0 {
		return 0
	}
	return uint64(blocks) * blockSize
}
