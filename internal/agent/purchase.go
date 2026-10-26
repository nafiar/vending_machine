package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"vending-machine/internal/board"
	"vending-machine/internal/journal"
	"vending-machine/internal/upstream"
)

// Transaction states (PRD, "Purchase sequence and transaction states").
const (
	statusPending   = "PENDING"
	statusPaid      = "PAID"
	statusDispensed = "DISPENSED"
	statusFailed    = "FAILED"
)

// Failure reasons this agent adds to the board's (NO_ACK, MOTOR_TIMEOUT, PORT_ERROR, UNKNOWN_SLOT).
const (
	reasonAgentRestart = "AGENT_RESTART" // BR-14: restarted after the vend command may have gone out
	reasonPayNoReply   = "PAY_NO_REPLY"  // the server took the payment but its answer never arrived, so the coil never turned
	reasonJournalError = "JOURNAL_ERROR" // the journal could not be written, so the coil was not turned
)

// txState is a transaction as the server returns it.
type txState struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	FailureReason *string `json:"failureReason"`
	SlotNumber    int     `json:"slotNumber"` // sent by servers that include it; else the agent's own record is used
}

// payReply is what the kiosk page reads after "Konfirmasi bayar".
type payReply struct {
	ID            string  `json:"id"`
	Status        string  `json:"status"`
	FailureReason *string `json:"failureReason"`
}

func reply(id, status, reason string) payReply {
	r := payReply{ID: id, Status: status}
	if reason != "" {
		r.FailureReason = &reason
	}
	return r
}

// ---------------------------------------------------------------- create

// handleCreate forwards "Beli". The agent remembers each new transaction's
// slot from the server's answer, and keeps the fresh catalog that comes with
// a CATALOG_STALE refusal (FR-02, FR-04).
func (a *Agent) handleCreate(w http.ResponseWriter, r *http.Request) {
	a.flushQuick(r.Context()) // results from earlier sales first, so counts and blocks are current
	a.forward(http.MethodPost, "/api/machine/transactions", func(resp *upstream.Response) {
		switch {
		case resp.Status == http.StatusOK || resp.Status == http.StatusCreated:
			var c struct {
				ID         string `json:"id"`
				SlotNumber int    `json:"slotNumber"`
				Amount     int64  `json:"amount"`
				Product    struct {
					Name string `json:"name"`
				} `json:"product"`
			}
			if json.Unmarshal(resp.Body, &c) == nil {
				a.slots.put(c.ID, c.SlotNumber, a.now())
				a.log.Info("transaction created", "tx", c.ID, "slot", c.SlotNumber, "product", c.Product.Name, "amount", c.Amount)
			}
		case resp.Status == http.StatusConflict:
			var e struct {
				Code    string          `json:"code"`
				Catalog json.RawMessage `json:"catalog"`
			}
			if json.Unmarshal(resp.Body, &e) == nil {
				switch e.Code {
				case "CATALOG_STALE":
					if len(e.Catalog) > 0 && a.storeCatalog(e.Catalog) == nil {
						a.log.Info("catalog was stale; the server sent the new one", "catalog_version", a.cat.currentVersion())
					}
				case "SOLD_OUT":
					a.catalogChanged()
				}
			}
		}
	})(w, r)
}

// ---------------------------------------------------------------- pay → vend → result

