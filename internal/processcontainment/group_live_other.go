//go:build !linux

package processcontainment

// groupHasNonZombieMember has no non-/proc probe on this platform.
// ok is always false so callers fall back to kill(-pgid, 0).
func groupHasNonZombieMember(pgid int) (hasLive bool, ok bool) {
	return false, false
}
