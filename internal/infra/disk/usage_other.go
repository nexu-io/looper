//go:build !unix

package disk

// statfsUsage is unavailable off unix; callers fail open on ErrUnsupported.
func statfsUsage(string) (Usage, error) {
	return Usage{}, ErrUnsupported
}
