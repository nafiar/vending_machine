package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"vending-machine/internal/board"
)

// The developer page (/dev/) reads this to show what the agent is doing, and
// switches the simulator (FR-29: success, motor timeout or no echo on demand).

type vendRecord struct {
	At     int64  `json:"at"`
	TxID   string `json:"txId"`
	Slot   int    `json:"slot"`
	Code   string `json:"code"`
	Status string `json:"status"`
	Reason string `json:"failureReason,omitempty"`
	AckMs  int64  `json:"ackMs,omitempty"`
	EndMs  int64  `json:"endMs,omitempty"`
}

type recentVends struct {
	mu   sync.Mutex
	list []vendRecord // newest first
}

func (r *recentVends) add(v vendRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append([]vendRecord{v}, r.list...)
	if len(r.list) > 20 {
		r.list = r.list[:20]
	}
}

func (r *recentVends) all() []vendRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]vendRecord{}, r.list...)
}

func (a *Agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.conn.mu.Lock()
	server := map[string]any{
		"url":       a.srv.BaseURL(),
		"online":    a.conn.online && a.conn.problem == "",
		"lastError": a.conn.lastError,
		"problem":   a.conn.problem,
		"message":   a.conn.problemMsg,
	}
	if !a.conn.lastContact.IsZero() {
		server["lastContactAt"] = a.conn.lastContact.UnixMilli()
	}
	a.conn.mu.Unlock()

	boardView := map[string]any{"kind": a.cfg.Board, "name": a.board.Describe(), "slotMap": a.cfg.Slots.String()}
	if a.sim != nil {
		boardView["simMode"] = a.sim.Mode()
		boardView["simModes"] = board.SimModes
		boardView["crashArmed"] = a.crashNext.Load()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":        a.version,
		"startedAt":      a.started.UnixMilli(),
		"listen":         a.cfg.Listen,
		"configFile":     a.cfg.Path,
		"dataDir":        a.cfg.DataDir,
		"server":         server,
		"machine":        a.cat.info(),
		"catalogVersion": a.cat.currentVersion(),
		"board":          boardView,
		"journal":        a.journal.List(),
		"recent":         a.recent.all(),
	})
}

// handleSim switches the simulator: {"mode": "ok" | "jam" | "no_ack"}, or
// {"mode": "crash"} to stop the agent right after the next vend command, as a
// power cut would (TC-37).
func (a *Agent) handleSim(w http.ResponseWriter, r *http.Request) {
	if a.sim == nil {
		writeError(w, http.StatusNotFound, "NOT_SIMULATOR", `The board is the real one ("board": "serial" in config.json).`)
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", `Send {"mode": "ok"}, "jam", "no_ack" or "crash".`)
		return
	}
	if in.Mode == "crash" {
		a.crashNext.Store(true)
		a.log.Warn("simulator: the agent will stop right after the next vend command (power cut test)")
	} else {
		if err := a.sim.SetMode(in.Mode); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "INVALID_INPUT", err.Error())
			return
		}
		a.crashNext.Store(false)
		a.log.Info("simulator mode changed", "mode", in.Mode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"simMode": a.sim.Mode(), "crashArmed": a.crashNext.Load()})
}
