//go:build linux || darwin

package board

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// On macOS and Linux the agent only runs for development, but a CH340 plugged
// into a Mac (/dev/cu.usbserial-*) or a Linux PC (/dev/ttyUSB*) works too.

type unixPort struct {
	fd int
}

// OpenPort opens a serial device at 9600 baud, 8N1, raw mode.
func OpenPort(name string) (Port, error) {
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	fail := func(err error) (Port, error) {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "configure", Path: name, Err: err}
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return fail(err)
	}
	// The same as cfmakeraw, plus 8N1, no flow control, and reads that wait at
	// most 0.1 s (VMIN 0, VTIME 1) so the driver can check its deadlines.
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON | unix.IXOFF | unix.IXANY
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 1
	setSpeed9600(t)
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		return fail(err)
	}
	if err := unix.SetNonblock(fd, false); err != nil { // VTIME applies to blocking reads
		return fail(err)
	}
	// DTR and RTS low, as on Windows. Not every device supports it (a pty doesn't).
	_ = unix.IoctlSetPointerInt(fd, unix.TIOCMBIC, unix.TIOCM_DTR|unix.TIOCM_RTS)
	return &unixPort{fd: fd}, nil
}

func (p *unixPort) Read(b []byte) (int, error) {
	for {
		n, err := unix.Read(p.fd, b)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}

func (p *unixPort) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		n, err := unix.Write(p.fd, b[written:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (p *unixPort) ResetInput() error { return flushInput(p.fd) }

func (p *unixPort) Close() error { return unix.Close(p.fd) }

// ListPorts lists USB serial devices.
func ListPorts() ([]PortInfo, error) {
	var out []PortInfo
	for _, pattern := range portPatterns {
		names, _ := filepath.Glob(pattern)
		for _, name := range names {
			desc, ch340 := describePort(name)
			out = append(out, PortInfo{Name: name, Description: desc, CH340: ch340})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func hasAny(s string, subs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
