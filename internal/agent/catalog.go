package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"vending-machine/internal/upstream"
)

// catalogCache is FR-02: the catalog and its version, kept in memory and
// reloaded at boot, after each sale and each operator visit, daily at 03:00,
// and when the server reports a newer version. The kiosk page reads it from
// here, so the home screen appears without waiting for the server (NFR-01).
type catalogCache struct {
	mu        sync.Mutex // held during a reload, so callers share one fetch
	body      []byte
	version   int64
	machine   machineInfo
	fetchedAt time.Time
	dirty     bool
}

type machineInfo struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

func (c *catalogCache) currentVersion() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *catalogCache) info() machineInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.machine
}

func (c *catalogCache) loaded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body != nil
}

func (c *catalogCache) storeLocked(body []byte, now time.Time) error {
	var head struct {
		CatalogVersion int64           `json:"catalogVersion"`
		Machine        machineInfo     `json:"machine"`
		Slots          json.RawMessage `json:"slots"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("the server's catalog is not readable: %w", err)
	}
	if head.Slots == nil {
		return errors.New("the server's catalog has no slots")
	}
	c.body = append([]byte(nil), body...)
	c.version, c.machine, c.fetchedAt, c.dirty = head.CatalogVersion, head.Machine, now, false
	return nil
}

func (a *Agent) handleCatalog(w http.ResponseWriter, r *http.Request) {
	body, err := a.catalog(r.Context(), false)
	if err != nil {
		a.writeUpstreamError(w, err)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// catalog returns the cached catalog, or reloads it when it is missing, marked
// stale, older than the last 03:00, or when the server was last seen offline
// (the kiosk doesn't sell offline: D1, FR-10).
func (a *Agent) catalog(ctx context.Context, force bool) ([]byte, error) {
	c := &a.cat
	c.mu.Lock()
	defer c.mu.Unlock()
	now := a.now()
	if !force && c.body != nil && !c.dirty && a.conn.isOnline() && !passed0300(c.fetchedAt, now) {
		return c.body, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.t.catalog)
	defer cancel()
	resp, err := a.srv.Do(cctx, http.MethodGet, "/api/machine/catalog", nil, nil)
	if err != nil {
		a.markOffline(err)
		return nil, err
	}
	if resp.Status != http.StatusOK {
		apiErr := upstream.AsError(resp)
		a.noteAPIError(apiErr)
		return nil, apiErr
	}
	a.markOnline()
	if err := c.storeLocked(resp.Body, now); err != nil {
		a.log.Error("catalog", "err", err)
		return nil, err
	}
	return c.body, nil
}

// storeCatalog keeps the fresh catalog a CATALOG_STALE refusal carries.
func (a *Agent) storeCatalog(body []byte) error {
	a.cat.mu.Lock()
	defer a.cat.mu.Unlock()
	return a.cat.storeLocked(body, a.now())
}

// catalogChanged marks the catalog stale and reloads it in the background, so
// the page's next request is quick.
func (a *Agent) catalogChanged() {
	a.cat.mu.Lock()
	a.cat.dirty = true
	a.cat.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = a.catalog(ctx, false)
	}()
}

// passed0300 reports whether a 03:00 local time lies between fetched and now.
func passed0300(fetched, now time.Time) bool {
	boundary := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
	if now.Before(boundary) {
		boundary = boundary.AddDate(0, 0, -1)
	}
	return fetched.Before(boundary)
}

// ---------------------------------------------------------------- heartbeat (FR-28)

// heartbeat records this machine's contact time on the server and learns the
// current catalog version.
func (a *Agent) heartbeat(ctx context.Context) (int64, error) {
	var hb struct {
		OK             bool  `json:"ok"`
		ServerTime     int64 `json:"serverTime"`
		CatalogVersion int64 `json:"catalogVersion"`
	}
	cctx, cancel := context.WithTimeout(ctx, a.t.health)
	defer cancel()
	if _, err := a.srv.JSON(cctx, http.MethodPost, "/api/machine/heartbeat", struct{}{}, nil, &hb); err != nil {
		if apiErr, ok := upstream.AsAPIError(err); ok {
			a.noteAPIError(apiErr)
		} else {
			a.markOffline(err)
		}
		return 0, err
	}
	a.markOnline()
	if a.cat.loaded() && hb.CatalogVersion != a.cat.currentVersion() {
		a.log.Info("the server has a newer catalog; reloading", "catalog_version", hb.CatalogVersion)
		a.catalogChanged()
	}
	return hb.CatalogVersion, nil
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	tick := time.NewTicker(a.cfg.Heartbeat())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, _ = a.heartbeat(ctx)
		}
	}
}

// handleHealth is the page's connection check: while "Mesin offline" shows, and
// when a product is tapped. It asks the server live.
func (a *Agent) handleHealth(w http.ResponseWriter, r *http.Request) {
	v, err := a.heartbeat(r.Context())
	if err != nil {
		a.writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "catalogVersion": v, "board": a.board.Describe(), "pending": a.journal.Len()})
}
