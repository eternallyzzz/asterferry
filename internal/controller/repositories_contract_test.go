package controller

import (
	"path/filepath"
	"testing"
)

func TestControllerRepositoriesSharePoolAndChangeBus(t *testing.T) {
	repositories, err := openTestRepositories(filepath.Join(t.TempDir(), "repositories.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repositories.Close()
	if repositories.Resources == nil || repositories.Runtime == nil || repositories.Changes == nil {
		t.Fatal("controller repositories were not fully composed")
	}
	if repositories.Resources.databaseHandle != repositories.Runtime.databaseHandle {
		t.Fatal("resource and runtime repositories do not share one database handle")
	}
	if repositories.Resources.ChangeBus() != repositories.Changes || repositories.Runtime.ChangeBus() != repositories.Changes {
		t.Fatal("resource and runtime repositories do not share the composition change bus")
	}
}
