package board

import (
	"fmt"
	"sync"
	"time"
)

// Simulator modes (FR-29): the three board behaviours, switchable at any time.
const (
	SimOK    = "ok"     // echo, then END after the turn time: DISPENSED
	SimJam   = "jam"    // echo, no END: MOTOR_TIMEOUT after 5 s
	SimNoAck = "no_ack" // no echo: NO_ACK after 0.5 s
)

// SimModes lists the valid modes.
var SimModes = []string{SimOK, SimJam, SimNoAck}

// Sim is a board without hardware, for development, tests and as a demo
// fallback. It waits as long as the real board would.
type Sim struct {
	mu         sync.Mutex // one vend at a time, like the real board
	modeMu     sync.Mutex
	slots      SlotMap
	mode       string
	turn       time.Duration
	ackTimeout time.Duration
	endTimeout time.Duration
}

// NewSim returns a simulator. turn is how long a successful turn takes
// (measured on VM-01: about 2.5 s).
func NewSim(slots SlotMap, mode string, turn time.Duration) (*Sim, error) {
	s := &Sim{slots: slots, turn: turn, ackTimeout: AckTimeout, endTimeout: EndTimeout}
	if mode == "" {
		mode = SimOK
	}
	if err := s.SetMode(mode); err != nil {
		return nil, err
	}
	return s, nil
}

// SetMode switches the behaviour for the next vends.
func (s *Sim) SetMode(mode string) error {
	switch mode {
	case SimOK, SimJam, SimNoAck:
	default:
		return fmt.Errorf("simulator mode %q: use ok, jam or no_ack", mode)
	}
	s.modeMu.Lock()
	s.mode = mode
	s.modeMu.Unlock()
	return nil
}

// Mode returns the current behaviour.
func (s *Sim) Mode() string {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	return s.mode
}

// Describe names the board for logs and the status page.
func (s *Sim) Describe() string { return "simulator (" + s.Mode() + ")" }

// Close does nothing.
func (s *Sim) Close() error { return nil }

// Vend pretends to turn the slot's coil.
func (s *Sim) Vend(slot int) Result {
	code, ok := s.slots[slot]
	if !ok {
		return Result{Slot: slot, Reason: ReasonUnknownSlot}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	echo := 70 * time.Millisecond // as measured
	switch s.Mode() {
	case SimNoAck:
		time.Sleep(s.ackTimeout)
		return Result{Slot: slot, Code: code, Reason: ReasonNoAck}
	case SimJam:
		time.Sleep(s.endTimeout)
		return Result{Slot: slot, Code: code, Reason: ReasonMotorTimeout, AckMs: millis(echo)}
	default:
		time.Sleep(s.turn)
		return Result{OK: true, Slot: slot, Code: code, AckMs: millis(echo), EndMs: millis(s.turn)}
	}
}
