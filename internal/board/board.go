// Package board drives the vending machine's motor board, or simulates it.
//
// Protocol, as measured with coil_probe.py on VM-01 (22 Oct 2026):
//
//	agent -> board   <column><row>\r      e.g. "AB\r"  (column A-H, row A-F, 9600 baud, 8N1)
//	board -> agent   <column><row>\r\n    echo: command received
//	board -> agent   END\r\n              the motor finished its turn
//
// A code with no coil wired to it (CA, DA, EA, FA on VM-01) is echoed but never
// gets END. The echo is not validation: the board repeats the last two
// characters it received, so the driver checks that the echo equals the code it
// sent and treats END as the confirmation. A command sent while a coil turns is
// queued and runs after the first END, so a vend is never resent (PRD BR-14).
//
// Measured over 18 vends: echo after 62-78 ms, END after 2.31-2.64 s.
package board

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Baud       = 9600
	AckTimeout = 500 * time.Millisecond // FR-08: no echo within 0.5 s -> NO_ACK
	EndTimeout = 5 * time.Second        // FR-08: no END within 5 s of the command -> MOTOR_TIMEOUT
)

// Failure reasons, as the server records them on a FAILED transaction.
const (
	ReasonNoAck        = "NO_ACK"        // no echo: cable, port or board down
	ReasonMotorTimeout = "MOTOR_TIMEOUT" // echo but no END: jam, no motor, no power
	ReasonPortError    = "PORT_ERROR"    // the serial port could not be opened, read or written
	ReasonUnknownSlot  = "UNKNOWN_SLOT"  // the slot number is not in the slot map
)

// Result is the outcome of one vend command.
type Result struct {
	OK     bool   `json:"ok"`
	Slot   int    `json:"slot"`
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
	AckMs  int64  `json:"ackMs,omitempty"` // milliseconds until the echo
	EndMs  int64  `json:"endMs,omitempty"` // milliseconds until END
	Extra  string `json:"extra,omitempty"` // unexpected lines from the board, or the port error
}

// Board turns one coil per call. Implementations allow one vend at a time.
type Board interface {
	Vend(slot int) Result
	Describe() string
	Close() error
}

// SlotMap maps the slot number the server and kiosk use (1..N) to the board
// code of the coil wired to it. The wiring is per machine, so it lives in the
// agent's configuration and the server keeps using plain slot numbers.
type SlotMap map[int]string

// DefaultSlotMap is VM-01's wiring as measured:
// top row 1 = AA, 2 = BA; middle row 3 = AB, 4 = BB; bottom row 5 = AC, 6 = BC.
func DefaultSlotMap() SlotMap {
	return SlotMap{1: "AA", 2: "BA", 3: "AB", 4: "BB", 5: "AC", 6: "BC"}
}

// ParseSlotMap reads the "slotMap" setting, e.g. {"1": "AA", "2": "BA"}.
// An empty setting gives the default map.
func ParseSlotMap(raw map[string]string) (SlotMap, error) {
	if len(raw) == 0 {
		return DefaultSlotMap(), nil
	}
	m := SlotMap{}
	usedBy := map[string]int{}
	for k, v := range raw {
		n, err := strconv.Atoi(strings.TrimSpace(k))
		if err != nil || n < 1 || n > 60 {
			return nil, fmt.Errorf("slotMap: %q is not a slot number (1 to 60)", k)
		}
		code := strings.ToUpper(strings.TrimSpace(v))
		if !ValidCode(code) {
			return nil, fmt.Errorf("slotMap: slot %d: %q is not a board code (column A-H, then row A-F, e.g. AB)", n, v)
		}
		if other, dup := usedBy[code]; dup {
			a, b := min(n, other), max(n, other)
			return nil, fmt.Errorf("slotMap: slots %d and %d both use %s", a, b, code)
		}
		usedBy[code] = n
		m[n] = code
	}
	return m, nil
}

// ValidCode reports whether code is a column A-H followed by a row A-F.
func ValidCode(code string) bool {
	return len(code) == 2 && code[0] >= 'A' && code[0] <= 'H' && code[1] >= 'A' && code[1] <= 'F'
}

// String lists the map in slot order, e.g. "1=AA 2=BA".
func (m SlotMap) String() string {
	slots := make([]int, 0, len(m))
	for s := range m {
		slots = append(slots, s)
	}
	sort.Ints(slots)
	parts := make([]string, len(slots))
	for i, s := range slots {
		parts[i] = fmt.Sprintf("%d=%s", s, m[s])
	}
	return strings.Join(parts, " ")
}

func millis(d time.Duration) int64 { return d.Milliseconds() }

// printable keeps a board line readable in the log.
func printable(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}
