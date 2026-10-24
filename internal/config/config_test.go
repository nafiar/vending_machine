package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	path := write(t, `{"serverUrl": "https://35.219.98.32/", "machineKey": "vmk_abc"}`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerURL != "https://35.219.98.32" || c.Listen != "127.0.0.1:8080" || c.Board != "serial" ||
		c.Slots.String() != "1=AA 2=BA 3=AB 4=BB 5=AC 6=BC" || c.SimTurnMs != 2500 || c.HeartbeatS != 300 ||
		c.DataDir != filepath.Join(filepath.Dir(path), "data") || c.LogLevel != "info" {
		t.Fatalf("%+v", c)
	}
}

func TestMistakesAreExplained(t *testing.T) {
	cases := map[string]string{
		`{"serverUrl": "35.219.98.32", "machineKey": "vmk_a"}`:                          "serverUrl",
		`{"serverUrl": "https://x.y/api", "machineKey": "vmk_a"}`:                       "serverUrl",
		`{"serverUrl": "https://x.y", "machineKey": "` + Placeholder + `"}`:             "machineKey is not set",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a b"}`:                         "spaces",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "board": "usb"}`:           "board",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "slotMap": {"1": "ZZ"}}`:   "board code",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "simMode": "broken"}`:      "simulator mode",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "comPort": "COM3"}`:        "unknown field",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "listen": "8080"}`:         "listen",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "heartbeatSeconds": 1}`:    "heartbeatSeconds",
		`{"serverUrl": "https://x.y", "machineKey": "vmk_a", "logLevel": "everything"}`: "logLevel",
	}
	for body, want := range cases {
		if _, err := Load(write(t, body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error about %q", body, err, want)
		}
	}
}
