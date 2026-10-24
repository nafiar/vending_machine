// Command vending-machine is the Kiosk Agent that runs on each vending
// machine's PC: it serves the kiosk page to Edge at http://127.0.0.1:8080,
// talks to the vending server with the machine's key, and drives the motor
// board on the COM port.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"vending-machine/internal/agent"
	"vending-machine/internal/board"
	"vending-machine/internal/config"
	"vending-machine/internal/journal"
	"vending-machine/internal/logx"
	"vending-machine/internal/upstream"
	"vending-machine/internal/winsvc"
	"vending-machine/web"
)

var version = "dev" // set by the Makefile: -ldflags "-X main.version=..."

const usage = `vending-machine - the Kiosk Agent of the vending machine information system

Usage:
  vending-machine [run]            serve the kiosk page and run the agent (the default)
  vending-machine check            check config.json, the server, the machine key and the board, then exit
  vending-machine ports            list the serial ports and which one looks like the CH340 board
  vending-machine vend <slot>      turn one slot's coil once, without the server (to test the wiring)
  vending-machine service install  Windows: run the agent as a service at every boot (as administrator)
  vending-machine service uninstall|start|stop|status
  vending-machine version

Flags:
  -config <file>   the settings file (default: config.json here, or next to the program)
  -port <name>     with vend and check: use this serial port instead of the configured one
  -sim             with vend: use the simulator instead of the board

The kiosk is at http://127.0.0.1:8080/ (the "listen" setting); /dev/ is a page for
testing it on a computer, with the simulator's controls.
`

func main() {
	if winsvc.IsService() {
		if err := winsvc.Run(func(ctx context.Context) error { return runAgent(ctx, os.Args[1:], false) }); err != nil {
			os.Exit(1)
		}
		return
	}

	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = runAgent(ctx, args, true)
		stop()
	case "check":
		err = check(args)
	case "ports":
		err = listPorts()
	case "vend":
		err = vendOnce(args)
	case "service":
		err = service(args)
	case "version":
		fmt.Println("vending-machine", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// flags parses the common flags; the arguments that are not flags come back.
type options struct {
	config string
	port   string
	sim    bool
}

func parseFlags(name string, args []string) (options, []string, error) {
	var o options
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.config, "config", "", "")
	fs.StringVar(&o.port, "port", "", "")
	fs.BoolVar(&o.sim, "sim", false, "")
	// Allow flags after positional arguments too: "vend 3 -sim".
	var rest []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return o, nil, fmt.Errorf("%s: %w", name, err)
		}
		args = fs.Args()
		if len(args) > 0 {
			rest = append(rest, args[0])
			args = args[1:]
		}
	}
	return o, rest, nil
}

func loadConfig(path string) (config.Config, error) {
	found, err := config.Find(path)
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(found)
}

func newBoard(cfg config.Config, port string, forceSim bool) (board.Board, *board.Sim, error) {
	if cfg.Board == "sim" || forceSim {
		s, err := board.NewSim(cfg.Slots, cfg.SimMode, cfg.SimTurn())
		return s, s, err
	}
	if port == "" {
		port = cfg.SerialPort
	}
	return board.NewSerial(port, cfg.Slots), nil, nil
}

// ---------------------------------------------------------------- run

