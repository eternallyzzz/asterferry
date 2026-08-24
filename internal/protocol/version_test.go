package protocol

import "testing"

func TestVersionConstants(t *testing.T) {

	if Version != 4 || RelayRecordVersion != 2 || ObfuscationVersion != 2 {
		t.Fatalf("unexpected protocol versions: version=%d relay=%d obfuscation=%d", Version, RelayRecordVersion, ObfuscationVersion)
	}
	if AuthDomain == "" || DatagramMaskDomain == "" || DatagramTagDomain == "" {
		t.Fatal("protocol domains must not be empty")
	}
}
