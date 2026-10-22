package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/thinkovation/service_example/api"
	"github.com/thinkovation/service_example/internal/httpapi"
)

func TestSwaggerRedirectsToTheDocs(t *testing.T) {
	srv := newServer(t)

	w := do(t, srv, http.MethodGet, "/swagger", nil)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMovedPermanently)
	}
	if got := w.Header().Get("Location"); got != "/swagger/" {
		t.Errorf("Location = %q, want \"/swagger/\"", got)
	}
}

func TestSwaggerUIPage(t *testing.T) {
	srv := newServer(t)

	w := do(t, srv, http.MethodGet, "/swagger/", nil)
	assertStatus(t, w, http.StatusOK)

	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}

	body := w.Body.String()
	// The page must load its assets from this service, not from a CDN.
	for _, want := range []string{`id="swagger-ui"`, `src="swagger-ui-bundle.js"`, `href="swagger-ui.css"`, `url: "openapi.yaml"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, "//unpkg.com") || strings.Contains(body, "//cdn.") {
		t.Error("page loads an asset from a CDN; the assets are vendored for a reason")
	}
}

func TestSwaggerAssetsAreServed(t *testing.T) {
	srv := newServer(t)

	cases := []struct {
		path        string
		contentType string
	}{
		{"/swagger/swagger-ui.css", "text/css"},
		{"/swagger/swagger-ui-bundle.js", "text/javascript"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			w := do(t, srv, http.MethodGet, tc.path, nil)
			assertStatus(t, w, http.StatusOK)

			if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
				t.Errorf("Content-Type = %q, want %s", got, tc.contentType)
			}
			if w.Body.Len() == 0 {
				t.Error("asset is empty")
			}
		})
	}
}

func TestOpenAPISpecIsServed(t *testing.T) {
	srv := newServer(t)

	w := do(t, srv, http.MethodGet, "/swagger/openapi.yaml", nil)
	assertStatus(t, w, http.StatusOK)

	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml", got)
	}

	var spec map[string]any
	if err := yaml.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("served document is not valid YAML: %v", err)
	}
	if version, _ := spec["openapi"].(string); !strings.HasPrefix(version, "3.") {
		t.Errorf("openapi = %v, want a 3.x version", spec["openapi"])
	}
}

func TestDocsRejectsUnknownPathsAndMethods(t *testing.T) {
	srv := newServer(t)

	t.Run("unknown file", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/swagger/../main.go", nil)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMovedPermanently {
			t.Errorf("status = %d, want 404 or a redirect, never the file", w.Code)
		}
	})

	t.Run("unknown asset", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/swagger/swagger-ui-standalone-preset.js", nil)
		assertErrorCode(t, w, http.StatusNotFound, "not_found")
	})

	t.Run("wrong method", func(t *testing.T) {
		w := do(t, srv, http.MethodPost, "/swagger/", nil)
		assertErrorCode(t, w, http.StatusMethodNotAllowed, "method_not_allowed")
	})
}

// openAPIDocument is the part of the spec these tests read.
type openAPIDocument struct {
	Info struct {
		Description string `yaml:"description"`
	} `yaml:"info"`
	Paths map[string]map[string]any `yaml:"paths"`
}

// TestSpecMatchesRoutes is the guard against the hand-written spec drifting
// from the router. It checks both directions: everything documented is
// routable, and every route is documented.
func TestSpecMatchesRoutes(t *testing.T) {
	srv := newServer(t)

	var spec openAPIDocument
	if err := yaml.Unmarshal(api.OpenAPISpec, &spec); err != nil {
		t.Fatalf("parse the embedded spec: %v", err)
	}

	// The routes the service serves, as registered in server.go. QUERY
	// /customers is absent because OpenAPI 3.0 cannot express a method outside
	// its fixed list; TestSpecDescribesTheQueryMethod covers it instead.
	wantDocumented := map[string]bool{
		"GET /status":               true,
		"GET /zhealth":              true,
		"GET /customers":            true,
		"POST /customers":           true,
		"POST /customers/query":     true,
		"GET /customers/{id}":       true,
		"PUT /customers/{id}":       true,
		"PATCH /customers/{id}":     true,
		"GET /swagger/":             true,
		"GET /swagger/openapi.yaml": true,
	}

	documented := map[string]bool{}
	for path, operations := range spec.Paths {
		for method := range operations {
			switch method {
			case "get", "post", "put", "patch", "delete", "head", "options":
			default:
				continue // parameters, summary, and the like
			}
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}

	for operation := range wantDocumented {
		if !documented[operation] {
			t.Errorf("%s is served but not described in api/openapi.yaml", operation)
		}
	}
	for operation := range documented {
		if !wantDocumented[operation] {
			t.Errorf("%s is described in api/openapi.yaml but is not a route (or this test is stale)", operation)
		}
	}

	// Every documented operation must actually be routable. OPTIONS reports
	// what a path accepts, so this catches a path documented with the wrong
	// method as well as one that does not exist.
	for operation := range documented {
		method, path, _ := strings.Cut(operation, " ")
		probe := strings.ReplaceAll(path, "{id}", uuid.Must(uuid.NewV7()).String())

		t.Run(operation, func(t *testing.T) {
			w := do(t, srv, http.MethodOptions, probe, nil)
			if w.Code != http.StatusNoContent {
				t.Fatalf("OPTIONS %s = %d, want %d: the documented path is not routed",
					probe, w.Code, http.StatusNoContent)
			}
			if allow := w.Header().Get("Allow"); !strings.Contains(allow, method) {
				t.Errorf("OPTIONS %s allows %q, which does not include the documented %s",
					probe, allow, method)
			}
		})
	}
}

// TestSpecDescribesTheQueryMethod makes sure the one endpoint OpenAPI 3.0
// cannot express is still described in prose rather than quietly dropped.
func TestSpecDescribesTheQueryMethod(t *testing.T) {
	var spec openAPIDocument
	if err := yaml.Unmarshal(api.OpenAPISpec, &spec); err != nil {
		t.Fatalf("parse the embedded spec: %v", err)
	}

	if !strings.Contains(spec.Info.Description, httpapi.MethodQuery+" /customers") {
		t.Errorf("the spec description does not mention %s /customers, so the endpoint is undocumented",
			httpapi.MethodQuery)
	}
}
