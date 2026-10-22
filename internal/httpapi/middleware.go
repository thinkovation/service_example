package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const (
	requestIDKey contextKey = "request_id"
	loggerKey    contextKey = "logger"
)

// RequestIDHeader carries the correlation id in and out of the service.
const RequestIDHeader = "X-Request-Id"

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

type middleware func(http.Handler) http.Handler

// chain applies middlewares so that the first listed is the outermost.
func chain(h http.Handler, mws ...middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// withRequestID reuses an inbound X-Request-Id when present, so a correlation
// id set by a gateway survives into our logs, and echoes it back.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if len(id) == 0 || len(id) > 128 {
			id = uuid.NewString()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// withLogger puts a request-scoped logger in the context and logs the outcome
// of every request once it completes.
func withLogger(base *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			logger := base.With(
				slog.String("request_id", requestIDFrom(ctx)),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			ctx = context.WithValue(ctx, loggerKey, logger)

			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			logger.InfoContext(ctx, "request completed",
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// withRecovery turns a panic into a 500 rather than a dropped connection.
func withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				ctx := r.Context()
				loggerFrom(ctx).ErrorContext(ctx, "panic recovered",
					slog.Any("panic", rec), slog.String("stack", stack()))

				writeJSON(w, r, http.StatusInternalServerError, errorResponse{Error: errorDetail{
					Code:      codeInternal,
					Message:   "an unexpected error occurred",
					RequestID: requestIDFrom(ctx),
				}})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withTimeout bounds how long a handler may run. Handlers surface the expiry
// through their context, and asAPIError renders it as 504.
func withTimeout(d time.Duration) middleware {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusRecorder remembers what the handler wrote, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
	wrote   bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
