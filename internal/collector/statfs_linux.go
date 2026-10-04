//go:build linux

package collector

import (
	"strconv"
	"syscall"

	"fncluster/internal/cluster"
)

func filesystem(path string) (cluster.Filesystem, string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return cluster.Filesystem{}, "", err
	}
	var pathStat syscall.Stat_t
	if err := syscall.Stat(path, &pathStat); err != nil {
		return cluster.Filesystem{}, "", err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	used := (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
	return cluster.Filesystem{Mount: path, TotalBytes: total, UsedBytes: used}, strconv.FormatUint(uint64(pathStat.Dev), 10), nil
}