// handlePay is "Konfirmasi bayar": PAID on the server, then the coil, then the
// result. It answers after the coil has turned (or failed).
func (a *Agent) handlePay(w http.ResponseWriter, r *http.Request) {
	id, ok := normalizeID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Transaksi tidak ditemukan.")
		return
	}
	// The purchase carries on even if the page stops waiting: once money is
	// taken, the coil turns and the result is recorded.
	ctx := context.WithoutCancel(r.Context())
	out, perr := a.purchase(ctx, id, requestID(r))
	if perr != nil {
		perr.write(w)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Agent) purchase(ctx context.Context, id, rid string) (payReply, *pageError) {
	a.vendMu.Lock()
	defer a.vendMu.Unlock()

	// A purchase this machine already took through the board: answer from
	// the journal and never turn the coil again (BR-14).
	if e, ok := a.journal.Get(id); ok {
		switch e.Stage {
		case journal.Result:
			a.wakeFlusher()
			return reply(id, e.Status, e.Reason), nil
		case journal.Vending: // only while a vend is in progress, which the lock excludes
			return reply(id, statusFailed, reasonAgentRestart), nil
		case journal.Paying:
			// An earlier pay never got an answer. It is settled without dispensing.
			if err := a.settleUnanswered(ctx, e); upstream.IsOffline(err) {
				return payReply{}, offlineError("")
			}
			return payReply{}, newPageError(http.StatusConflict, "TRANSACTION_CLOSED", "Transaksi ini sudah ditutup. Silakan ulangi pembelian.")
		}
	}

	slot := a.slots.get(id)
	if err := a.journal.Put(journal.Entry{TxID: id, Slot: slot, Stage: journal.Paying}); err != nil {
		a.log.Error("journal: cannot save the payment step, so the payment is not taken", "tx", id, "err", err)
		_ = a.journal.Delete(id)
		return payReply{}, newPageError(http.StatusInternalServerError, "AGENT_ERROR", "Mesin tidak dapat mencatat transaksi. Hubungi petugas.")
	}

	st, err := a.pay(ctx, id, rid)
	if err != nil {
		if apiErr, ok := upstream.AsAPIError(err); ok {
			if apiErr.Status >= 500 && !keyProblem(apiErr) {
				// A server fault: the payment may or may not have been saved, so it
				// is settled like a lost answer, without dispensing.
				a.log.Warn("pay failed on the server; the coil was not turned, the transaction is settled later", "tx", id, "code", apiErr.Code)
				a.wakeFlusher()
				return payReply{}, a.passAPIError(apiErr)
			}
			_ = a.journal.Delete(id) // the server refused: nothing was paid
			return payReply{}, a.passAPIError(apiErr)
		}
		a.log.Warn("pay got no answer; the coil was not turned. The transaction is settled once the server answers", "tx", id, "err", err)
		a.wakeFlusher()
		return payReply{}, offlineError("")
	}
	switch st.Status {
	case statusPaid:
	case statusDispensed, statusFailed: // finished earlier: a repeated request
		_ = a.journal.Delete(id)
		reason := ""
		if st.FailureReason != nil {
			reason = *st.FailureReason
		}
		return reply(id, st.Status, reason), nil
	default:
		_ = a.journal.Delete(id)
		return payReply{}, newPageError(http.StatusConflict, "TRANSACTION_CLOSED", "Transaksi sudah "+strings.ToLower(st.Status)+".")
	}
	if st.SlotNumber > 0 {
		if slot != 0 && slot != st.SlotNumber {
			a.log.Warn("the server and the agent disagree on the slot; using the server's", "tx", id, "agent_slot", slot, "server_slot", st.SlotNumber)
		}
		slot = st.SlotNumber
	}

	// BR-14: the transaction is on disk before the command goes out.
	e := journal.Entry{TxID: id, Slot: slot, Stage: journal.Vending}
	if err := a.journal.Put(e); err != nil {
		a.log.Error("journal: cannot save before vending, so the coil is not turned (BR-14)", "tx", id, "err", err)
		e.Stage, e.Status, e.Reason = journal.Result, statusFailed, reasonJournalError
		_ = a.journal.Put(e) // the memory copy, at least
		_ = a.report(ctx, e, a.t.result)
		return reply(id, statusFailed, reasonJournalError), nil
	}

	if a.sim != nil && a.crashNext.CompareAndSwap(true, false) {
		a.simulatePowerCut(id, slot)
		return reply(id, statusFailed, reasonAgentRestart), nil // only reached in tests
	}

	res := a.vend(slot)
	e.Stage, e.Board = journal.Result, &res
	if res.OK {
		e.Status = statusDispensed
	} else {
		e.Status, e.Reason = statusFailed, res.Reason
	}
	if err := a.journal.Put(e); err != nil {
		a.log.Error("journal: cannot save the result; reporting it now", "tx", id, "err", err)
	}
	a.recent.add(vendRecord{At: a.now().UnixMilli(), TxID: id, Slot: slot, Code: res.Code, Status: e.Status, Reason: e.Reason, AckMs: res.AckMs, EndMs: res.EndMs})
	a.log.Info("vend", "tx", id, "slot", slot, "code", res.Code, "result", e.Status, "reason", e.Reason,
		"ack_ms", res.AckMs, "end_ms", res.EndMs, "extra", res.Extra, "board", a.board.Describe())

	if err := a.report(ctx, e, a.t.result); err != nil {
		a.log.Warn("result not delivered yet; it stays in the journal and is resent", "tx", id, "err", err)
	}
	return reply(id, e.Status, e.Reason), nil
}

