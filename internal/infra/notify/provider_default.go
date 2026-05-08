//go:build !darwin && !linux && !windows

package notify

func defaultPlatformProviders() []Provider {
	return []Provider{}
}
