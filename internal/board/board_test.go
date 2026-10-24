package board

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Codes that turned a coil in probe_log.csv on VM-01.
var wired = map[string]bool{"AA": true, "BA": true, "AB": true, "BB": true, "AC": true, "BC": true}

// fakeBoard behaves like the measured board: it echoes every command and sends
// END after the turn, but only for wired codes.
type fakeBoard struct {
	mu        sync.Mutex
	out       []byte
	turn      time.Duration
	jam       bool
	silent    bool
	noise     []byte
	written   []string
	writeErr  error
	closed    bool
	resetErrs int
}

func (f *fakeBoard) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	f.written = append(f.written, string(p))
	if f.silent {
		return len(p), nil
	}
	code := string(p[:2])
	f.out = append(f.out, f.noise...)
	f.out = append(f.out, code+"\r\n"...)
	if wired[code] && !f.jam {
		go func() {
			time.Sleep(f.turn)
			f.mu.Lock()
			f.out = append(f.out, "END\r\n"...)
			f.mu.Unlock()
		}()
	}
	return len(p), nil
}

func (f *fakeBoard) Read(p []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

func (f *fakeBoard) ResetInput() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = nil
	return nil
}

func (f *fakeBoard) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func serialWith(fake *fakeBoard, slots SlotMap) *Serial {
	if slots == nil {
		slots = DefaultSlotMap()
	}
	b := NewSerial("COM3", slots)
	b.openPort = func(string) (Port, error) { return fake, nil }
	b.ackTimeout = 300 * time.Millisecond
	b.endTimeout = 2 * time.Second
	return b
}

func TestDefaultSlotMapMatchesProbe(t *testing.T) {
	if got := DefaultSlotMap().String(); got != "1=AA 2=BA 3=AB 4=BB 5=AC 6=BC" {
		t.Fatalf("slot map: %s", got)
	}
}

func TestEverySlotDispenses(t *testing.T) {
	fake := &fakeBoard{turn: 200 * time.Millisecond}
	b := serialWith(fake, nil)
	for slot := 1; slot <= 6; slot++ {
		r := b.Vend(slot)
		if !r.OK || r.Reason != "" || r.EndMs < 150 || r.AckMs <= 0 || r.AckMs > r.EndMs {
			t.Fatalf("slot %d: %+v", slot, r)
		}
	}
	if fake.written[2] != "AB\r" {
		t.Fatalf("slot 3 sent %q, want AB\\r", fake.written[2])
	}
	if len(fake.written) != 6 {
		t.Fatalf("sent %d commands, want exactly one per vend", len(fake.written))
	}
}

func TestUnwiredCodeTimesOut(t *testing.T) { // TC-34 without hardware
	b := serialWith(&fakeBoard{}, SlotMap{6: "CA"})
	b.endTimeout = 600 * time.Millisecond
	r := b.Vend(6)
	if r.OK || r.Reason != ReasonMotorTimeout || r.AckMs == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestJamTimesOutWithoutResending(t *testing.T) {
	fake := &fakeBoard{jam: true}
	b := serialWith(fake, nil)
	b.endTimeout = 600 * time.Millisecond
	start := time.Now()
	r := b.Vend(4)
	if r.Reason != ReasonMotorTimeout || time.Since(start) < 550*time.Millisecond {
		t.Fatalf("%+v after %s", r, time.Since(start))
	}
	if len(fake.written) != 1 {
		t.Fatalf("sent %v: a vend must never be resent (BR-14)", fake.written)
	}
}

func TestNoEchoIsNoAck(t *testing.T) {
	r := serialWith(&fakeBoard{silent: true}, nil).Vend(1)
	if r.OK || r.Reason != ReasonNoAck {
		t.Fatalf("%+v", r)
	}
}

func TestUnexpectedLinesAreKept(t *testing.T) {
	r := serialWith(&fakeBoard{turn: 100 * time.Millisecond, noise: []byte("BOOT\r\n")}, nil).Vend(2)
	if !r.OK || r.Extra != "BOOT" {
		t.Fatalf("%+v", r)
	}
}

func TestUnknownSlot(t *testing.T) {
	fake := &fakeBoard{}
	r := serialWith(fake, nil).Vend(9)
	if r.OK || r.Reason != ReasonUnknownSlot || len(fake.written) != 0 {
		t.Fatalf("%+v, sent %v", r, fake.written)
	}
}

func TestPortErrorsCloseAndReopen(t *testing.T) { // TC-35 without hardware
	fake := &fakeBoard{turn: 50 * time.Millisecond}
	opens := 0
	b := serialWith(fake, nil)
	b.openPort = func(string) (Port, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("The system cannot find the file specified.")
		}
		return fake, nil
	}
	if r := b.Vend(1); r.Reason != ReasonPortError || !strings.Contains(r.Extra, "open COM3") {
		t.Fatalf("unplugged: %+v", r)
	}
	if r := b.Vend(1); !r.OK {
		t.Fatalf("plugged back: %+v", r)
	}
	fake.writeErr = errors.New("device removed")
	if r := b.Vend(1); r.Reason != ReasonPortError || !fake.closed {
		t.Fatalf("write error: %+v, closed=%v", r, fake.closed)
	}
	fake.writeErr = nil
	if r := b.Vend(1); !r.OK || opens != 3 {
		t.Fatalf("after the error the port is opened again: %+v, opens=%d", r, opens)
	}
}

func TestAutoDetectFailureIsAPortError(t *testing.T) {
	b := NewSerial("", DefaultSlotMap())
	b.findPort = func() (string, error) { return "", errors.New("no CH340 board found") }
	if r := b.Vend(1); r.Reason != ReasonPortError || !strings.Contains(r.Extra, "no CH340") {
		t.Fatalf("%+v", r)
	}
}

func TestSimulatorModes(t *testing.T) {
	s, err := NewSim(DefaultSlotMap(), SimOK, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	s.ackTimeout, s.endTimeout = 20*time.Millisecond, 60*time.Millisecond
	if r := s.Vend(1); !r.OK || r.Code != "AA" {
		t.Fatalf("ok: %+v", r)
	}
	_ = s.SetMode(SimJam)
	if r := s.Vend(1); r.Reason != ReasonMotorTimeout {
		t.Fatalf("jam: %+v", r)
	}
	_ = s.SetMode(SimNoAck)
	if r := s.Vend(1); r.Reason != ReasonNoAck {
		t.Fatalf("no_ack: %+v", r)
	}
	if err := s.SetMode("explode"); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
	if r := s.Vend(7); r.Reason != ReasonUnknownSlot {
		t.Fatalf("unknown slot: %+v", r)
	}
}

func TestParseSlotMap(t *testing.T) {
	m, err := ParseSlotMap(map[string]string{"1": "aa", " 2 ": "BA"})
	if err != nil || m[1] != "AA" || m[2] != "BA" {
		t.Fatalf("%v %v", m, err)
	}
	if m, _ := ParseSlotMap(nil); m.String() != DefaultSlotMap().String() {
		t.Fatal("empty means the default map")
	}
	for _, bad := range []map[string]string{
		{"0": "AA"}, {"x": "AA"}, {"1": "ZA"}, {"1": "AG"}, {"1": "A"}, {"1": "AA", "2": "AA"},
	} {
		if _, err := ParseSlotMap(bad); err == nil {
			t.Errorf("%v must be refused", bad)
		}
	}
}
