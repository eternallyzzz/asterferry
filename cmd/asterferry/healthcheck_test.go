package main

import "testing"

func TestParseHealthcheckURL(t *testing.T) {
	tests := []struct {
		name   string
		target string
		wantOK bool
	}{
		{name: "http", target: "http://127.0.0.1:8080/healthz", wantOK: true},
		{name: "https with whitespace", target: "  https://example.test/readyz  ", wantOK: true},
		{name: "relative", target: "/healthz"},
		{name: "wrong scheme", target: "ftp://example.test/healthz"},
		{name: "userinfo", target: "https://user:password@example.test/healthz"},
		{name: "missing host", target: "https:///healthz"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := parseHealthcheckURL(test.target)
			if (err == nil) != test.wantOK {
				t.Fatalf("parseHealthcheckURL(%q) = %v, %v", test.target, parsed, err)
			}
			if healthcheckURLIsSafe(test.target) != test.wantOK {
				t.Fatalf("healthcheckURLIsSafe(%q) disagrees", test.target)
			}
		})
	}
}
