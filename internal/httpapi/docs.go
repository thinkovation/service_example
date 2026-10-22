package httpapi

import (
	"bytes"
	"embed"
	"fmt"
	"net/http"

	"github.com/thinkovation/service_example/api"
)

// DocsPath is the prefix the documentation is served under.
const DocsPath = "/swagger/"

// Swagger UI, vendored rather than pulled from a CDN at page load: the docs
// then work on a machine with no internet access, and nothing third-party is
// executed from this service's own origin. See swaggerui/README.md for the
// version and how to update it.
//
//go:embed swaggerui/index.html swaggerui/swagger-ui.css swaggerui/swagger-ui-bundle.js
var swaggerUI embed.FS

// Read once at startup rather than per request: these are over a megabyte and
// embed.FS hands back a copy each time.
var (
	docsIndexHTML = mustReadAsset("swaggerui/index.html")
	docsCSS       = mustReadAsset("swaggerui/swagger-ui.css")
	docsJS        = mustReadAsset("swaggerui/swagger-ui-bundle.js")
)

func mustReadAsset(name string) []byte {
	content, err := swaggerUI.ReadFile(name)
	if err != nil {
		// The files are embedded at build time, so this cannot fail in a
		// binary that compiled.
		panic(fmt.Sprintf("httpapi: read embedded asset %q: %v", name, err))
	}
	return content
}

// handleDocs serves the documentation: the Swagger UI page, the OpenAPI
// description it reads, and the two assets the page needs.
//
// The paths are an explicit list rather than a file server over the embedded
// directory, so nothing but these four files can ever be reached.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case DocsPath, DocsPath + "index.html":
		s.serveAsset(w, r, "index.html", "text/html; charset=utf-8", docsIndexHTML)

	case DocsPath + "openapi.yaml":
		// Set explicitly: Go's built-in MIME table has no entry for .yaml, so
		// in a container with no /etc/mime.types this would be sniffed as
		// text/plain.
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		// The description changes with the deployment, so it is not cached as
		// aggressively as the assets.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "openapi.yaml", s.started, bytes.NewReader(api.OpenAPISpec))

	case DocsPath + "swagger-ui.css":
		s.serveAsset(w, r, "swagger-ui.css", "text/css; charset=utf-8", docsCSS)

	case DocsPath + "swagger-ui-bundle.js":
		s.serveAsset(w, r, "swagger-ui-bundle.js", "text/javascript; charset=utf-8", docsJS)

	default:
		writeError(w, r, newAPIError(http.StatusNotFound, codeNotFound,
			"no documentation at "+r.URL.Path))
	}
}

// serveAsset writes one embedded file. http.ServeContent handles conditional
// requests, so a reload of the page does not resend the bundle.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name, contentType string, content []byte) {
	w.Header().Set("Content-Type", contentType)
	if name != "index.html" {
		// The assets only change when the binary does.
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, name, s.started, bytes.NewReader(content))
}
