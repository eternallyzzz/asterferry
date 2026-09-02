package controller

var testMasterKey = []byte("01234567890123456789012345678901")

func openTestStore(path string) (*Store, error) {
	return OpenStoreWithConfig(Config{DatabaseDriver: DatabaseDriverSQLite, DatabasePath: path}, testMasterKey)
}
