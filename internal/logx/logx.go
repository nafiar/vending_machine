// Package logx sets up the agent's log: one line per event, to the console
// and to data/logs/vending-machine.log (kept to 3 old files of 5 MB each).
// As a Windows service the console goes nowhere, so the file is the log.
package logx

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const (
	FileName = "vending-machine.log"
	maxSize  = 5 << 20
	keep     = 3
)

// Setup returns the logger and the file writer to close at exit. toConsole
// adds stderr.
func Setup(dataDir, level string, toConsole bool) (*slog.Logger, io.Closer, error) {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	file, err := openRotating(filepath.Join(dir, FileName))
	if err != nil {
		return nil, nil, err
	}
	var w io.Writer = file
	if toConsole {
		w = fanout{file, os.Stderr}
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(level))
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})), file, nil
}

// Path is where the log file is written.
func Path(dataDir string) string { return filepath.Join(dataDir, "logs", FileName) }

// fanout writes to every writer and ignores their errors, so a missing
// console never stops the file log.
type fanout []io.Writer

func (f fanout) Write(p []byte) (int, error) {
	for _, w := range f {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

type rotating struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openRotating(path string) (*rotating, error) {
	r := &rotating{path: path}
	return r, r.open()
}

func (r *rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size+int64(len(p)) > maxSize && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate renames vending-machine.log to .1, .1 to .2, and so on.
func (r *rotating) rotate() {
	r.f.Close()
	for i := keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	_ = os.Rename(r.path, r.path+".1")
	if err := r.open(); err != nil {
		r.f = nil
	}
}

func (r *rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
