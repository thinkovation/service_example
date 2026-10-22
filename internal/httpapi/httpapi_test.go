package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/thinkovation/service_example/internal/customer"
	"github.com/thinkovation/service_example/internal/httpapi"
	"github.com/thinkovation/service_example/internal/store"
	"github.com/thinkovation/service_example/internal/store/sqlite"
)

func newServer(t *testing.T) *httpapi.Server {
	t.Helper()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := sqlite.Open(context.Background(), sqlite.Config{DSN: dsn, AutoMigrate: true})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return httpapi.New(httpapi.Options{
		Store:       db,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		ServiceName: "customer-service-test",
		Env:         "test",
	})
}

// do sends a request to the server and returns the recorded response.
func do(t *testing.T, srv *httpapi.Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		default:
			encoded, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("encode request body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
	}

	r := httptest.NewRequest(method, target, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return out
}

func assertStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", w.Code, want, w.Body.String())
	}
}

// errorBody mirrors the error envelope the service returns.
type errorBody struct {
	Error struct {
		Code      string                `json:"code"`
		Message   string                `json:"message"`
		Details   []customer.FieldError `json:"details"`
		RequestID string                `json:"request_id"`
	} `json:"error"`
}

func assertErrorCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errorBody {
	t.Helper()
	assertStatus(t, w, status)
	body := decode[errorBody](t, w)
	if body.Error.Code != code {
		t.Fatalf("error code = %q, want %q (body: %s)", body.Error.Code, code, w.Body.String())
	}
	return body
}

func createCustomer(t *testing.T, srv *httpapi.Server, name string, status customer.Status) customer.Customer {
	t.Helper()
	w := do(t, srv, http.MethodPost, "/customers", map[string]any{"name": name, "status": status})
	assertStatus(t, w, http.StatusCreated)
	return decode[customer.Customer](t, w)
}

func TestStatus(t *testing.T) {
	srv := newServer(t)
	w := do(t, srv, http.MethodGet, "/status", nil)
	assertStatus(t, w, http.StatusOK)

	body := decode[struct {
		Service       string  `json:"service"`
		Env           string  `json:"env"`
		Status        string  `json:"status"`
		UptimeSeconds float64 `json:"uptime_seconds"`
		Build         struct {
			Version   string `json:"version"`
			GoVersion string `json:"go_version"`
		} `json:"build"`
		Store struct {
			Driver string `json:"driver"`
		} `json:"store"`
	}](t, w)

	if body.Service != "customer-service-test" || body.Env != "test" || body.Status != "ok" {
		t.Errorf("status body = %+v, want the configured service identity", body)
	}
	if body.Store.Driver != sqlite.Driver {
		t.Errorf("store driver = %q, want %q", body.Store.Driver, sqlite.Driver)
	}
	if body.Build.GoVersion == "" {
		t.Error("build.go_version is empty")
	}
	if body.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %v, want >= 0", body.UptimeSeconds)
	}
}

func TestHealthOK(t *testing.T) {
	srv := newServer(t)
	w := do(t, srv, http.MethodGet, "/zhealth", nil)
	assertStatus(t, w, http.StatusOK)

	body := decode[struct {
		Status string `json:"status"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}](t, w)

	if body.Status != "ok" {
		t.Errorf("status = %q, want \"ok\"", body.Status)
	}
	if len(body.Checks) != 1 || body.Checks[0].Name != "database" || body.Checks[0].Status != "ok" {
		t.Errorf("checks = %+v, want a passing database check", body.Checks)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want \"no-store\"", got)
	}
}

// unhealthyStore fails its ping and is otherwise unused.
type unhealthyStore struct{ store.Store }

func (unhealthyStore) Driver() string { return "sqlite" }

func (unhealthyStore) Ping(context.Context) error { return errors.New("connection refused") }

func TestHealthReportsFailingDependency(t *testing.T) {
	srv := httpapi.New(httpapi.Options{
		Store:  unhealthyStore{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	w := do(t, srv, http.MethodGet, "/zhealth", nil)
	assertStatus(t, w, http.StatusServiceUnavailable)

	body := decode[struct {
		Status string `json:"status"`
		Checks []struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"checks"`
	}](t, w)

	if body.Status != "fail" {
		t.Errorf("status = %q, want \"fail\"", body.Status)
	}
	if len(body.Checks) != 1 || body.Checks[0].Status != "fail" || body.Checks[0].Error == "" {
		t.Errorf("checks = %+v, want a failing database check carrying the reason", body.Checks)
	}
}

