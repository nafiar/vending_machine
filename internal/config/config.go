// Package config reads the agent's settings from config.json.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vending-machine/internal/board"
)

// Placeholder is the machineKey in the example files; the agent refuses to start with it.
const Placeholder = "PASTE-THE-MACHINE-KEY-HERE"

// Config is config.json. Every field except serverUrl and machineKey has a default.
type Config struct {
	ServerURL   string            `json:"serverUrl"`          // https://35.219.98.32, or http://localhost:8080 on your Mac
	MachineKey  string            `json:"machineKey"`         // vmk_..., shown once when the machine is registered or seeded
	Listen      string            `json:"listen"`             // where the kiosk page is served; default 127.0.0.1:8080
	Board       string            `json:"board"`              // "serial" (default) or "sim"
	SerialPort  string            `json:"serialPort"`         // "" finds the CH340 by itself; or COM3, /dev/cu.usbserial-110
	SlotMap     map[string]string `json:"slotMap"`            // slot number -> board code; default 1=AA 2=BA 3=AB 4=BB 5=AC 6=BC
	SimMode     string            `json:"simMode"`            // simulator start mode: ok (default), jam or no_ack
	SimTurnMs   int               `json:"simTurnMs"`          // simulated turn time; default 2500 (measured 2.31-2.64 s)
	InsecureTLS bool              `json:"insecureSkipVerify"` // only for a server with a self-signed certificate
	DataDir     string            `json:"dataDir"`            // journal, image cache and logs; default "data" next to config.json
	LogLevel    string            `json:"logLevel"`           // debug, info (default), warn or error
	HeartbeatS  int               `json:"heartbeatSeconds"`   // FR-28; default 300

	Path  string        `json:"-"` // the file this was read from
	Slots board.SlotMap `json:"-"`
}

// Heartbeat is the interval between heartbeats.
func (c Config) Heartbeat() time.Duration { return time.Duration(c.HeartbeatS) * time.Second }

// SimTurn is the simulated turn time.
func (c Config) SimTurn() time.Duration { return time.Duration(c.SimTurnMs) * time.Millisecond }

// Find picks the config file: the explicit path, else config.json in the
// current folder, else config.json next to the program (where the Windows
// service looks, since services start in C:\Windows\System32).
func Find(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	candidates := []string{"config.json"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("config.json not found: copy config.example.json (on the machine) or config.dev.json (on your computer) to config.json, or pass -config <file>")
}

// Load reads and checks a config file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields() // a typo in a setting name is an error, not a silent default
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	if err := c.normalize(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) normalize() error {
	c.ServerURL = strings.TrimRight(strings.TrimSpace(c.ServerURL), "/")
	u, err := url.Parse(c.ServerURL)
	if c.ServerURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("serverUrl %q: use the server's address, e.g. https://35.219.98.32 or http://localhost:8080", c.ServerURL)
	}

	c.MachineKey = strings.TrimSpace(c.MachineKey)
	switch {
	case c.MachineKey == "" || c.MachineKey == Placeholder:
		return errors.New("machineKey is not set: paste this machine's key (vmk_...), printed by `vending-server seed-demo` or `./deploy/deploy.sh <ssh-target> keys`, or shown once when the machine is registered or its key rotated in the admin web")
	case strings.ContainsAny(c.MachineKey, " \t\r\n\""):
		return errors.New("machineKey contains spaces or quotes: paste the key alone, e.g. vmk_AbC...")
	}

	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q: use host:port, e.g. 127.0.0.1:8080", c.Listen)
	}

	c.Board = strings.ToLower(strings.TrimSpace(c.Board))
	switch c.Board {
	case "":
		c.Board = "serial"
	case "serial", "sim":
	default:
		return fmt.Errorf(`board %q: use "serial" (the motor board) or "sim" (the simulator)`, c.Board)
	}

	slots, err := board.ParseSlotMap(c.SlotMap)
	if err != nil {
		return err
	}
	c.Slots = slots

	if c.SimMode == "" {
		c.SimMode = board.SimOK
	}
	if _, err := board.NewSim(slots, c.SimMode, 0); err != nil {
		return err
	}
	if c.SimTurnMs == 0 {
		c.SimTurnMs = 2500
	}
	if c.SimTurnMs < 0 || c.SimTurnMs > 60000 {
		return fmt.Errorf("simTurnMs %d: use 0 to 60000", c.SimTurnMs)
	}

	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(filepath.Dir(c.Path), c.DataDir)
	}

	c.LogLevel = strings.ToLower(strings.TrimSpace(c.LogLevel))
	switch c.LogLevel {
	case "":
		c.LogLevel = "info"
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logLevel %q: use debug, info, warn or error", c.LogLevel)
	}

	if c.HeartbeatS == 0 {
		c.HeartbeatS = 300
	}
	if c.HeartbeatS < 5 {
		return fmt.Errorf("heartbeatSeconds %d: use 5 or more", c.HeartbeatS)
	}
	return nil
}
