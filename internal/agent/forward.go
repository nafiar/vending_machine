package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"vending-machine/internal/upstream"
)

// forward passes one page call to the server unchanged (body, operator token)
// and returns the server's answer unchanged, except that "server unreachable"
// and a refused machine key become 503 OFFLINE, which the page shows as
// "Mesin offline" (FR-10). after sees successful and error answers alike.
func (a *Agent) forward(method, path string, after func(*upstream.Response)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if method != http.MethodGet {
			b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				writeError(w, http.StatusBadRequest, "BAD_REQUEST", "Permintaan tidak terbaca.")
				return
			}
			body = b
		}
		header := http.Header{"X-Request-Id": {requestID(r)}}
		if v := r.Header.Get("Authorization"); v != "" {
			header.Set("Authorization", v)
		}
		ctx, cancel := context.WithTimeout(r.Context(), a.t.call)
		defer cancel()
		resp, err := a.srv.Do(ctx, method, path, body, header)
		if err != nil {
			a.markOffline(err)
			offlineError("").write(w)
			return
		}
		if resp.Status >= 400 {
			if apiErr := upstream.AsError(resp); keyProblem(apiErr) {
				a.noteAPIError(apiErr)
				offlineFor(apiErr).write(w)
				return
			}
		}
		a.markOnline()
		if after != nil {
			after(resp)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.Status)
		_, _ = w.Write(resp.Body)
	}
}

// handleLogin forwards the operator login (FR-12, FR-13). The body holds the
// password, so it is never logged.
func (a *Agent) handleLogin(w http.ResponseWriter, r *http.Request) {
	a.forward(http.MethodPost, "/api/machine/operator/login", func(resp *upstream.Response) {
		var who struct {
			Username string `json:"username"`
		}
		if resp.Status == http.StatusOK && json.Unmarshal(resp.Body, &who) == nil {
			a.log.Info("operator logged in", "username", who.Username)
		}
	})(w, r)
}

// handleRestock forwards a confirmed visit (FR-15). Results from earlier sales
// go first, so the server applies the visit on top of every sale before it.
// Nothing is queued here: offline, the page keeps the visit on screen and the
// operator confirms again (D1; an offline queue is future work).
func (a *Agent) handleRestock(w http.ResponseWriter, r *http.Request) {
	a.flushQuick(r.Context())
	a.forward(http.MethodPost, "/api/machine/restock-sessions", func(resp *upstream.Response) {
		if resp.Status < 300 {
			var s struct {
				ID             string `json:"id"`
				CatalogVersion int64  `json:"catalogVersion"`
			}
			_ = json.Unmarshal(resp.Body, &s)
			a.log.Info("restock visit saved", "visit", s.ID, "catalog_version", s.CatalogVersion)
			a.catalogChanged() // FR-02, FR-15: the kiosk reloads right after the visit
		}
	})(w, r)
}

// ---------------------------------------------------------------- errors for the page

// pageError is an error answer for the kiosk page: {"code", "message", ...}.
type pageError struct {
	status int
	body   []byte
}

func (e *pageError) write(w http.ResponseWriter) { writeRaw(w, e.status, e.body) }

func newPageError(status int, code, message string) *pageError {
	b, _ := json.Marshal(map[string]string{"code": code, "message": message})
	return &pageError{status: status, body: b}
}

// offlineError is 503 OFFLINE, optionally with the reason the server refuses this machine.
func offlineError(reason string, message ...string) *pageError {
	body := map[string]string{"code": "OFFLINE", "message": "Server tidak terjangkau."}
	if reason != "" {
		body["reason"] = reason
	}
	if len(message) > 0 && message[0] != "" {
		body["message"] = message[0]
	}
	b, _ := json.Marshal(body)
	return &pageError{status: http.StatusServiceUnavailable, body: b}
}

func offlineFor(e *upstream.APIError) *pageError { return offlineError(e.Code, e.Message) }

// keyProblem: the server refuses this machine (wrong key, or deactivated in the
// admin web), so the kiosk can't sell until an admin fixes it.
func keyProblem(e *upstream.APIError) bool {
	return e.Code == "INVALID_MACHINE_KEY" || e.Code == "MACHINE_INACTIVE"
}

// passAPIError hands a server refusal to the page as it came.
func (a *Agent) passAPIError(e *upstream.APIError) *pageError {
	if keyProblem(e) {
		return offlineFor(e)
	}
	body := e.Body
	if len(body) == 0 {
		b, _ := json.Marshal(map[string]string{"code": e.Code, "message": e.Message})
		body = b
	}
	return &pageError{status: e.Status, body: body}
}

func (a *Agent) writeUpstreamError(w http.ResponseWriter, err error) {
	if apiErr, ok := upstream.AsAPIError(err); ok {
		a.passAPIError(apiErr).write(w)
		return
	}
	if upstream.IsOffline(err) {
		offlineError("").write(w)
		return
	}
	writeError(w, http.StatusBadGateway, "AGENT_ERROR", err.Error())
}

// ---------------------------------------------------------------- connection state

type connState struct {
	mu          sync.Mutex
	online      bool
	lastContact time.Time
	lastError   string
	problem     string // INVALID_MACHINE_KEY or MACHINE_INACTIVE
	problemMsg  string
}

func (c *connState) isOnline() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online && c.problem == ""
}

func (a *Agent) markOnline() {
	c := &a.conn
	c.mu.Lock()
	was, hadProblem := c.online, c.problem
	c.online, c.lastContact, c.lastError, c.problem, c.problemMsg = true, a.now(), "", "", ""
	c.mu.Unlock()
	if !was || hadProblem != "" {
		a.log.Info("server reachable", "server", a.srv.BaseURL())
	}
}

func (a *Agent) markOffline(err error) {
	c := &a.conn
	c.mu.Lock()
	was := c.online
	c.online = false
	if err != nil {
		c.lastError = err.Error()
	}
	c.mu.Unlock()
	if was {
		a.log.Warn("server unreachable; the kiosk shows \"Mesin offline\"", "server", a.srv.BaseURL(), "err", err)
	}
}

// noteAPIError records an answer from the server: it is reachable, but it may
// be refusing this machine.
func (a *Agent) noteAPIError(e *upstream.APIError) {
	if !keyProblem(e) {
		a.markOnline()
		return
	}
	c := &a.conn
	c.mu.Lock()
	changed := c.problem != e.Code
	c.online, c.lastContact, c.problem, c.problemMsg = false, a.now(), e.Code, e.Message
	c.mu.Unlock()
	if changed {
		hint := "check machineKey in config.json"
		if e.Code == "MACHINE_INACTIVE" {
			hint = "activate the machine in the admin web"
		}
		a.log.Error("the server refuses this machine: "+hint, "code", e.Code, "message", e.Message)
	}
}
