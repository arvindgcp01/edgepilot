package collect

import "syscall"

// diskTotalBytes reports the size of the filesystem holding path.
//
// Zero on failure rather than an error: disk size is one fact among many, and a
// device whose filesystem stat fails is still worth managing.
//
// The conversions are not redundant even where a linter says so. Statfs_t's
// Bsize is uint32 on darwin and int64 on Linux, so exactly one of the two
// platforms this builds for needs each of them.
func diskTotalBytes(path string) int64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0
	}
	//nolint:unconvert // Bsize is uint32 on darwin, int64 on Linux
	return int64(stat.Blocks) * int64(stat.Bsize)
}
