package plugin

import "syscall"

func filesystemBlockSize(stat syscall.Statfs_t) int64 {
	if stat.Frsize != 0 {
		return int64(stat.Frsize)
	}
	return int64(stat.Bsize)
}