func runAgent(ctx context.Context, args []string, console bool) error {
	o, _, err := parseFlags("run", args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(o.config)
	if err != nil {
		return err
	}
	log, logFile, err := logx.Setup(cfg.DataDir, cfg.LogLevel, console)
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}
	defer logFile.Close()
	log.Info("starting", "version", version, "config", cfg.Path, "server", cfg.ServerURL, "board", cfg.Board, "slots", cfg.Slots.String())

	j, problems, err := journal.Open(filepath.Join(cfg.DataDir, "journal"))
	if err != nil {
		log.Error("journal", "err", err)
		return fmt.Errorf("journal: %w", err)
	}
	for _, p := range problems {
		log.Error("journal: a damaged file was set aside", "problem", p)
	}

	b, sim, err := newBoard(cfg, "", false)
	if err != nil {
		return err
	}
	defer b.Close()

	srv, err := upstream.New(cfg.ServerURL, cfg.MachineKey, cfg.InsecureTLS, version)
	if err != nil {
		return err
	}
	if cfg.InsecureTLS {
		log.Warn("insecureSkipVerify is on: the server's certificate is not checked (only for a self-signed demo server)")
	}

	a := agent.New(agent.Options{
		Config: cfg, Board: b, Sim: sim, Journal: j, Server: srv, Log: log, Web: web.Files, Version: version,
		Exit: func(code int) { logFile.Close(); os.Exit(code) },
	})
	a.Recover()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("cannot listen", "listen", cfg.Listen, "err", err)
		return fmt.Errorf("listen on %s: %w (is another copy already running, e.g. the VendingMachine service?)", cfg.Listen, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.Start(ctx)

	httpSrv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	base := "http://" + cfg.Listen
	log.Info("kiosk ready", "kiosk", base+"/", "developer_page", base+"/dev/", "board", b.Describe(), "log", logx.Path(cfg.DataDir))
	if console {
		fmt.Fprintf(os.Stderr, "\n  Kiosk:          %s/\n  Developer page: %s/dev/\n  Stop:           Ctrl+C\n\n", base, base)
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("stopping: finishing any purchase in progress")
	shutdown, done := context.WithTimeout(context.Background(), 25*time.Second)
	defer done()
	_ = httpSrv.Shutdown(shutdown)
	log.Info("stopped")
	return nil
}

// ---------------------------------------------------------------- check

func check(args []string) error {
	o, _, err := parseFlags("check", args)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(o.config)
	if err != nil {
		return err
	}
	ok := true
	report := func(good bool, what, detail string) {
		mark := "OK  "
		if !good {
			mark, ok = "FAIL", false
		}
		fmt.Printf("%s %-9s %s\n", mark, what, detail)
	}
	fmt.Printf("config    %s\n          server %s · listen %s · board %s · slots %s\n          data %s\n\n",
		cfg.Path, cfg.ServerURL, cfg.Listen, cfg.Board, cfg.Slots.String(), cfg.DataDir)

	srv, err := upstream.New(cfg.ServerURL, cfg.MachineKey, cfg.InsecureTLS, version)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var hb struct {
		CatalogVersion int64 `json:"catalogVersion"`
	}
	if _, err := srv.JSON(ctx, http.MethodPost, "/api/machine/heartbeat", struct{}{}, nil, &hb); err != nil {
		hint := ""
		if e, isAPI := upstream.AsAPIError(err); isAPI && e.Code == "INVALID_MACHINE_KEY" {
			hint = " (the key is wrong: paste it again from the seed output or the admin web)"
		} else if upstream.IsOffline(err) {
			hint = " (is the server running, and is serverUrl right?)"
		}
		report(false, "server", err.Error()+hint)
	} else {
		var cat struct {
			Machine struct{ Code, Name string } `json:"machine"`
			Slots   []struct {
				SlotNumber int  `json:"slotNumber"`
				Available  bool `json:"available"`
				Product    *struct {
					Name  string `json:"name"`
					Price int64  `json:"price"`
				} `json:"product"`
			} `json:"slots"`
		}
		if _, err := srv.JSON(ctx, http.MethodGet, "/api/machine/catalog", nil, nil, &cat); err != nil {
			report(false, "catalog", err.Error())
		} else {
			report(true, "server", fmt.Sprintf("reachable, machine key accepted: %s (%s), catalog version %d", cat.Machine.Code, cat.Machine.Name, hb.CatalogVersion))
			for _, s := range cat.Slots {
				name, state := "(empty)", "sold out"
				if s.Product != nil {
					name = fmt.Sprintf("%s, Rp%d", s.Product.Name, s.Product.Price)
				}
				if s.Available {
					state = "available"
				}
				code, wired := cfg.Slots[s.SlotNumber]
				if !wired {
					code = "not in slotMap"
				}
				fmt.Printf("          slot %d -> %-14s %s (%s)\n", s.SlotNumber, code, name, state)
			}
		}
	}

	b, _, err := newBoard(cfg, o.port, false)
	if err != nil {
		return err
	}
	if serial, isSerial := b.(*board.Serial); isSerial {
		if name, err := serial.Open(); err != nil {
			report(false, "board", err.Error())
		} else {
			report(true, "board", "serial port "+name+" opened (no command sent; try `vending-machine vend 1`)")
		}
	} else {
		report(true, "board", b.Describe()+": no hardware needed")
	}
	b.Close()

	j, problems, err := journal.Open(filepath.Join(cfg.DataDir, "journal"))
	if err != nil {
		report(false, "journal", err.Error())
	} else {
		detail := "empty"
		if n := j.Len(); n > 0 {
			detail = fmt.Sprintf("%d purchase(s) still to report; the agent sends them when it runs", n)
		}
		report(len(problems) == 0, "journal", detail+" ("+j.Dir()+")")
	}
	if !ok {
		return errors.New("some checks failed")
	}
	return nil
}

// ---------------------------------------------------------------- ports, vend

func listPorts() error {
	ports, err := board.ListPorts()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		fmt.Println("No serial ports found. Plug in the board's USB cable and run this again.")
		return nil
	}
	for _, p := range ports {
		mark := "  "
		if p.CH340 {
			mark = "* "
		}
		fmt.Printf("%s%-28s %s\n", mark, p.Name, p.Description)
	}
	fmt.Println("\n* looks like the CH340 board. With \"serialPort\": \"\" the agent picks it by itself.")
	return nil
}

func vendOnce(args []string) error {
	o, rest, err := parseFlags("vend", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: vending-machine vend <slot> [-sim] [-port COM3]")
	}
	slot, err := strconv.Atoi(rest[0])
	if err != nil || slot < 1 {
		return fmt.Errorf("%q is not a slot number", rest[0])
	}
	cfg, err := loadConfig(o.config)
	if err != nil {
		return err
	}
	b, _, err := newBoard(cfg, o.port, o.sim)
	if err != nil {
		return err
	}
	defer b.Close()
	code, known := cfg.Slots[slot]
	if !known {
		code = "?"
	}
	fmt.Fprintf(os.Stderr, "Turning slot %d (%s) on %s. This only tests the board: nothing is recorded on the server.\n", slot, code, b.Describe())
	res := b.Vend(slot)
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if !res.OK {
		return fmt.Errorf("vend failed: %s", res.Reason)
	}
	return nil
}

// ---------------------------------------------------------------- service

func service(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: vending-machine service install|uninstall|start|stop|status [-config file]")
	}
	switch args[0] {
	case "install":
		o, _, err := parseFlags("service install", args[1:])
		if err != nil {
			return err
		}
		cfgPath, err := config.Find(o.config)
		if err != nil {
			return err
		}
		if _, err := config.Load(cfgPath); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := winsvc.Install(exe, cfgPath); err != nil {
			return err
		}
		fmt.Printf("Service %s installed and started.\n  program %s\n  config  %s\nIt starts at every boot and restarts 5 s after a failure.\n", winsvc.Name, exe, cfgPath)
	case "uninstall":
		if err := winsvc.Uninstall(); err != nil {
			return err
		}
		fmt.Printf("Service %s stopped and removed.\n", winsvc.Name)
	case "start":
		if err := winsvc.Start(); err != nil {
			return err
		}
		fmt.Printf("Service %s started.\n", winsvc.Name)
	case "stop":
		if err := winsvc.Stop(); err != nil {
			return err
		}
		fmt.Printf("Service %s stopped.\n", winsvc.Name)
	case "status":
		st, err := winsvc.Status()
		if err != nil {
			return err
		}
		fmt.Printf("Service %s: %s\n", winsvc.Name, st)
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
	return nil
}
