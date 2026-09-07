//go:build !linux && !windows

package systeminfo

func collectPlatform(string) (platformSnapshot, error) {
	return platformSnapshot{}, nil
}
