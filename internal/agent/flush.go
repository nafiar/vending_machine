package agent

import (
	"context"
	"time"

	"vending-machine/internal/journal"
	"vending-machine/internal/upstream"
)

// Recover runs once at start-up, before the kiosk page is served. A purchase
// left in stage VENDING may or may not have turned its coil (the product may
// have fallen), so it becomes FAILED with AGENT_RESTART and is never turned
// again (BR-14, TC-37). The flusher then reports it.
func (a *Agent) Recover() {
	for _, e := range a.journal.List() {
		switch e.Stage {
		case journal.Vending:
			a.log.Warn("restarted during a vend: reporting FAILED with AGENT_RESTART; the coil is never turned again (BR-14)",
				"tx", e.TxID, "slot", e.Slot, "since", e.UpdatedAt.Local().Format(time.DateTime))
			e.Stage, e.Status, e.Reason = journal.Result, statusFailed, reasonAgentRestart
			if err := a.journal.Put(e); err != nil {
				a.log.Error("journal: cannot save the AGENT_RESTART result; it is reported from memory", "tx", e.TxID, "err", err)
			}
			a.recent.add(vendRecord{At: a.now().UnixMilli(), TxID: e.TxID, Slot: e.Slot, Status: statusFailed, Reason: reasonAgentRestart})
		case journal.Result:
			a.log.Info("result from before the restart still to report", "tx", e.TxID, "status", e.Status, "reason", e.Reason)
		case journal.Paying:
			a.log.Info("payment from before the restart without an answer; it is settled without dispensing", "tx", e.TxID)
		}
	}
}

// flushLoop delivers the journal: at once, every 10 s, and when nudged.
func (a *Agent) flushLoop(ctx context.Context) {
	tick := time.NewTicker(a.t.flushEvery)
	defer tick.Stop()
	for {
		if a.journal.Len() > 0 {
			a.flush(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.wake:
		}
	}
}

func (a *Agent) wakeFlusher() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// flush goes through the journal oldest first and stops at the first sign
// that the server is unreachable.
func (a *Agent) flush(ctx context.Context) {
	for _, e := range a.journal.List() {
		if ctx.Err() != nil {
			return
		}
		if err := a.flushOne(ctx, e.TxID); upstream.IsOffline(err) {
			return
		}
	}
}

func (a *Agent) flushOne(ctx context.Context, id string) error {
	a.vendMu.Lock()
	defer a.vendMu.Unlock()
	e, ok := a.journal.Get(id) // read again under the lock
	if !ok {
		return nil
	}
	switch e.Stage {
	case journal.Result:
		return a.report(ctx, e, a.t.result)
	case journal.Paying:
		return a.settleUnanswered(ctx, e)
	}
	return nil // VENDING exists only while a purchase holds the lock, or before Recover
}

// flushQuick gives the journal a moment before work that depends on the
// server's counts: a new purchase or a restock visit.
func (a *Agent) flushQuick(ctx context.Context) {
	if a.journal.Len() == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, a.t.flushQuick)
	defer cancel()
	a.flush(cctx)
}
