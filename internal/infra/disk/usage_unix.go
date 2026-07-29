//go:build unix

package disk

import "golang.org/x/sys/unix"

// statfsUsage computes filesystem capacity via statfs(2). Field widths differ
// across platforms (darwin Bsize is uint32, linux int64), so every field is
// widened to uint64 before arithmetic.
func statfsUsage(path string) (Usage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Usage{}, err
	}

	blockSize := uint64(st.Bsize)
	total := uint64(st.Blocks) * blockSize
	avail := uint64(st.Bavail) * blockSize
	free := uint64(st.Bfree) * blockSize

	var used uint64
	if total > free {
		used = total - free
	}

	// df capacity treats root-reserved blocks as unavailable: the denominator is
	// used + available-to-us, not the raw total, so a full-to-a-normal-user disk
	// reads as ~100% even though a few root-only blocks remain.
	var usedPercent float64
	if denom := used + avail; denom > 0 {
		usedPercent = float64(used) / float64(denom) * 100
	}

	return Usage{
		TotalBytes:  total,
		AvailBytes:  avail,
		UsedBytes:   used,
		UsedPercent: usedPercent,
	}, nil
}
