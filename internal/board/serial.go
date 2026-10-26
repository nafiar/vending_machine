package board

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Port is an open serial port. Read waits briefly (about 50-100 ms) and then
// returns 0, nil when nothing arrived, so callers can check their deadlines.
type Port interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	ResetInput() error // drop anything received and not read yet
	Close() error
}

// Serial is the real motor board on a serial port (a CH340 USB adapter on VM-01).
// The port is opened on the first vend and kept open; after an error it is
// closed and opened again on the next vend, so re-plugging the cable recovers.
type Serial struct {
	mu         sync.Mutex
	portName   string // from the configuration; "" finds the CH340 by itself
	slots      SlotMap
	ackTimeout time.Duration
	endTimeout time.Duration
	openPort   func(name string) (Port, error)
	findPort   func() (string, error)
	port       Port
	current    string // name of the open port
}

// NewSerial returns the driver for the board on portName ("" = find the CH340).
func NewSerial(portName string, slots SlotMap) *Serial {
	return &Serial{
		portName:   strings.TrimSpace(portName),
		slots:      slots,
		ackTimeout: AckTimeout,
		endTimeout: EndTimeout,
		openPort:   OpenPort,
		findPort:   FindBoardPort,
	}
}

// Describe names the port for logs and the status page.
func (b *Serial) Describe() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.current != "":
		return "serial " + b.current
	case b.portName != "":
		return "serial " + b.portName
	default:
		return "serial (CH340, found at first use)"
	}
}

// Open opens the port now instead of at the first vend, to check the setup.
func (b *Serial) Open() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.ensureOpen(); err != nil {
		return "", err
	}
	return b.current, nil
}

// Close releases the port.
func (b *Serial) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeLocked()
	return nil
}

func (b *Serial) ensureOpen() error {
	if b.port != nil {
		return nil
	}
	name := b.portName
	if name == "" {
		found, err := b.findPort()
		if err != nil {
			return err
		}
		name = found
	}
	p, err := b.openPort(name)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	b.port, b.current = p, name
	return nil
}

func (b *Serial) closeLocked() {
	if b.port != nil {
		_ = b.port.Close()
		b.port = nil
	}
}

// Vend sends one command and waits for the echo, then for END. It never resends.
func (b *Serial) Vend(slot int) Result {
	code, ok := b.slots[slot]
	if !ok {
		return Result{Slot: slot, Reason: ReasonUnknownSlot}
	}
	b.mu.Lock() // one vend at a time: the board would queue a second command
	defer b.mu.Unlock()

	res := Result{Slot: slot, Code: code}
	var extra []string
	finish := func(reason string) Result {
		res.Reason = reason
		res.Extra = strings.Join(extra, " | ")
		return res
	}
	portFailed := func(err error) Result {
		b.closeLocked()
		extra = append(extra, err.Error())
		return finish(ReasonPortError)
	}

	if err := b.ensureOpen(); err != nil {
		return portFailed(err)
	}
	if err := b.port.ResetInput(); err != nil {
		return portFailed(err)
	}
	lines := lineReader{port: b.port}
	start := time.Now()
	if _, err := b.port.Write([]byte{code[0], code[1], '\r'}); err != nil {
		return portFailed(err)
	}

	// 1. The echo: the board heard the command.
	for {
		line, ok, err := lines.next(start.Add(b.ackTimeout))
		if err != nil {
			return portFailed(err)
		}
		if !ok {
			return finish(ReasonNoAck)
		}
		if line == code {
			res.AckMs = millis(time.Since(start))
			break
		}
		if line != "" {
			extra = append(extra, printable(line))
		}
	}
	// 2. END: the coil completed a turn. It does not prove the product fell
	// (there is no drop sensor), hence "Produk tidak keluar?" on the kiosk.
	for {
		line, ok, err := lines.next(start.Add(b.endTimeout))
		if err != nil {
			return portFailed(err)
		}
		if !ok {
			return finish(ReasonMotorTimeout)
		}
		if line == "END" {
			res.EndMs = millis(time.Since(start))
			res.OK = true
			return finish("")
		}
		if line != "" {
			extra = append(extra, printable(line))
		}
	}
}

// lineReader splits what the board sends into lines ending in "\n".
type lineReader struct {
	port  Port
	buf   []byte
	chunk [64]byte
}

// next returns the next line without "\r", "\n" and surrounding spaces. ok is
// false when the deadline passed before a whole line arrived.
func (r *lineReader) next(deadline time.Time) (line string, ok bool, err error) {
	for {
		if i := bytes.IndexByte(r.buf, '\n'); i >= 0 {
			line = strings.Trim(string(r.buf[:i]), "\r\n ")
			r.buf = r.buf[i+1:]
			return line, true, nil
		}
		if !time.Now().Before(deadline) {
			return "", false, nil
		}
		n, err := r.port.Read(r.chunk[:])
		if n > 0 {
			r.buf = append(r.buf, r.chunk[:n]...)
		}
		if err != nil {
			return "", false, err
		}
	}
}

// PortInfo describes one serial port found on this computer.
type PortInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CH340       bool   `json:"ch340"` // looks like the motor board's USB adapter (WCH CH340, USB VID 1A86)
}

// FindBoardPort returns the port of the CH340 adapter. It fails when there is
// none or more than one, and the error lists the ports it saw.
func FindBoardPort() (string, error) {
	ports, err := ListPorts()
	if err != nil {
		return "", fmt.Errorf("list serial ports: %w", err)
	}
	var matches []string
	for _, p := range ports {
		if p.CH340 {
			matches = append(matches, p.Name)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if len(ports) == 0 {
			return "", fmt.Errorf("no serial port found: check the board's USB cable, or set serialPort in config.json")
		}
		names := make([]string, len(ports))
		for i, p := range ports {
			names[i] = p.Name
		}
		return "", fmt.Errorf("no CH340 board found (ports: %s): check the USB cable, or set serialPort in config.json", strings.Join(names, ", "))
	default:
		return "", fmt.Errorf("more than one CH340 port (%s): set serialPort in config.json", strings.Join(matches, ", "))
	}
}
