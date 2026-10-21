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

// diskUsage reports used and total bytes of the filesystem holding path.
//
// "Used" is what df reports: total minus the blocks available to an
// unprivileged caller, so the root-reserved slice counts as used. That is the
// figure a customer compares against Finder, and disagreeing with Finder is
// how a dashboard loses trust over a number nobody can act on anyway.
func diskUsage(path string) (used, total int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	//nolint:unconvert // Bsize is uint32 on darwin, int64 on Linux
	block := int64(stat.Bsize)
	total = int64(stat.Blocks) * block
	used = total - int64(stat.Bavail)*block
	if used < 0 {
		used = 0
	}
	return used, total
}
