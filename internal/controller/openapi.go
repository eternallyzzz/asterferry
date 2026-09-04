package controller

import (
	_ "embed"
	"net/http"
)

// OpenAPISpec is embedded into the Controller binary so the documented API is
// available even when the source tree is not present at runtime. The canonical
// document is internal/controller/openapi.yaml; refresh the generated public copy with
// `python scripts/sync-openapi.py` (or `go generate ./internal/controller`).
//
//go:generate python ../../scripts/sync-openapi.py
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
