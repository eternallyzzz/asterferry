package node

import (
	"asterferry/internal/atomicfile"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNodeDecommissioned tells the outer runtime loop that the Controller has
// issued a permanent lifecycle transition. It is different from a transport
// outage: a decommissioned identity must not retry forever.
var ErrNodeDecommissioned = errors.New("node has been decommissioned")

func isControllerIdentityRejection(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}

func isControllerDecommissioned(err error) bool {
	if errors.Is(err, ErrNodeDecommissioned) {
		return true
	}
	if status.Code(err) != codes.PermissionDenied {
		return false
	}
	message := strings.ToLower(status.Convert(err).Message())
	return strings.Contains(message, "decommissioned") || strings.Contains(message, "not enrolled")
}

func (r *Runtime) decommissionMarkerPath() string {
	if r == nil {
		return ""
	}
	if path := strings.TrimSpace(r.runtimeOpts.DecommissionMarkerPath); path != "" {
		return filepath.Clean(path)
	}
	if path := strings.TrimSpace(r.bootstrapPath); path != "" {
		return filepath.Clean(path) + ".decommissioned"
	}
	return ""
}

func (r *Runtime) hasDecommissionMarker() (bool, error) {
	path := r.decommissionMarkerPath()
	if path == "" {
		return false, nil
	}
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *Runtime) persistDecommissionMarker(cause error) error {
	path := r.decommissionMarkerPath()
	if path == "" {
		return nil
	}
	reason := "controller rejected this node identity"
	if cause != nil {
		reason = cause.Error()
	}
	content := fmt.Sprintf("decommissioned_at=%s\nreason=%s\n", time.Now().UTC().Format(time.RFC3339Nano), reason)
	return atomicfile.AtomicWrite(path, []byte(content), 0o600)
}

// ClearDecommissionMarker is called only after a successful explicit
// enrollment. It lets a replacement identity start without requiring a full
// binary or service uninstall.
func ClearDecommissionMarker(bootstrapPath string) error {
	path := strings.TrimSpace(bootstrapPath)
	if path == "" {
		return nil
	}
	err := os.Remove(filepath.Clean(path) + ".decommissioned")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
