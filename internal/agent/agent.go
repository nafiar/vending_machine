// Package agent is the Kiosk Agent (PRD D12): it serves the kiosk page on
// localhost, forwards the page's calls to the server with the machine key (so
// the key never sits in browser code), owns the money-to-motor step, and keeps
// the journal that makes that step safe across restarts.
package agent

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"vending-machine/internal/board"
	"vending-machine/internal/config"
	"vending-machine/internal/journal"
	"vending-machine/internal/upstream"
)

// Options are the parts an Agent is built from.
type Options struct {
	Config  config.Config
	Board   board.Board
	Sim     *board.Sim // the same board when it is the simulator, else nil
	Journal *journal.Journal
	Server  *upstream.Client
	Log     *slog.Logger
	Web     fs.FS // web/ with kiosk/ and dev/
	Version string
	Exit    func(code int) // for the simulated power cut; os.Exit when nil
}

// Agent is one machine's agent.
type Agent struct {
	cfg     config.Config
	board   board.Board
	sim     *board.Sim
	journal *journal.Journal
	srv     *upstream.Client
	log     *slog.Logger
	web     fs.FS
	version string
	exit    func(int)
	started time.Time
	now     func() time.Time

	// vendMu allows one purchase at a time through the board (BR-14). The
	// journal flusher takes it too, so it never touches a purchase in progress.
	vendMu sync.Mutex

	cat       catalogCache
	conn      connState
	slots     slotMemory
	recent    recentVends
	imageDir  string
	wake      chan struct{}
	crashNext atomic.Bool

	t timeouts
}

type timeouts struct {
	pay         time.Duration // one pay attempt
	payAttempts int
	payGap      time.Duration // pause between pay attempts
	result      time.Duration // reporting a result while the customer waits
	call        time.Duration // a forwarded call
	catalog     time.Duration
	health      time.Duration
	flushQuick  time.Duration // settling the journal before a new purchase or a restock
	flushEvery  time.Duration
}

func defaultTimeouts() timeouts {
	return timeouts{
		pay:         3 * time.Second,
		payAttempts: 3,
		payGap:      500 * time.Millisecond,
		result:      4 * time.Second,
		call:        6 * time.Second,
		catalog:     5 * time.Second,
		health:      5 * time.Second,
		flushQuick:  2 * time.Second,
		flushEvery:  10 * time.Second,
	}
}

// New builds the agent. Call Recover, then Start, then serve Handler.
func New(o Options) *Agent {
	a := &Agent{
		cfg:      o.Config,
		board:    o.Board,
		sim:      o.Sim,
		journal:  o.Journal,
		srv:      o.Server,
		log:      o.Log,
		web:      o.Web,
		version:  o.Version,
		exit:     o.Exit,
		started:  time.Now(),
		now:      time.Now,
		imageDir: imageCacheDir(o.Config.DataDir),
		wake:     make(chan struct{}, 1),
		t:        defaultTimeouts(),
	}
	a.slots.m = map[string]slotNote{}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	return a
}

// Start runs the background work until ctx ends: the first heartbeat and
// catalog load (FR-02 "at boot"), the heartbeat every 5 minutes (FR-28) and
// the journal flusher.
func (a *Agent) Start(ctx context.Context) {
	go func() {
		if _, err := a.heartbeat(ctx); err != nil {
			a.log.Warn("server not reachable at start; the kiosk shows \"Mesin offline\" until it is", "server", a.srv.BaseURL(), "err", err)
		}
		if _, err := a.catalog(ctx, true); err == nil {
			m := a.cat.info()
			a.log.Info("catalog loaded", "machine", m.Code, "name", m.Name, "catalog_version", a.cat.currentVersion())
		}
	}()
	go a.flushLoop(ctx)
	go a.heartbeatLoop(ctx)
}

// Handler serves the kiosk page, its API and the developer page.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("GET /api/catalog", a.handleCatalog)
	mux.HandleFunc("POST /api/transactions", a.handleCreate)
	mux.HandleFunc("POST /api/transactions/{id}/pay", a.handlePay)
	mux.HandleFunc("POST /api/transactions/{id}/cancel", a.handleCancel)
	mux.HandleFunc("POST /api/transactions/{id}/report", a.handleReport)
	mux.HandleFunc("POST /api/operator/login", a.handleLogin)
	mux.HandleFunc("POST /api/operator/logout", a.forward(http.MethodPost, "/api/machine/operator/logout", nil))
	mux.HandleFunc("GET /api/operator/slots", a.forward(http.MethodGet, "/api/machine/operator/slots", nil))
	mux.HandleFunc("POST /api/restock-sessions", a.handleRestock)
	mux.HandleFunc("GET /api/agent/status", a.handleStatus)
	mux.HandleFunc("POST /api/agent/sim", a.handleSim)
	mux.HandleFunc("GET /api/", a.apiNotFound)
	mux.HandleFunc("POST /api/", a.apiNotFound)
	mux.HandleFunc("GET /uploads/", a.handleUploads)
	mux.Handle("GET /dev/", http.StripPrefix("/dev", a.static("dev")))
	mux.Handle("GET /", a.static("kiosk"))
	return a.middleware(mux)
}

func (a *Agent) apiNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "NOT_FOUND", "Alamat API tidak ditemukan: "+r.Method+" "+r.URL.Path)
}

// static serves one folder of web/. Files change with each release, so the
// browser always revalidates them.
func (a *Agent) static(dir string) http.Handler {
	sub, err := fs.Sub(a.web, dir)
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
