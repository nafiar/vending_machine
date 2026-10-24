# Shortcuts; every command also works without make (see README).
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || date +%Y%m%d-%H%M)
LDFLAGS := -s -w -X main.version=$(VERSION)
WINARCH ?= amd64

.PHONY: run check test build windows fmt vet

# Run the agent on this computer with config.json (the simulator, see config.dev.json).
run:
	go run ./cmd/vending-machine run

# Check config.json, the server, the machine key and the board.
check:
	go run ./cmd/vending-machine check

test:
	go test ./...

build:
	go build -ldflags "$(LDFLAGS)" -o bin/vending-machine ./cmd/vending-machine

# The folder for the vending machine, zipped: dist/vending-machine-windows.zip
# 32-bit Windows: make windows WINARCH=386
windows:
	rm -rf dist/vending-machine dist/vending-machine-windows.zip
	mkdir -p dist/vending-machine
	GOOS=windows GOARCH=$(WINARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o dist/vending-machine/vending-machine.exe ./cmd/vending-machine
	cp config.example.json scripts/windows/start-kiosk.cmd README.md dist/vending-machine/
	cd dist && zip -qr vending-machine-windows.zip vending-machine
	@printf '%s\n' 'Ready: dist/vending-machine-windows.zip (unzip it on the machine to C:\vending-machine)'

fmt:
	gofmt -w .

vet:
	go vet ./...
