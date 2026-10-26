//go:build !windows

// Package winsvc runs the agent as a Windows service. Elsewhere it does nothing.
package winsvc

import (
	"context"
	"errors"
)

const Name = "VendingMachine"

var errWindowsOnly = errors.New("the service commands are for the Windows machine; on this computer run `vending-machine run` in a terminal")

func IsService() bool                               { return false }
func Run(run func(ctx context.Context) error) error { return errWindowsOnly }
func Install(exe, configPath string) error          { return errWindowsOnly }
func Uninstall() error                              { return errWindowsOnly }
func Start() error                                  { return errWindowsOnly }
func Stop() error                                   { return errWindowsOnly }
func Status() (string, error)                       { return "", errWindowsOnly }
