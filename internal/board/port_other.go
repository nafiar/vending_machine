//go:build !windows && !linux && !darwin

package board

import "errors"

var errUnsupported = errors.New("serial ports are supported on Windows, macOS and Linux only")

// OpenPort is not available on this system; use the simulator.
func OpenPort(name string) (Port, error) { return nil, errUnsupported }

// ListPorts is not available on this system.
func ListPorts() ([]PortInfo, error) { return nil, errUnsupported }