func TestCreateCustomer(t *testing.T) {
	srv := newServer(t)

	w := do(t, srv, http.MethodPost, "/customers", map[string]any{"name": "  Acme Ltd  "})
	assertStatus(t, w, http.StatusCreated)

	created := decode[customer.Customer](t, w)
	if created.Name != "Acme Ltd" {
		t.Errorf("name = %q, want it trimmed to \"Acme Ltd\"", created.Name)
	}
	if created.Status != customer.DefaultStatus {
		t.Errorf("status = %q, want the default %q", created.Status, customer.DefaultStatus)
	}
	if created.ID.Version() != 7 {
		t.Errorf("id version = %d, want 7", created.ID.Version())
	}
	if created.CreateTS.IsZero() || !created.CreateTS.Equal(created.UpdateTS) {
		t.Errorf("timestamps = %s / %s, want both set and equal on create", created.CreateTS, created.UpdateTS)
	}
	if got, want := w.Header().Get("Location"), "/customers/"+created.ID.String(); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	t.Run("readable afterwards", func(t *testing.T) {
		got := do(t, srv, http.MethodGet, "/customers/"+created.ID.String(), nil)
		assertStatus(t, got, http.StatusOK)
		if fetched := decode[customer.Customer](t, got); fetched.ID != created.ID {
			t.Errorf("id = %s, want %s", fetched.ID, created.ID)
		}
	})
}

func TestCreateCustomerRejectsBadInput(t *testing.T) {
	srv := newServer(t)

	cases := []struct {
		name   string
		body   any
		status int
		code   string
	}{
		{"empty name", map[string]any{"name": "   "}, http.StatusBadRequest, "validation_failed"},
		{"missing name", map[string]any{"status": "active"}, http.StatusBadRequest, "validation_failed"},
		{"unknown status", map[string]any{"name": "Acme", "status": "retired"}, http.StatusBadRequest, "validation_failed"},
		{"over-long name", map[string]any{"name": strings.Repeat("x", customer.MaxNameLen+1)}, http.StatusBadRequest, "validation_failed"},
		{"unknown field", `{"name":"Acme","id":"123"}`, http.StatusBadRequest, "invalid_json"},
		{"malformed json", `{"name":`, http.StatusBadRequest, "invalid_json"},
		{"two json documents", `{"name":"a"}{"name":"b"}`, http.StatusBadRequest, "invalid_json"},
		{"no body", nil, http.StatusBadRequest, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, srv, http.MethodPost, "/customers", tc.body)
			assertErrorCode(t, w, tc.status, tc.code)
		})
	}
}

