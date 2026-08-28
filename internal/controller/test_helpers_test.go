package controller

var testMasterKey = []byte("01234567890123456789012345678901")

func openTestStore(path string) (*Store, error) {
	return OpenStore(path, testMasterKey)
}
