package controller

import (
	"errors"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

const maxNodeInstallerAssetSize = 1 << 20

// nodeInstaller serves the small bootstrap assets installed by
// install-controller.sh. It deliberately does not expose the Controller data
// directory as a general file server.
func (s *Server) nodeInstaller(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	token := strings.TrimSpace(r.Header.Get(nodeEnrollmentTokenHeader))
	if err := s.resources.validateNodeBootstrapToken(r.Context(), token); err != nil {
		if isCredentialError(err) {
			writeError(w, http.StatusUnauthorized, "invalid_enrollment_token", "enrollment token is invalid, expired or already used")
			return
		}
		writeStoreError(w, err)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, nodeInstallersRoute)
	if name == "release" {
		name = nodeReleaseMetadataName
	}
	if name != bootstrapInstallerUnix && name != bootstrapInstallerWindows && name != nodeReleaseMetadataName {
		writeError(w, http.StatusNotFound, "not_found", "Node installer asset was not found")
		return
	}
	if name == nodeReleaseMetadataName {
		if _, err := loadNodeReleaseMetadata(s.config); err != nil {
			writeError(w, http.StatusServiceUnavailable, "node_release_unavailable", err.Error())
			return
		}
	}
	assetPath := nodeInstallerPath(s.config, name)
	info, err := os.Stat(assetPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "not_found", "Node installer asset was not found")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "node_release_unavailable", "Node installer asset is unavailable")
		return
	}
	if info.IsDir() || info.Size() > maxNodeInstallerAssetSize {
		writeError(w, http.StatusServiceUnavailable, "node_release_unavailable", "Node installer asset is invalid")
		return
	}
	data, err := os.ReadFile(assetPath)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "node_release_unavailable", "Node installer asset is unavailable")
		return
	}
	contentType := "text/plain; charset=utf-8"
	switch path.Ext(name) {
	case ".sh":
		contentType = "text/x-shellscript; charset=utf-8"
	case ".ps1":
		contentType = "text/plain; charset=utf-8"
	case ".json":
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", stringSize(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(data)
}

func stringSize(size int) string {
	// strconv.Itoa is kept behind this tiny helper so the handler's response
	// path stays visually focused on the asset policy.
	return strconv.Itoa(size)
}