// pay asks the server for PENDING -> PAID. Pay is safe to repeat (a repeat
// returns the current state), so attempts that got no answer are retried.
func (a *Agent) pay(ctx context.Context, id, rid string) (txState, error) {
	header := http.Header{"X-Request-Id": {rid}}
	var last error
	for attempt := 1; attempt <= a.t.payAttempts; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, a.t.pay)
		var st txState
		_, err := a.srv.JSON(cctx, http.MethodPost, "/api/machine/transactions/"+id+"/pay", struct{}{}, header, &st)
		cancel()
		if err == nil {
			a.markOnline()
			return st, nil
		}
		if apiErr, ok := upstream.AsAPIError(err); ok {
			a.noteAPIError(apiErr)
			return txState{}, err
		}
		last = err
		if attempt < a.t.payAttempts {
			time.Sleep(a.t.payGap)
		}
	}
	a.markOffline(last)
	return txState{}, last
}

func (a *Agent) vend(slot int) board.Result {
	if slot <= 0 {
		return board.Result{Slot: slot, Reason: board.ReasonUnknownSlot, Extra: "the agent does not know this transaction's slot"}
	}
	return a.board.Vend(slot)
}

// report sends a final result. It returns an error while the result stays in
// the journal (no answer, or a server fault); a refusal the server will never
// change is logged and dropped.
func (a *Agent) report(ctx context.Context, e journal.Entry, timeout time.Duration) error {
	body := map[string]string{"status": e.Status}
	if e.Reason != "" {
		body["failureReason"] = e.Reason
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := a.srv.JSON(cctx, http.MethodPost, "/api/machine/transactions/"+e.TxID+"/result", body, nil, nil)
	if err == nil {
		a.markOnline()
		_ = a.journal.Delete(e.TxID)
		a.log.Info("result recorded", "tx", e.TxID, "status", e.Status, "reason", e.Reason)
		a.catalogChanged() // FR-02: reload after each sale
		return nil
	}
	if apiErr, ok := upstream.AsAPIError(err); ok {
		a.noteAPIError(apiErr)
		if !keyProblem(apiErr) && apiErr.Status < 500 {
			a.log.Error("the server refused the result; removing it from the journal", "tx", e.TxID, "status", e.Status,
				"reason", e.Reason, "code", apiErr.Code, "message", apiErr.Message)
			_ = a.journal.Delete(e.TxID)
			return nil
		}
	} else {
		a.markOffline(err)
	}
	e.Attempts++
	e.LastError = err.Error()
	_ = a.journal.Put(e)
	return err
}

// settleUnanswered closes a payment whose answer never arrived. The coil
// never turned, so: still PENDING -> cancel it (EXPIRED); PAID -> FAILED with
// PAY_NO_REPLY; anything else is already final.
func (a *Agent) settleUnanswered(ctx context.Context, e journal.Entry) error {
	cctx, cancel := context.WithTimeout(ctx, a.t.call)
	defer cancel()
	var st txState
	if _, err := a.srv.JSON(cctx, http.MethodGet, "/api/machine/transactions/"+e.TxID, nil, nil, &st); err != nil {
		if apiErr, ok := upstream.AsAPIError(err); ok {
			a.noteAPIError(apiErr)
			if apiErr.Status == http.StatusNotFound {
				_ = a.journal.Delete(e.TxID)
				return nil
			}
		} else {
			a.markOffline(err)
		}
		return err
	}
	a.markOnline()
	switch st.Status {
	case statusPending:
		if _, err := a.srv.JSON(cctx, http.MethodPost, "/api/machine/transactions/"+e.TxID+"/cancel", map[string]string{"reason": "TIMEOUT"}, nil, nil); err != nil {
			if !upstream.IsOffline(err) {
				_ = a.journal.Delete(e.TxID)
			}
			return err
		}
		a.log.Info("unanswered payment settled: it never reached the server, closed as EXPIRED", "tx", e.TxID)
		_ = a.journal.Delete(e.TxID)
		return nil
	case statusPaid:
		a.log.Warn("unanswered payment settled: the server has it as PAID but the coil never turned; reporting FAILED with PAY_NO_REPLY", "tx", e.TxID, "slot", e.Slot)
		e.Stage, e.Status, e.Reason = journal.Result, statusFailed, reasonPayNoReply
		_ = a.journal.Put(e)
		return a.report(ctx, e, a.t.result)
	default:
		_ = a.journal.Delete(e.TxID)
		return nil
	}
}

func (a *Agent) simulatePowerCut(id string, slot int) {
	a.log.Warn("simulated power cut: the agent stops right after sending the vend command (TC-37). Start it again to see AGENT_RESTART",
		"tx", id, "slot", slot)
	time.Sleep(300 * time.Millisecond) // the coil has started turning
	if a.exit != nil {
		a.exit(3)
	}
}

// ---------------------------------------------------------------- cancel, report

func (a *Agent) handleCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := normalizeID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Transaksi tidak ditemukan.")
		return
	}
	a.forward(http.MethodPost, "/api/machine/transactions/"+id+"/cancel", nil)(w, r)
}

