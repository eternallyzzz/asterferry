package controller

import (
	_ "embed"
	"net/http"
)

// OpenAPISpec is embedded into the Controller binary so the documented API is
// available even when the source tree is not present at runtime. The canonical
// document is api/openapi.yaml; the checked-in copy is refreshed by the build
// workflow before compiling the binary.
//
//go:embed openapi.yaml
var OpenAPISpec string

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(OpenAPISpec))
}
