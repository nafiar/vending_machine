//go:build linux

package board

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

var portPatterns = []string{"/dev/ttyUSB*", "/dev/ttyACM*"}

func setSpeed9600(t *unix.Termios) {
	t.Cflag &^= unix.CBAUD
	t.Cflag |= unix.B9600
	t.Ispeed = unix.B9600
	t.Ospeed = unix.B9600
}

func flushInput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }

// describePort names the kernel driver behind /dev/ttyUSBn; the CH340 uses ch341.
func describePort(name string) (string, bool) {
	link, err := os.Readlink(filepath.Join("/sys/class/tty", filepath.Base(name), "device", "driver"))
	if err != nil {
		return "", false
	}
	driver := filepath.Base(link)
	return "driver " + driver, hasAny(driver, "ch341")
}