func TestCreateCustomerRejectsNonJSONContentType(t *testing.T) {
	srv := newServer(t)

	r := httptest.NewRequest(http.MethodPost, "/customers", strings.NewReader(`name=Acme`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)

	assertErrorCode(t, w, http.StatusUnsupportedMediaType, "unsupported_media_type")
}

func TestCreateCustomerRejectsOversizedBody(t *testing.T) {
	db, err := sqlite.Open(context.Background(), sqlite.Config{
		DSN: "file:oversized?mode=memory&cache=shared", AutoMigrate: true,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	srv := httpapi.New(httpapi.Options{
		Store:        db,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes: 64,
	})

	w := do(t, srv, http.MethodPost, "/customers",
		fmt.Sprintf(`{"name":%q}`, strings.Repeat("x", 512)))
	assertErrorCode(t, w, http.StatusRequestEntityTooLarge, "payload_too_large")
}

func TestGetCustomer(t *testing.T) {
	srv := newServer(t)

	t.Run("unknown id", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers/"+uuid.Must(uuid.NewV7()).String(), nil)
		assertErrorCode(t, w, http.StatusNotFound, "not_found")
	})

	t.Run("id that is not a uuid", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers/not-a-uuid", nil)
		assertErrorCode(t, w, http.StatusBadRequest, "invalid_id")
	})
}

func TestReplaceCustomer(t *testing.T) {
	srv := newServer(t)
	created := createCustomer(t, srv, "Acme Ltd", customer.StatusPending)

	t.Run("replaces an existing customer", func(t *testing.T) {
		w := do(t, srv, http.MethodPut, "/customers/"+created.ID.String(),
			map[string]any{"name": "Acme Limited", "status": "active"})
		assertStatus(t, w, http.StatusOK)

		replaced := decode[customer.Customer](t, w)
		if replaced.Name != "Acme Limited" || replaced.Status != customer.StatusActive {
			t.Errorf("customer = %+v, want the replacement values", replaced)
		}
		if !replaced.CreateTS.Equal(created.CreateTS) {
			t.Errorf("create_ts = %s, want it preserved at %s", replaced.CreateTS, created.CreateTS)
		}
		if replaced.UpdateTS.Before(created.UpdateTS) {
			t.Errorf("update_ts = %s, want it at or after %s", replaced.UpdateTS, created.UpdateTS)
		}
	})

	t.Run("creates when the id is unknown", func(t *testing.T) {
		id := uuid.Must(uuid.NewV7())
		w := do(t, srv, http.MethodPut, "/customers/"+id.String(),
			map[string]any{"name": "Fresh Co", "status": "active"})
		assertStatus(t, w, http.StatusCreated)

		got := decode[customer.Customer](t, w)
		if got.ID != id {
			t.Errorf("id = %s, want the requested %s", got.ID, id)
		}
		if want := "/customers/" + id.String(); w.Header().Get("Location") != want {
			t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
		}
	})

	t.Run("requires a complete body", func(t *testing.T) {
		w := do(t, srv, http.MethodPut, "/customers/"+created.ID.String(), map[string]any{"status": "active"})
		assertErrorCode(t, w, http.StatusBadRequest, "validation_failed")
	})
}

func TestPatchCustomer(t *testing.T) {
	srv := newServer(t)
	created := createCustomer(t, srv, "Acme Ltd", customer.StatusPending)

	t.Run("changes only what it sets", func(t *testing.T) {
		w := do(t, srv, http.MethodPatch, "/customers/"+created.ID.String(),
			map[string]any{"status": "suspended"})
		assertStatus(t, w, http.StatusOK)

		patched := decode[customer.Customer](t, w)
		if patched.Name != created.Name {
			t.Errorf("name = %q, want it untouched at %q", patched.Name, created.Name)
		}
		if patched.Status != customer.StatusSuspended {
			t.Errorf("status = %q, want %q", patched.Status, customer.StatusSuspended)
		}
	})

	t.Run("rejects an empty patch", func(t *testing.T) {
		w := do(t, srv, http.MethodPatch, "/customers/"+created.ID.String(), map[string]any{})
		assertErrorCode(t, w, http.StatusBadRequest, "validation_failed")
	})

	t.Run("rejects an unknown status", func(t *testing.T) {
		w := do(t, srv, http.MethodPatch, "/customers/"+created.ID.String(),
			map[string]any{"status": "retired"})
		assertErrorCode(t, w, http.StatusBadRequest, "validation_failed")
	})

	t.Run("unknown id", func(t *testing.T) {
		w := do(t, srv, http.MethodPatch, "/customers/"+uuid.Must(uuid.NewV7()).String(),
			map[string]any{"name": "Ghost"})
		assertErrorCode(t, w, http.StatusNotFound, "not_found")
	})
}

func TestListCustomers(t *testing.T) {
	srv := newServer(t)
	acme := createCustomer(t, srv, "Acme Ltd", customer.StatusActive)
	beta := createCustomer(t, srv, "Beta Industries", customer.StatusPending)

	t.Run("returns every customer", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers", nil)
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 2 || len(page.Items) != 2 {
			t.Fatalf("page = %+v, want 2 customers", page)
		}
		if page.Limit == 0 {
			t.Error("limit = 0, want the default page size")
		}
	})

	t.Run("filters by status", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers?status=pending", nil)
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 1 || page.Items[0].ID != beta.ID {
			t.Errorf("page = %+v, want only %s", page, beta.ID)
		}
	})

	t.Run("filters by name fragment", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers?name_contains=acme", nil)
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 1 || page.Items[0].ID != acme.ID {
			t.Errorf("page = %+v, want only %s", page, acme.ID)
		}
	})

	t.Run("pages", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers?limit=1&offset=1&sort=create_ts&order=asc", nil)
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 2 || len(page.Items) != 1 || page.Items[0].ID != beta.ID {
			t.Errorf("page = %+v, want the second customer only", page)
		}
	})

	t.Run("rejects bad parameters", func(t *testing.T) {
		for _, target := range []string{
			"/customers?limit=many",
			"/customers?limit=100000",
			"/customers?offset=-1",
			"/customers?sort=secret_column",
			"/customers?order=sideways",
			"/customers?status=retired",
			"/customers?created_after=yesterday",
			"/customers?id=not-a-uuid",
		} {
			t.Run(target, func(t *testing.T) {
				w := do(t, srv, http.MethodGet, target, nil)
				assertErrorCode(t, w, http.StatusBadRequest, "validation_failed")
			})
		}
	})
}

