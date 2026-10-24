//go:build windows

package board

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type winPort struct {
	h windows.Handle
}

// dcbBinary is DCB.fBinary. Every other flag stays 0: no parity check, no
// hardware or XON/XOFF flow control, DTR and RTS control disabled (lines low,
// as the vendor's test scripts and board.py open the port), no abort on error.
const dcbBinary = 0x00000001

// OpenPort opens a COM port at 9600 baud, 8 data bits, no parity, 1 stop bit.
func OpenPort(name string) (Port, error) {
	path := name
	if !strings.HasPrefix(path, `\\.\`) {
		path = `\\.\` + name // required from COM10 up, harmless below
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf("%w (another program has the port open, e.g. the VendingMachine service or the vendor app)", err)
		}
		return nil, err
	}
	fail := func(step string, err error) (Port, error) {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("%s: %w", step, err)
	}

	var dcb windows.DCB
	dcb.DCBlength = uint32(unsafe.Sizeof(dcb))
	if err := windows.GetCommState(h, &dcb); err != nil {
		return fail("GetCommState", err)
	}
	dcb.BaudRate = Baud
	dcb.ByteSize = 8
	dcb.Parity = windows.NOPARITY
	dcb.StopBits = windows.ONESTOPBIT
	dcb.Flags = dcbBinary
	if err := windows.SetCommState(h, &dcb); err != nil {
		return fail("SetCommState", err)
	}
	_ = windows.EscapeCommFunction(h, windows.CLRDTR)
	_ = windows.EscapeCommFunction(h, windows.CLRRTS)

	// ReadFile returns at once with whatever has arrived, or waits up to 50 ms
	// for the first byte and then returns 0 bytes (the documented MAXDWORD case).
	timeouts := windows.CommTimeouts{
		ReadIntervalTimeout:        0xFFFFFFFF,
		ReadTotalTimeoutMultiplier: 0xFFFFFFFF,
		ReadTotalTimeoutConstant:   50,
		WriteTotalTimeoutConstant:  1000,
	}
	if err := windows.SetCommTimeouts(h, &timeouts); err != nil {
		return fail("SetCommTimeouts", err)
	}
	_ = windows.PurgeComm(h, windows.PURGE_RXCLEAR|windows.PURGE_TXCLEAR|windows.PURGE_RXABORT|windows.PURGE_TXABORT)
	return &winPort{h: h}, nil
}

func (p *winPort) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(p.h, b, &n, nil)
	return int(n), err
}

func (p *winPort) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(p.h, b, &n, nil)
	return int(n), err
}

func (p *winPort) ResetInput() error {
	return windows.PurgeComm(p.h, windows.PURGE_RXCLEAR|windows.PURGE_RXABORT)
}

func (p *winPort) Close() error { return windows.CloseHandle(p.h) }

// The "Ports (COM & LPT)" device class.
var guidDevClassPorts = windows.GUID{Data1: 0x4D36E978, Data2: 0xE325, Data3: 0x11CE, Data4: [8]byte{0xBF, 0xC1, 0x08, 0x00, 0x2B, 0xE1, 0x03, 0x18}}

var comInName = regexp.MustCompile(`\((COM\d+)\)`)

// ListPorts lists the COM ports, with the Device Manager name of each, e.g.
// "USB-SERIAL CH340 (COM3)".
func ListPorts() ([]PortInfo, error) {
	byName := map[string]*PortInfo{}

	// Every COM port Windows knows about right now.
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE); err == nil {
		values, _ := k.ReadValueNames(0)
		for _, v := range values {
			if com, _, err := k.GetStringValue(v); err == nil && com != "" {
				byName[strings.ToUpper(com)] = &PortInfo{Name: strings.ToUpper(com)}
			}
		}
		k.Close()
	}

	// Names and USB ids from the device manager, to recognise the CH340.
	set, err := windows.SetupDiGetClassDevsEx(&guidDevClassPorts, "", 0, windows.DIGCF_PRESENT, 0, "")
	if err == nil {
		defer windows.SetupDiDestroyDeviceInfoList(set)
		for i := 0; ; i++ {
			data, err := windows.SetupDiEnumDeviceInfo(set, i)
			if err != nil {
				break // ERROR_NO_MORE_ITEMS
			}
			friendly, _ := windows.SetupDiGetDeviceRegistryProperty(set, data, windows.SPDRP_FRIENDLYNAME)
			hwids, _ := windows.SetupDiGetDeviceRegistryProperty(set, data, windows.SPDRP_HARDWAREID)
			name := ""
			if key, err := windows.SetupDiOpenDevRegKey(set, data, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, registry.QUERY_VALUE); err == nil {
				k := registry.Key(key)
				name, _, _ = k.GetStringValue("PortName")
				k.Close()
			}
			desc, _ := friendly.(string)
			if name == "" {
				if m := comInName.FindStringSubmatch(desc); m != nil {
					name = m[1]
				}
			}
			name = strings.ToUpper(name)
			if !strings.HasPrefix(name, "COM") {
				continue // e.g. a printer port
			}
			info := byName[name]
			if info == nil {
				info = &PortInfo{Name: name}
				byName[name] = info
			}
			info.Description = desc
			ids := strings.ToUpper(desc)
			switch v := hwids.(type) {
			case []string:
				ids += " " + strings.ToUpper(strings.Join(v, " "))
			case string:
				ids += " " + strings.ToUpper(v)
			}
			info.CH340 = strings.Contains(ids, "CH340") || strings.Contains(ids, "CH341") || strings.Contains(ids, "VID_1A86")
		}
	}

	out := make([]PortInfo, 0, len(byName))
	for _, p := range byName {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return comNumber(out[i].Name) < comNumber(out[j].Name) })
	return out, nil
}

func comNumber(name string) int {
	n := 0
	for _, c := range strings.TrimPrefix(name, "COM") {
		if c < '0' || c > '9' {
			return 1 << 30
		}
		n = n*10 + int(c-'0')
	}
	return n
}
