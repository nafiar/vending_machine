//go:build windows

// Package winsvc runs the agent as a Windows service: it starts at boot
// before anyone logs in, runs without a window, and Windows restarts it if it
// stops unexpectedly.
package winsvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	Name        = "VendingMachine"
	DisplayName = "Vending Machine Kiosk Agent"
	description = "Serves the kiosk page at http://127.0.0.1:8080, forwards it to the vending server and drives the motor board."
)

// IsService reports whether the program was started by the service manager.
func IsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

type handler struct {
	run func(ctx context.Context) error
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1 // a failure: the recovery actions restart the service
			}
			return false, 0
		case c := <-requests:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 30000}
				cancel() // the agent finishes a vend in progress, then stops
				select {
				case <-done:
				case <-time.After(30 * time.Second):
				}
				return false, 0
			}
		}
	}
}

// Run hands the program to the service manager until the service stops.
func Run(run func(ctx context.Context) error) error {
	return svc.Run(Name, &handler{run: run})
}

// Install registers the service to start at boot with this program and config
// file, restarting it 5 s after any failure, and starts it.
func Install(exe, configPath string) error {
	m, err := connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(Name); err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists: run `vending-machine.exe service uninstall` first", Name)
	}
	s, err := m.CreateService(Name, exe, mgr.Config{
		DisplayName: DisplayName,
		Description: description,
		StartType:   mgr.StartAutomatic,
	}, "run", "-config", configPath)
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
	if err := s.SetRecoveryActions(actions, uint32((24 * time.Hour).Seconds())); err != nil {
		return fmt.Errorf("set restart on failure: %w", err)
	}
	_ = s.SetRecoveryActionsOnNonCrashFailures(true)
	if err := s.Start(); err != nil {
		return fmt.Errorf("installed, but it did not start: %w (see data\\logs\\vending-machine.log)", err)
	}
	return nil
}

// Uninstall stops the service and removes it.
func Uninstall() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	_ = stop(s)
	if err := s.Delete(); err != nil {
		return fmt.Errorf("remove service: %w", err)
	}
	return nil
}

// Start starts the installed service.
func Start() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return s.Start()
}

// Stop stops the service and waits until it has stopped.
func Stop() error {
	m, s, err := open()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	return stop(s)
}

// Status describes the service state, e.g. "running".
func Status() (string, error) {
	m, s, err := open()
	if err != nil {
		return "", err
	}
	defer m.Disconnect()
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return "", err
	}
	names := map[svc.State]string{
		svc.Stopped: "stopped", svc.StartPending: "starting", svc.StopPending: "stopping", svc.Running: "running",
		svc.ContinuePending: "continuing", svc.PausePending: "pausing", svc.Paused: "paused",
	}
	return names[st.State], nil
}

func stop(s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return nil
		}
		return fmt.Errorf("stop service: %w", err)
	}
	deadline := time.Now().Add(40 * time.Second)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("the service did not stop within 40 s")
		}
		time.Sleep(300 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	return nil
}

func connect() (*mgr.Mgr, error) {
	m, err := mgr.Connect()
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, errors.New("access denied: open the Command Prompt with \"Run as administrator\"")
		}
		return nil, err
	}
	return m, nil
}

func open() (*mgr.Mgr, *mgr.Service, error) {
	m, err := connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(Name)
	if err != nil {
		m.Disconnect()
		return nil, nil, fmt.Errorf("service %s is not installed (run `vending-machine.exe service install`): %w", Name, err)
	}
	return m, s, nil
}