// handleReport is "Produk tidak keluar?". The sale's result goes first if it
// is still in the journal, since the server only accepts reports on DISPENSED.
func (a *Agent) handleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := normalizeID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Transaksi tidak ditemukan.")
		return
	}
	if e, ok := a.journal.Get(id); ok && e.Stage == journal.Result {
		cctx, cancel := context.WithTimeout(r.Context(), a.t.flushQuick)
		_ = a.flushOne(cctx, id)
		cancel()
	}
	a.forward(http.MethodPost, "/api/machine/transactions/"+id+"/report", func(resp *upstream.Response) {
		if resp.Status == http.StatusOK {
			a.log.Info("customer reported \"Produk tidak keluar\"", "tx", id)
		}
	})(w, r)
}

// ---------------------------------------------------------------- slot memory

// slotMemory remembers each transaction's slot from the create answer, for
// servers whose pay answer doesn't carry it.
type slotMemory struct {
	mu sync.Mutex
	m  map[string]slotNote
}

type slotNote struct {
	slot int
	at   time.Time
}

func (s *slotMemory) put(id string, slot int, now time.Time) {
	id, ok := normalizeID(id)
	if !ok || slot <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.m {
		if now.Sub(v.at) > 15*time.Minute {
			delete(s.m, k)
		}
	}
	s.m[id] = slotNote{slot: slot, at: now}
}

func (s *slotMemory) get(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id].slot
}

func normalizeID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	return id, journal.ValidID(id)
}
