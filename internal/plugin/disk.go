package plugin

import "syscall"

func filesystemHasRoom(directory string, required uint64) (bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return false, err
	}
	size := filesystemBlockSize(stat)
	if size <= 0 {
		return false, syscall.EINVAL
	}
	blockSize := uint64(size)
	// Round up without multiplying the filesystem's block count.
	blocks := required / blockSize
	if required%blockSize != 0 {
		blocks++
	}
	return stat.Bavail >= blocks, nil
}
