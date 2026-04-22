package cliapp

import (
	"fmt"
	"strconv"
	"strings"
)

type semver struct {
	major      int
	minor      int
	patch      int
	preRelease string
}

func parseSemver(value string) (semver, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return semver{}, fmt.Errorf("empty version")
	}

	base := trimmed
	preRelease := ""
	if idx := strings.Index(base, "+"); idx >= 0 {
		base = base[:idx]
	}
	if idx := strings.Index(base, "-"); idx >= 0 {
		preRelease = base[idx+1:]
		base = base[:idx]
	}

	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("invalid semver %q", value)
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver major %q", value)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver minor %q", value)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return semver{}, fmt.Errorf("invalid semver patch %q", value)
	}

	return semver{major: major, minor: minor, patch: patch, preRelease: preRelease}, nil
}

func compareSemver(current string, latest string) (int, error) {
	c, err := parseSemver(current)
	if err != nil {
		return 0, err
	}
	l, err := parseSemver(latest)
	if err != nil {
		return 0, err
	}

	if c.major != l.major {
		if c.major < l.major {
			return -1, nil
		}
		return 1, nil
	}
	if c.minor != l.minor {
		if c.minor < l.minor {
			return -1, nil
		}
		return 1, nil
	}
	if c.patch != l.patch {
		if c.patch < l.patch {
			return -1, nil
		}
		return 1, nil
	}

	if c.preRelease == l.preRelease {
		return 0, nil
	}
	if c.preRelease == "" {
		return 1, nil
	}
	if l.preRelease == "" {
		return -1, nil
	}
	if c.preRelease < l.preRelease {
		return -1, nil
	}
	if c.preRelease > l.preRelease {
		return 1, nil
	}
	return 0, nil
}

func isSemverUpgradeAvailable(current string, latest string) (bool, error) {
	cmp, err := compareSemver(current, latest)
	if err != nil {
		return false, err
	}
	return cmp < 0, nil
}
