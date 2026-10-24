//go:build linux

package board

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestRealTTY runs the driver over a pseudo-terminal, so OpenPort's termios
// setup and the line reader are tested on a real tty device. The test plays
// the board on the other end: echo after 70 ms, END after 300 ms.
func TestRealTTY(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skip("no /dev/ptmx:", err)
	}
	defer master.Close()
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skip("unlockpt:", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skip("ptsname:", err)
	}
	slave := fmt.Sprintf("/dev/pts/%d", n)

	got := make(chan string, 4)
	go func() {
		buf := make([]byte, 64)
		var cmd []byte
		for {
			k, err := master.Read(buf)
			if err != nil {
				return
			}
			cmd = append(cmd, buf[:k]...)
			for {
				i := strings.IndexByte(string(cmd), '\r')
				if i < 0 {
					break
				}
				code := string(cmd[:i])
				cmd = cmd[i+1:]
				got <- code
				time.Sleep(70 * time.Millisecond)
				master.Write([]byte(code + "\r\n"))
				if code != "CA" {
					time.Sleep(230 * time.Millisecond)
					master.Write([]byte("END\r\n"))
				}
			}
		}
	}()

	b := NewSerial(slave, SlotMap{3: "AB", 6: "CA"})
	b.endTimeout = 1500 * time.Millisecond
	defer b.Close()

	r := b.Vend(3)
	if !r.OK || r.AckMs < 60 || r.EndMs < 290 || r.EndMs > 1000 {
		t.Fatalf("vend over a tty: %+v", r)
	}
	if code := <-got; code != "AB" {
		t.Fatalf("board got %q", code)
	}
	r = b.Vend(6) // unwired: echo, no END
	if r.Reason != ReasonMotorTimeout || r.AckMs == 0 {
		t.Fatalf("unwired over a tty: %+v", r)
	}
	if name, err := b.Open(); err != nil || name != slave {
		t.Fatalf("Open: %q %v", name, err)
	}
}
