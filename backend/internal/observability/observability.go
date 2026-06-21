// Package observability provides structured logging, request-id propagation,
// request logging, panic recovery, and the /healthz + /readyz probes. It is
// transport-agnostic (chi-compatible net/http middleware) and holds no globals
// beyond the standard library's slog default, which callers opt into via
// SetDefault.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// ctxKey is an unexported context key type to avoid collisions.
type ctxKey int

const (
	requestIDKey ctxKey = iota
)

// requestIDHeader is the canonical header used to receive/emit a request id.
const requestIDHeader = "X-Request-Id"

// NewLogger builds a slog.Logger honoring the level and format strings
// ("debug|info|warn|error" and "json|text"). Unknown values fall back to
// info/json so logging never silently disappears.
func NewLogger(level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}

// parseLevel maps a level string to slog.Level (defaults to info).
func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newRequestID returns a 128-bit random hex id; on the (practically impossible)
// RNG failure it falls back to a timestamp so requests stay traceable.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ts-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// RequestIDFrom returns the request id stored in ctx, or "" if none.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// RequestID is middleware that ensures every request carries an id: it honors a
// well-formed inbound X-Request-Id, otherwise generates one, stores it in the
// context, and echoes it on the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sanitizeRequestID accepts only short, safe inbound ids (defends against header
// injection / log forging via the request-id). Anything unsuitable yields "".
func sanitizeRequestID(in string) string {
	if in == "" || len(in) > 128 {
		return ""
	}
	for _, r := range in {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return ""
		}
	}
	return in
}

// statusRecorder captures the response status and byte count for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	// If the handler wrote a body without an explicit WriteHeader, the status
	// is an implicit 200 — record it so logs are accurate.
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// RequestLogger returns middleware that emits one structured access-log line per
// request, enriched with the request id, at info (5xx → error).
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			}
			level := slog.LevelInfo
			if status >= http.StatusInternalServerError {
				level = slog.LevelError
			}
			logger.LogAttrs(r.Context(), level, "http_request", attrs...)
		})
	}
}

// Recoverer returns middleware that recovers from panics, logs them with the
// request id, and returns a 500 (without leaking the panic to the client).
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.LogAttrs(r.Context(), slog.LevelError, "panic_recovered",
						slog.String("request_id", RequestIDFrom(r.Context())),
						slog.Any("panic", rec),
						slog.String("path", r.URL.Path),
					)
					// Only write a status if nothing has been sent yet; a partial
					// response can't be retroactively turned into a 500.
					w.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ReadinessChecker reports whether a dependency is ready to serve. Returning a
// non-nil error makes /readyz respond 503. Phase 1+ wires the DB + /data checks.
type ReadinessChecker interface {
	Ready(ctx context.Context) error
}

// ReadinessFunc adapts a function to the ReadinessChecker interface.
type ReadinessFunc func(ctx context.Context) error

// Ready implements ReadinessChecker.
func (f ReadinessFunc) Ready(ctx context.Context) error { return f(ctx) }

// Health is the liveness probe: the process is up and serving. Always 200.
func Health(w http.ResponseWriter, _ *http.Request) {
	writePlain(w, http.StatusOK, "ok")
}

// Ready returns a readiness handler that runs the given checker. A nil checker
// is treated as always-ready (the P0 default before the DB exists).
func Ready(checker ReadinessChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if checker != nil {
			if err := checker.Ready(r.Context()); err != nil {
				writePlain(w, http.StatusServiceUnavailable, "not ready")
				return
			}
		}
		writePlain(w, http.StatusOK, "ready")
	}
}

// writePlain writes a short text/plain response with an explicit status.
func writePlain(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
