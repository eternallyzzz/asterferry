// Package buildinfo contains the small set of values surfaced by the CLI's
// version command. The defaults make local builds useful; release pipelines
// can replace them with -ldflags.
package buildinfo

import (
	"runtime"

	"asterferry/internal/wireversion"
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
	Protocol     string
	GoVersion    string
	OS           string
	Architecture string
}

func Current() Info {
	return Info{
		Version:      valueOr(Version, "dev"),
		Commit:       valueOr(Commit, "unknown"),
		BuildDate:    valueOr(BuildDate, "unknown"),
		Protocol:     wireversion.Display,
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
