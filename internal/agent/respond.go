package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "AGENT_ERROR", err.Error())
		return
	}
	writeRaw(w, status, b)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	newPageError(status, code, message).write(w)
}

type ctxKey int

const requestIDKey ctxKey = 1

// requestID is sent to the server as X-Request-ID, so a kiosk call can be
// found in both logs.
func requestID(r *http.Request) string {
	v, _ := r.Context().Value(requestIDKey).(string)
	return v
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "vm-" + hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// middleware adds a request id, one log line per request and panic recovery.
// The page's polling and static files log at debug level only.
func (a *Agent) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := newRequestID()
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, rid))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		sw.Header().Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("panic", "err", p, "path", r.URL.Path, "stack", string(debug.Stack()))
				if !sw.wrote {
					writeError(sw, http.StatusInternalServerError, "AGENT_ERROR", "Terjadi kesalahan di mesin. Coba lagi.")
				}
			}
			level := slog.LevelDebug
			if strings.HasPrefix(r.URL.Path, "/api/") && !quiet(r) {
				level = slog.LevelInfo
			}
			a.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"ms", time.Since(start).Milliseconds(), "request_id", rid)
		}()
		next.ServeHTTP(sw, r)
	})
}

func quiet(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch r.URL.Path {
	case "/api/catalog", "/api/health", "/api/agent/status":
		return true
	}
	return false
}
