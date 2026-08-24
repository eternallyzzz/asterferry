package buildinfo

import "testing"

func TestCurrentUsesBuildMetadataAndProtocol(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, BuildDate
	Version, Commit, BuildDate = "test-version", "test-commit", "2026-01-01"
	t.Cleanup(func() { Version, Commit, BuildDate = oldVersion, oldCommit, oldDate })
	info := Current()
	if info.Version != "test-version" || info.Commit != "test-commit" || info.BuildDate != "2026-01-01" {
		t.Fatalf("unexpected build info: %#v", info)
	}
	if info.Protocol <= 0 || info.GoVersion == "" || info.OS == "" || info.Architecture == "" {
		t.Fatalf("unexpected runtime info: %#v", info)
	}
}

func TestCurrentFallsBackForEmptyMetadata(t *testing.T) {
	oldVersion, oldCommit, oldDate := Version, Commit, BuildDate
	Version, Commit, BuildDate = "", "", ""
	t.Cleanup(func() { Version, Commit, BuildDate = oldVersion, oldCommit, oldDate })
	info := Current()
	if info.Version != "dev" || info.Commit != "unknown" || info.BuildDate != "unknown" {
		t.Fatalf("unexpected fallback build info: %#v", info)
	}
}
