// Package journal keeps the purchase steps that must survive a crash or a
// power cut: one small JSON file per transaction, written before each step
// that can't be repeated, and deleted once the server has the final result.
//
// BR-14: the agent saves the transaction to disk before sending the vend
// command. A transaction found at start-up in stage VENDING may or may not
// have turned its coil, so it is reported FAILED with AGENT_RESTART and is
// never turned again.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"vending-machine/internal/board"
)

// Stages of one purchase.
const (
	Paying  = "PAYING"  // pay sent to the server, no answer yet: the coil has not turned
	Vending = "VENDING" // the vend command may have reached the board
	Result  = "RESULT"  // the board answered; the server has not acknowledged the result yet
)

// Entry is one unfinished purchase.
type Entry struct {
	TxID      string        `json:"txId"`
	Slot      int           `json:"slot"`
	Stage     string        `json:"stage"`
	Status    string        `json:"status,omitempty"`        // DISPENSED or FAILED, in stage RESULT
	Reason    string        `json:"failureReason,omitempty"` // for FAILED
	Board     *board.Result `json:"board,omitempty"`         // what the board answered, for the log
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
	Attempts  int           `json:"attempts,omitempty"` // tries to report the result
	LastError string        `json:"lastError,omitempty"`
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidID reports whether id is a lowercase UUID, the only names the journal uses.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Journal is the folder of unfinished purchases. The copy in memory is the
// live state; the files are what a restart reads back.
type Journal struct {
	dir     string
	mu      sync.Mutex
	entries map[string]Entry
	now     func() time.Time
}

// Open reads the folder, creating it if needed. Files it can't read are
// renamed to *.bad and listed in problems, so one damaged file doesn't stop
// the agent.
func Open(dir string) (j *Journal, problems []string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	j = &Journal{dir: dir, entries: map[string]Entry{}, now: time.Now}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range names {
		name := f.Name()
		path := filepath.Join(dir, name)
		switch {
		case strings.HasSuffix(name, ".tmp"):
			_ = os.Remove(path) // a write that never finished; its previous version (if any) is intact
			continue
		case !strings.HasSuffix(name, ".json"):
			continue
		}
		data, err := os.ReadFile(path)
		var e Entry
		if err == nil {
			err = json.Unmarshal(data, &e)
		}
		if err == nil && (!ValidID(e.TxID) || e.TxID+".json" != name) {
			err = errors.New("the file name and txId don't match")
		}
		if err != nil {
			_ = os.Rename(path, path+".bad")
			problems = append(problems, fmt.Sprintf("%s: %v (renamed to %s.bad)", name, err, name))
			continue
		}
		j.entries[e.TxID] = e
	}
	return j, problems, nil
}

// Dir is the journal folder.
func (j *Journal) Dir() string { return j.dir }

// Put saves an entry: in memory first, then on disk with an atomic replace.
// A disk error is returned, but the memory copy stays updated.
func (j *Journal) Put(e Entry) error {
	if !ValidID(e.TxID) {
		return fmt.Errorf("journal: %q is not a transaction id", e.TxID)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now().UTC()
	if old, ok := j.entries[e.TxID]; ok && e.CreatedAt.IsZero() {
		e.CreatedAt = old.CreatedAt
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	j.entries[e.TxID] = e
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(j.dir, e.TxID+".json"), data)
}

// Get returns the entry for a transaction.
func (j *Journal) Get(id string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[id]
	return e, ok
}

// Delete removes a finished transaction.
func (j *Journal) Delete(id string) error {
	if !ValidID(id) {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.entries, id)
	err := os.Remove(filepath.Join(j.dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns the entries, oldest first.
func (j *Journal) List() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Entry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.Before(out[b].CreatedAt)
		}
		return out[a].TxID < out[b].TxID
	})
	return out
}

// Len is the number of unfinished purchases.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}

// writeFileAtomic writes to a temporary file, flushes it to the disk, then
// renames it over the old one, so a power cut leaves either version, whole.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" { // NTFS records the rename itself; elsewhere sync the folder too
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}
