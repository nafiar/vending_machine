//go:build darwin

package board

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// The call-out devices; Bluetooth and the debug console are left out by name.
var portPatterns = []string{"/dev/cu.usbserial*", "/dev/cu.wchusbserial*", "/dev/cu.usbmodem*"}

func setSpeed9600(t *unix.Termios) {
	t.Ispeed = 9600 // on macOS the speed fields hold the plain number
	t.Ospeed = 9600
}

// flushInput is tcflush(fd, TCIFLUSH): TIOCFLUSH with FREAD (1).
func flushInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 1) }

// describePort recognises the CH340: Apple's driver names it cu.usbserial-*,
// the WCH driver cu.wchusbserial*.
func describePort(name string) (string, bool) {
	base := filepath.Base(name)
	return "USB serial", hasAny(base, "wchusbserial", "usbserial")
}
