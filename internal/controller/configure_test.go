package controller

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitRequiresGRPCAdvertise(t *testing.T) {
	_, err := Init(context.Background(), InitOptions{Dir: filepath.Join(t.TempDir(), "controller"), Password: "a-very-long-admin-password"})
	if err == nil || !strings.Contains(err.Error(), "grpc_advertise") {
		t.Fatalf("Init without grpc_advertise error = %v", err)
	}
}

func TestConfigureGRPCAdvertisePreservesStateAndReissuesCertificate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "controller")
	initial, err := Init(context.Background(), InitOptions{
		Dir:           root,
		GRPCAdvertise: "127.0.0.1:9443",
		Password:      "a-very-long-admin-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldCA, err := os.ReadFile(initial.Config.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	oldMasterKey, err := os.ReadFile(initial.Config.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	oldDatabase, err := os.ReadFile(initial.Config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	oldTLSKey, err := os.ReadFile(initial.Config.TLSKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	oldTLSCert, err := os.ReadFile(initial.Config.TLSCertPath)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := Configure(context.Background(), ConfigureOptions{
		ConfigPath:    initial.ConfigPath,
		GRPCAdvertise: "172.28.80.1:9443",
		Now:           func() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.GRPCAdvertise != "172.28.80.1:9443" {
		t.Fatalf("updated grpc_advertise = %q", updated.GRPCAdvertise)
	}

	for name, item := range map[string]struct {
		want []byte
		path string
	}{
		"CA":       {want: oldCA, path: initial.Config.CACertPath},
		"master":   {want: oldMasterKey, path: initial.Config.MasterKeyPath},
		"database": {want: oldDatabase, path: initial.Config.DatabasePath},
	} {
		got, readErr := os.ReadFile(item.path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(got) != string(item.want) {
			t.Fatalf("%s changed while configuring grpc_advertise", name)
		}
	}
	newTLSKey, err := os.ReadFile(initial.Config.TLSKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	newTLSCert, err := os.ReadFile(initial.Config.TLSCertPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(newTLSKey) == string(oldTLSKey) || string(newTLSCert) == string(oldTLSCert) {
		t.Fatal("TLS identity was not reissued")
	}
	block, _ := pem.Decode(newTLSCert)
	if block == nil {
		t.Fatal("updated TLS certificate is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := certificate.VerifyHostname("172.28.80.1"); err != nil {
		t.Fatalf("updated TLS certificate does not cover advertised address: %v", err)
	}

	reloaded, err := LoadConfig(initial.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GRPCAdvertise != updated.GRPCAdvertise {
		t.Fatalf("reloaded grpc_advertise = %q, want %q", reloaded.GRPCAdvertise, updated.GRPCAdvertise)
	}
	instance, err := New(reloaded)
	if err != nil {
		t.Fatalf("updated Controller config was not startable: %v", err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControllerRejectsAdvertiseWithoutCertificateSAN(t *testing.T) {
	root := filepath.Join(t.TempDir(), "controller")
	initial, err := Init(context.Background(), InitOptions{
		Dir:           root,
		GRPCAdvertise: "127.0.0.1:9443",
		Password:      "a-very-long-admin-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	config := initial.Config
	config.GRPCAdvertise = "missing.example:9443"
	if err := SaveConfig(initial.ConfigPath, config); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(initial.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(reloaded)
	if err == nil || !strings.Contains(err.Error(), "does not cover grpc_advertise") {
		t.Fatalf("New certificate SAN validation error = %v", err)
	}
}

func TestConfigureRejectsUnspecifiedAdvertiseWithoutChangingFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "controller")
	initial, err := Init(context.Background(), InitOptions{
		Dir:           root,
		GRPCAdvertise: "127.0.0.1:9443",
		Password:      "a-very-long-admin-password",
	})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{initial.ConfigPath, initial.Config.TLSKeyPath, initial.Config.TLSCertPath}
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Configure(context.Background(), ConfigureOptions{ConfigPath: initial.ConfigPath, GRPCAdvertise: "0.0.0.0:9443"}); err == nil {
		t.Fatal("Configure accepted an unspecified address")
	}
	for _, path := range paths {
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(after) != string(before[path]) {
			t.Fatalf("%s changed after rejected configuration", path)
		}
	}
}

func TestConfigureRequiresConfigPath(t *testing.T) {
	_, err := Configure(context.Background(), ConfigureOptions{GRPCAdvertise: "127.0.0.1:9443"})
	if err == nil || !strings.Contains(err.Error(), "config path is required") {
		t.Fatalf("missing config path error = %v", err)
	}
}