func TestQueryCustomers(t *testing.T) {
	srv := newServer(t)
	acme := createCustomer(t, srv, "Acme Ltd", customer.StatusActive)
	beta := createCustomer(t, srv, "Beta Industries", customer.StatusPending)
	createCustomer(t, srv, "Gamma Holdings", customer.StatusInactive)

	t.Run("QUERY with a filter body", func(t *testing.T) {
		w := do(t, srv, httpapi.MethodQuery, "/customers", map[string]any{
			"status": []string{"active", "pending"},
			"sort":   "name",
			"order":  "asc",
		})
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 2 || len(page.Items) != 2 {
			t.Fatalf("page = %+v, want 2 matches", page)
		}
		if page.Items[0].ID != acme.ID || page.Items[1].ID != beta.ID {
			t.Errorf("items = %s, %s; want %s, %s", page.Items[0].ID, page.Items[1].ID, acme.ID, beta.ID)
		}
	})

	t.Run("QUERY with no body matches everything", func(t *testing.T) {
		w := do(t, srv, httpapi.MethodQuery, "/customers", nil)
		assertStatus(t, w, http.StatusOK)

		if page := decode[customer.Page](t, w); page.Total != 3 {
			t.Errorf("total = %d, want 3", page.Total)
		}
	})

	t.Run("QUERY by id and time window", func(t *testing.T) {
		w := do(t, srv, httpapi.MethodQuery, "/customers", map[string]any{
			"ids":            []string{acme.ID.String(), beta.ID.String()},
			"created_before": beta.CreateTS.UTC().Format(time.RFC3339Nano),
		})
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 2 {
			t.Errorf("total = %d, want 2", page.Total)
		}
	})

	t.Run("POST alias behaves the same", func(t *testing.T) {
		w := do(t, srv, http.MethodPost, "/customers/query", map[string]any{"name_contains": "ltd"})
		assertStatus(t, w, http.StatusOK)

		page := decode[customer.Page](t, w)
		if page.Total != 1 || page.Items[0].ID != acme.ID {
			t.Errorf("page = %+v, want only %s", page, acme.ID)
		}
	})

	t.Run("rejects an invalid id", func(t *testing.T) {
		w := do(t, srv, httpapi.MethodQuery, "/customers", map[string]any{"ids": []string{"nope"}})
		body := assertErrorCode(t, w, http.StatusBadRequest, "validation_failed")
		if len(body.Error.Details) == 0 || body.Error.Details[0].Field != "ids" {
			t.Errorf("details = %+v, want a field error on \"ids\"", body.Error.Details)
		}
	})
}

func TestRoutingEdges(t *testing.T) {
	srv := newServer(t)
	created := createCustomer(t, srv, "Acme Ltd", customer.StatusActive)

	t.Run("unknown path", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/nope", nil)
		assertErrorCode(t, w, http.StatusNotFound, "not_found")
	})

	t.Run("unsupported method", func(t *testing.T) {
		w := do(t, srv, http.MethodDelete, "/customers/"+created.ID.String(), nil)
		assertErrorCode(t, w, http.StatusMethodNotAllowed, "method_not_allowed")

		allow := w.Header().Get("Allow")
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch} {
			if !strings.Contains(allow, method) {
				t.Errorf("Allow = %q, want it to list %s", allow, method)
			}
		}
	})

	t.Run("collection advertises QUERY", func(t *testing.T) {
		w := do(t, srv, http.MethodDelete, "/customers", nil)
		assertStatus(t, w, http.StatusMethodNotAllowed)
		if allow := w.Header().Get("Allow"); !strings.Contains(allow, httpapi.MethodQuery) {
			t.Errorf("Allow = %q, want it to list QUERY", allow)
		}
	})

	t.Run("OPTIONS", func(t *testing.T) {
		w := do(t, srv, http.MethodOptions, "/customers", nil)
		assertStatus(t, w, http.StatusNoContent)
		if allow := w.Header().Get("Allow"); !strings.Contains(allow, http.MethodPost) {
			t.Errorf("Allow = %q, want it to list POST", allow)
		}
	})

	t.Run("HEAD sends no body", func(t *testing.T) {
		w := do(t, srv, http.MethodHead, "/status", nil)
		assertStatus(t, w, http.StatusOK)
		if w.Body.Len() != 0 {
			t.Errorf("body = %q, want it empty for HEAD", w.Body.String())
		}
	})

	t.Run("request id is echoed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/status", nil)
		r.Header.Set(httpapi.RequestIDHeader, "test-correlation-id")
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)

		if got := w.Header().Get(httpapi.RequestIDHeader); got != "test-correlation-id" {
			t.Errorf("%s = %q, want it echoed", httpapi.RequestIDHeader, got)
		}
	})

	t.Run("request id is generated when absent", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/status", nil)
		if _, err := uuid.Parse(w.Header().Get(httpapi.RequestIDHeader)); err != nil {
			t.Errorf("%s = %q, want a generated UUID", httpapi.RequestIDHeader,
				w.Header().Get(httpapi.RequestIDHeader))
		}
	})

	t.Run("errors carry the request id", func(t *testing.T) {
		w := do(t, srv, http.MethodGet, "/customers/not-a-uuid", nil)
		body := assertErrorCode(t, w, http.StatusBadRequest, "invalid_id")
		if body.Error.RequestID == "" {
			t.Error("error.request_id is empty")
		}
	})
}
