package controller

var testMasterKey = []byte("01234567890123456789012345678901")

func openTestStore(path string) (*ResourceRepository, error) {
	repositories, err := openTestRepositories(path)
	if err != nil {
		return nil, err
	}
	return repositories.Resources, nil
}

func openTestRepositories(path string) (*ControllerRepositories, error) {
	return OpenControllerRepositoriesWithConfig(Config{DatabaseDriver: DatabaseDriverSQLite, DatabasePath: path}, testMasterKey)
}

// Close is test-only cleanup for helpers that intentionally expose just the
// resource repository. Production code closes the composition root so the
// shared runtime repository and change bus are always shut down together.
func (s *ResourceRepository) Close() error {
	if s == nil || s.databaseHandle == nil {
		return nil
	}
	if s.changes != nil {
		s.changes.Close()
	}
	return s.databaseHandle.closeDatabase()
}

func schedulerForTest(repository *ResourceRepository) *Scheduler {
	scheduler, err := NewScheduler(repository, nil)
	if err != nil {
		panic(err)
	}
	return scheduler
}
