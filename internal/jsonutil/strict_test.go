package jsonutil

import (
	"errors"
	"testing"
)

type strictFixture struct {
	Name string `json:"name"`
}

func TestDecodeStrict(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		wantErr     error
		wantFailure bool
	}{
		{name: "valid", data: `{"name":"asterferry"}`},
		{name: "whitespace", data: " {\"name\":\"asterferry\"} \n\t"},
		{name: "unknown field", data: `{"name":"asterferry","extra":true}`, wantFailure: true},
		{name: "trailing value", data: `{"name":"asterferry"}{}`, wantErr: ErrTrailingJSON},
		{name: "malformed trailing", data: `{"name":"asterferry"} nope`, wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value strictFixture
			err := DecodeStrict([]byte(test.data), &value)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("expected %v, got %v", test.wantErr, err)
				}
				return
			}
			if test.wantFailure {
				if err == nil {
					t.Fatal("invalid JSON was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
