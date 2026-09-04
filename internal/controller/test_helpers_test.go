package controller

var testMasterKey = []byte("01234567890123456789012345678901")

func openTestStore(path string) (*Repository, error) {
	return OpenStoreWithConfig(Config{DatabaseDriver: DatabaseDriverSQLite, DatabasePath: path}, testMasterKey)
}

func schedulerForTest(repository *Repository) *Scheduler {
	scheduler, err := NewScheduler(repository, nil)
	if err != nil {
		panic(err)
	}
	return scheduler
}
