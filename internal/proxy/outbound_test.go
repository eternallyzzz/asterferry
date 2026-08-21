package proxy

import "testing"

func TestTargetAddress(t *testing.T) {
	if got := (Target{Host: "example.com", Port: 443}).Address(); got != "example.com:443" {
		t.Fatalf("address: %q", got)
	}
	if got := (Target{Host: "2001:db8::1", Port: 443}).Address(); got != "[2001:db8::1]:443" {
		t.Fatalf("IPv6 address: %q", got)
	}
}

func TestValidatePath(t *testing.T) {
	if err := ValidatePath(PathDirect); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath(PathGateway); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePath("unknown"); err == nil {
		t.Fatal("unknown path should be rejected")
	}
}
