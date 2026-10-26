package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a small in-memory copy of vending-server's machine API with the
// same rules: PENDING -> PAID -> DISPENSED/FAILED, a repeat returns the current
// state, FAILED blocks the slot and raises the catalog version.
type fakeServer struct {
	t  *testing.T
	ts *httptest.Server

	mu           sync.Mutex
	key          string
	version      int64
	slots        map[int]*fakeSlot
	txs          map[string]*fakeTx
	calls        []string // "POST /api/machine/transactions/<id>/result DISPENSED", in order
	down         bool     // every request gets its connection closed: "server unreachable"
	dropPay      bool     // pay is applied, then the answer is lost
	payFault     bool     // pay is applied, then the server answers 500
	slotInPay    bool     // newer servers include slotNumber in transaction states
	catalogReads int
	images       map[string][]byte
}

type fakeSlot struct {
	qty     int
	blocked bool
	name    string
	price   int64
}

type fakeTx struct {
	id      string
	slot    int
	status  string
	reason  string
	created time.Time
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{
		t: t, key: "vmk_test", version: 7, slotInPay: true, txs: map[string]*fakeTx{}, images: map[string][]byte{},
		slots: map[int]*fakeSlot{
			1: {qty: 3, name: "Gacha Legendary", price: 50000},
			3: {qty: 1, name: "Gacha Rare", price: 25000},
			4: {qty: 0, name: "Gacha Uncommon", price: 15000},
		},
	}
	f.ts = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeServer) set(fn func(f *fakeServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeServer) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func (f *fakeServer) tx(id string) fakeTx {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.txs[id]; t != nil {
		return *t
	}
	return fakeTx{}
}

func hangUp(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := map[string]any{"code": code, "message": msg}
	for k, v := range extra {
		body[k] = v
	}
	send(w, status, body)
}

func (f *fakeServer) catalogLocked() map[string]any {
	var slots []map[string]any
	for n := 1; n <= 4; n++ {
		s := f.slots[n]
		if s == nil {
			slots = append(slots, map[string]any{"slotNumber": n, "available": false, "product": nil})
			continue
		}
		open := 0
		for _, t := range f.txs {
			if t.slot == n && (t.status == "PENDING" || t.status == "PAID") {
				open++
			}
		}
		slots = append(slots, map[string]any{"slotNumber": n, "available": !s.blocked && s.qty-open > 0,
			"product": map[string]any{"id": n, "name": s.name, "description": "", "price": s.price, "imageUrl": "/uploads/products/p" + fmt.Sprint(n) + ".png"}})
	}
	return map[string]any{"catalogVersion": f.version, "machine": map[string]any{"code": "VM-01", "name": "Lobby"}, "slots": slots}
}

func (f *fakeServer) stateLocked(t *fakeTx) map[string]any {
	st := map[string]any{"id": t.id, "status": t.status, "failureReason": nil}
	if t.reason != "" {
		st["failureReason"] = t.reason
	}
	if f.slotInPay {
		st["slotNumber"] = t.slot
	}
	return st
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		hangUp(w)
		return
	}
	path := r.URL.Path
	if strings.HasPrefix(path, "/uploads/") {
		if img, ok := f.images[path]; ok {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(img)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("X-Machine-Key") != f.key {
		fail(w, 401, "INVALID_MACHINE_KEY", "Kunci mesin tidak dikenal.", nil)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	entry := r.Method + " " + path
	if s, ok := body["status"].(string); ok {
		entry += " " + s
		if reason, ok := body["failureReason"].(string); ok {
			entry += " " + reason
		}
	}
	f.calls = append(f.calls, entry)

	parts := strings.Split(strings.TrimPrefix(path, "/api/machine/"), "/")
	switch {
	case r.Method == "POST" && path == "/api/machine/heartbeat":
		send(w, 200, map[string]any{"ok": true, "serverTime": time.Now().UnixMilli(), "catalogVersion": f.version})
	case r.Method == "GET" && path == "/api/machine/catalog":
		f.catalogReads++
		send(w, 200, f.catalogLocked())
	case r.Method == "POST" && path == "/api/machine/transactions":
		id, _ := body["id"].(string)
		slot := int(body["slotNumber"].(float64))
		if int64(body["catalogVersion"].(float64)) != f.version {
			fail(w, 409, "CATALOG_STALE", "Info produk diperbarui.", map[string]any{"catalog": f.catalogLocked()})
			return
		}
		if t := f.txs[id]; t != nil {
			send(w, 200, map[string]any{"id": id, "slotNumber": t.slot, "status": t.status})
			return
		}
		s := f.slots[slot]
		open := 0
		for _, t := range f.txs {
			if t.slot == slot && (t.status == "PENDING" || t.status == "PAID") {
				open++
			}
		}
		if s == nil || s.blocked || s.qty-open <= 0 {
			fail(w, 409, "SOLD_OUT", "Produk habis.", nil)
			return
		}
		f.txs[id] = &fakeTx{id: id, slot: slot, status: "PENDING", created: time.Now()}
		send(w, 201, map[string]any{"id": id, "slotNumber": slot, "amount": s.price, "expiresInSeconds": 60, "status": "PENDING",
			"product": map[string]any{"id": slot, "name": s.name, "price": s.price}})
	case len(parts) >= 2 && parts[0] == "transactions":
		t := f.txs[parts[1]]
		if t == nil {
			fail(w, 404, "NOT_FOUND", "Transaksi tidak ditemukan.", nil)
			return
		}
		action := ""
		if len(parts) == 3 {
			action = parts[2]
		}
		switch {
		case r.Method == "GET" && action == "":
			send(w, 200, f.stateLocked(t))
		case action == "pay":
			if t.status == "PENDING" && time.Since(t.created) > 60*time.Second {
				t.status = "EXPIRED"
			}
			switch t.status {
			case "EXPIRED":
				fail(w, 410, "EXPIRED", "Waktu pembayaran habis.", nil)
				return
			case "CANCELLED":
				fail(w, 409, "TRANSACTION_CLOSED", "Transaksi sudah dibatalkan.", nil)
				return
			case "PENDING":
				t.status = "PAID"
			}
			if f.dropPay {
				hangUp(w)
				return
			}
			if f.payFault {
				fail(w, 500, "INTERNAL", "Terjadi kesalahan di server. Coba lagi.", nil)
				return
			}
			send(w, 200, f.stateLocked(t))
		case action == "result":
			status, _ := body["status"].(string)
			reason, _ := body["failureReason"].(string)
			switch t.status {
			case "PAID":
				t.status, t.reason = status, reason
				if status == "DISPENSED" {
					f.slots[t.slot].qty--
				} else {
					f.slots[t.slot].blocked = true
					f.version++
				}
			case "DISPENSED", "FAILED":
				if t.status != status {
					fail(w, 409, "CONFLICT", "sudah tercatat", nil)
					return
				}
			default:
				fail(w, 409, "NOT_PAID", "Transaksi belum dibayar.", nil)
				return
			}
			send(w, 200, f.stateLocked(t))
		case action == "cancel":
			if t.status == "PENDING" {
				t.status = "CANCELLED"
				if body["reason"] == "TIMEOUT" {
					t.status = "EXPIRED"
				}
			}
			send(w, 200, f.stateLocked(t))
		case action == "report":
			if t.status != "DISPENSED" {
				fail(w, 409, "NOT_REPORTABLE", "Hanya penjualan yang berhasil yang bisa dilaporkan.", nil)
				return
			}
			send(w, 200, map[string]any{"id": t.id, "reported": true})
		default:
			fail(w, 404, "NOT_FOUND", "no such action", nil)
		}
	case r.Method == "POST" && path == "/api/machine/restock-sessions":
		if r.Header.Get("Authorization") != "Bearer op-token" {
			fail(w, 401, "UNAUTHORIZED", "Sesi berakhir.", nil)
			return
		}
		for _, s := range f.slots {
			s.blocked = false
		}
		f.version++
		send(w, 200, map[string]any{"id": body["id"], "sessionId": 1, "savedAt": time.Now().UnixMilli(), "catalogVersion": f.version})
	default:
		fail(w, 404, "NOT_FOUND", "Alamat API tidak ditemukan: "+entry, nil)
	}
}
