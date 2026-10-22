// Package httpapi exposes the customer service over HTTP: routing, request
// decoding, and the JSON shapes clients see.
package httpapi

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/thinkovation/service_example/internal/store"
)

// MethodQuery is the HTTP QUERY method (draft-ietf-httpbis-safe-method-w-body):
// a safe, idempotent read that carries its parameters in the request body.
const MethodQuery = "QUERY"

// Options configures a Server. Store and Logger are required; the rest have
// defaults applied by New.
type Options struct {
	Store  store.Store
	Logger *slog.Logger

	ServiceName string
	Env         string

	// MaxBodyBytes caps the size of a request body.
	MaxBodyBytes int64
	// RequestTimeout bounds a single request; zero disables the bound.
	RequestTimeout time.Duration
	// HealthTimeout bounds the dependency checks run by /zhealth.
	HealthTimeout time.Duration

	DefaultPageSize int
	MaxPageSize     int

	// Clock is injectable so tests can pin /status output.
	Clock func() time.Time
}

// Server is the HTTP surface of the service. It implements http.Handler.
type Server struct {
	opts    Options
	handler http.Handler
	started time.Time
}

// New builds the router and middleware stack.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "customer-service"
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.DefaultPageSize <= 0 {
		opts.DefaultPageSize = 50
	}
	if opts.MaxPageSize <= 0 {
		opts.MaxPageSize = 500
	}
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = 2 * time.Second
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}

	s := &Server{opts: opts, started: opts.Clock()}
	s.handler = chain(s.routes(),
		withRequestID,
		withLogger(opts.Logger),
		withRecovery,
		withTimeout(opts.RequestTimeout),
	)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/status", methodRouter{http.MethodGet: s.handleStatus})
	mux.Handle("/zhealth", methodRouter{http.MethodGet: s.handleHealth})

	mux.Handle("/customers", methodRouter{
		http.MethodGet:  s.handleListCustomers,
		http.MethodPost: s.handleCreateCustomer,
		MethodQuery:     s.handleQueryCustomers,
	})
	// POST alias for clients, proxies, or runtimes that cannot send QUERY.
	mux.Handle("/customers/query", methodRouter{
		http.MethodPost: s.handleQueryCustomers,
		MethodQuery:     s.handleQueryCustomers,
	})
	mux.Handle("/customers/{id}", methodRouter{
		http.MethodGet:   s.handleGetCustomer,
		http.MethodPut:   s.handleReplaceCustomer,
		http.MethodPatch: s.handlePatchCustomer,
	})

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, newAPIError(http.StatusNotFound, codeNotFound,
			"no route matches "+r.Method+" "+r.URL.Path))
	}))
	return mux
}

// methodRouter dispatches one path across methods. net/http's own method
// patterns would do most of this, but routing here keeps QUERY on the same
// footing as the built-in methods and lets 405 carry the JSON error envelope.
type methodRouter map[string]http.HandlerFunc

func (m methodRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	if method == http.MethodHead {
		// HEAD is served by the GET handler; writeJSON omits the body.
		method = http.MethodGet
	}
	if h, ok := m[method]; ok {
		h(w, r)
		return
	}

	allow := m.allowed()
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", allow)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Allow", allow)
	writeError(w, r, newAPIError(http.StatusMethodNotAllowed, codeMethodNotAllowed,
		r.Method+" is not supported on this resource; allowed: "+allow))
}

func (m methodRouter) allowed() string {
	methods := make([]string, 0, len(m)+2)
	for method := range m {
		methods = append(methods, method)
		if method == http.MethodGet {
			methods = append(methods, http.MethodHead)
		}
	}
	methods = append(methods, http.MethodOptions)
	sort.Strings(methods)
	return strings.Join(methods, ", ")
}
