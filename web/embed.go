// Package web holds the pages the agent serves: the kiosk itself at / and the
// developer page at /dev/. They are built into the program, so the machine
// needs no other files.
package web

import "embed"

//go:embed kiosk dev
var Files embed.FS
