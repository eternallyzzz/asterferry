package node

import (
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestControllerIdentityRejectionIsTerminal(t *testing.T) {
	for _, test := range []struct {
		name         string
		err          error
		decommission bool
	}{
		{name: "decommissioned", err: status.Error(codes.PermissionDenied, "node is decommissioned"), decommission: true},
		{name: "not enrolled", err: status.Error(codes.PermissionDenied, "node is not enrolled"), decommission: true},
		{name: "disabled", err: status.Error(codes.PermissionDenied, "node is disabled"), decommission: false},
		{name: "revoked certificate", err: status.Error(codes.PermissionDenied, "certificate serial is not current"), decommission: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !isControllerIdentityRejection(test.err) {
				t.Fatal("identity rejection was not classified as terminal")
			}
			if got := isControllerDecommissioned(test.err); got != test.decommission {
				t.Fatalf("decommission classification = %v, want %v", got, test.decommission)
			}
		})
	}
	if isControllerIdentityRejection(status.Error(codes.Unavailable, "controller is down")) {
		t.Fatal("transport outage was incorrectly classified as an identity rejection")
	}
}

func TestDecommissionMarkerRequiresExplicitEnrollment(t *testing.T) {
	bootstrapPath := filepath.Join(t.TempDir(), "node-bootstrap.json")
	runtime := &Runtime{bootstrapPath: bootstrapPath}
	if err := runtime.persistDecommissionMarker(errors.New("node is not enrolled")); err != nil {
		t.Fatal(err)
	}
	marked, err := runtime.hasDecommissionMarker()
	if err != nil || !marked {
		t.Fatalf("marker state = %v, err=%v", marked, err)
	}
	if err := ClearDecommissionMarker(bootstrapPath); err != nil {
		t.Fatal(err)
	}
	marked, err = runtime.hasDecommissionMarker()
	if err != nil || marked {
		t.Fatalf("marker remained after explicit enrollment cleanup: %v, err=%v", marked, err)
	}
}
