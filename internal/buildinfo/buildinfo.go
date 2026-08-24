// Package buildinfo contains the small set of values surfaced by the CLI's
// version command. The defaults make local builds useful; release pipelines
// can replace them with -ldflags.
package buildinfo

import (
	"runtime"

	"asterferry/internal/protocol"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

type Info struct {
	Version      string
	Commit       string
	BuildDate    string
	Protocol     int
	GoVersion    string
	OS           string
	Architecture string
}

func Current() Info {
	return Info{
		Version:      valueOr(Version, "dev"),
		Commit:       valueOr(Commit, "unknown"),
		BuildDate:    valueOr(BuildDate, "unknown"),
		Protocol:     protocol.Version,
		GoVersion:    runtime.Version(),
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
	}
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
