# vending-machine: the Kiosk Agent

This is the program that runs on each vending machine's PC (PRD v3, decision D12). It is one Go program, `vending-machine`, that:

- serves the **kiosk page** (the six customer and operator screens) to Edge at `http://127.0.0.1:8080`;
- forwards every call from the page to **vending-server** with the machine's key, so the key never sits in browser code;
- turns the coils of the **motor board** on the COM port, or of a **simulator** when there is no hardware;
- keeps a small **journal** on disk, so a power cut or a network drop never loses a sale's result and never turns a coil twice.

```
 Edge (kiosk mode) ──► vending-machine on the machine's PC ──► vending-server (GCP VM) ──► MySQL
   kiosk page          127.0.0.1:8080, machine key added       https://35.219.98.32
                              │
                              └──► motor board (CH340 USB, COM3)  or the simulator
```

There is no database on the machine. The server holds products, prices, stock and transactions. The agent keeps only what the server hasn't confirmed yet (the journal), a copy of the product images, and its log.

You can test the whole thing on your Mac first, with the simulator in place of the board. Part 1 covers that; Part 2 installs it on the machine.

| Folder | What is in it |
| --- | --- |
| `cmd/vending-machine` | The program and its commands (`run`, `check`, `ports`, `vend`, `service`) |
| `internal/agent` | The purchase flow, the journal recovery, the catalog cache, the forwarding to the server |
| `internal/board` | The motor board driver (echo + END protocol) and the simulator |
| `internal/journal` | The on-disk journal: one small file per unfinished purchase |
| `web/kiosk` | The kiosk page (HTML, CSS, JavaScript), built into the program |
| `web/dev` | The developer page: the kiosk in a phone-sized frame, plus the simulator controls |
| `scripts/windows` | `start-kiosk.cmd`, which opens Edge in kiosk mode at logon |

---

## Part 1: test it on your Mac

You need **Go 1.24 or newer**, a browser, and **vending-server** running somewhere: on your Mac, or on the GCP VM. No motor board is needed, since the simulator stands in for it.

The agent only works with the server, because the kiosk can't sell offline (D1). So the first step is the server.

### Step 1: start the server

Pick one of the two options.

**Option A: the server on your Mac.** In the `vending-server` folder, with your local MySQL set in `.env` (see that README):

```bash
go run ./cmd/vending-server seed-demo   # only once: demo data, prints the logins and the machine keys
go run ./cmd/vending-server             # the server, on http://localhost:8080
```

Copy the **VM-01** key from the seed output, a line like `VM-01  vmk_T_utxgVx0d...`. If you seeded earlier and no longer have the key, print a new one with `go run ./cmd/vending-server rotate-key -machine VM-01`.

**Option B: the server on the GCP VM.** From the `vending-server` folder on your Mac:

```bash
./deploy/deploy.sh YOUR_USER@35.219.98.32 keys
```

Use the **VM-02** key for your Mac and keep VM-01 for the real machine. Two agents with the same key would both act as the same machine.

> This version of the agent works best with the latest vending-server: the server's pay answer now carries the slot number. Pull it, and on the VM run `./deploy/deploy.sh YOUR_USER@35.219.98.32 update` once. An older server still works; the agent then uses the slot it saw when the transaction was created.

### Step 2: make your config.json

In this folder:

```bash
cp config.dev.json config.json
```

Open `config.json` and paste the key:

```json
{
  "serverUrl": "http://localhost:8080",
  "machineKey": "vmk_T_utxgVx0dXWKiADQIYIXZDvsN6XtfHyqIE2EZRvJLg",
  "listen": "127.0.0.1:8090",
  "board": "sim",
  "simMode": "ok",
  "simTurnMs": 2500
}
```

- **`serverUrl`**: for option B, use `https://35.219.98.32` instead.
- **`listen`**: on your Mac the server already uses port 8080, so the agent takes 8090. On the machine it is 8080.
- **`board`**: `sim` is the simulator. `simTurnMs` is how long a good turn takes; 2500 ms is close to the 2.45 s measured on VM-01.

`config.json` holds the key, so `.gitignore` keeps it out of git.

### Step 3: check the setup

```bash
go run ./cmd/vending-machine check
```

Each line should start with `OK`:

```
OK   server    reachable, machine key accepted: VM-01 (Lobby Gedung A), catalog version 191
          slot 1 -> AA             Gacha Legendary, Rp50000 (available)
          ...
OK   board     simulator (ok): no hardware needed
OK   journal   empty (.../data/journal)
```

If `server` fails, the message says why: the server isn't running, the URL is wrong, or the key is wrong.

### Step 4: run the agent

```bash
go run ./cmd/vending-machine        # or: make run
```

Then open the **developer page** at <http://127.0.0.1:8090/dev/>.

- **Left:** the kiosk at the size of the machine's screen (portrait 600 × 1024; one button switches to landscape). You click where a customer would tap.
- **Right:** the simulator switches, the server status, the journal and the last vends, all updated live.

The kiosk alone, as Edge will show it on the machine, is at <http://127.0.0.1:8090/>. The admin web is at <http://localhost:8080/> (option A) or `https://35.219.98.32` (option B); keep it open in another tab to watch each sale arrive. Stop the agent with Ctrl+C.

### Step 5: run through the test cases

Each row is a test case from the PRD that you can run without the hardware. The demo operator login is `budi` / `budi123`.

| Test | What to do | What the kiosk shows | What to check in the admin web |
| --- | --- | --- | --- |
| TC-01 sale | Tap a product, **Bayar**, **Konfirmasi bayar** | "Produk sedang dikeluarkan…" for 2.5 s, then "Silakan ambil produk" | Transactions: the sale is DISPENSED at its price; the slot's count is 1 lower |
| TC-12 report | After a sale, tap **Produk tidak keluar?** within 60 s | "Laporan diterima" with the transaction ID | The sale is still DISPENSED, under the customer-reported filter |
| TC-03 cancel | On the payment screen, tap the back arrow | The home screen | CANCELLED; the count is unchanged |
| TC-04 timeout | On the payment screen, wait 60 s | Back to the home screen with "Waktu pembayaran habis." | EXPIRED |
| TC-06 jam | Developer page: **Jam**. Then buy | After 5 s: "Maaf, terjadi gangguan" and a transaction ID; back home, the slot shows **Habis** | FAILED, MOTOR_TIMEOUT; the slot is blocked ("Perlu dicek") |
| No echo | Developer page: **No echo**. Then buy | After 0.5 s: "Maaf, terjadi gangguan" | FAILED, NO_ACK |
| TC-07 offline | Stop the server (Ctrl+C in its terminal), then tap a product | "Mesin offline" within a few seconds; buying is blocked | Start the server again: the kiosk recovers by itself within about 5 s |
| Result delivered later | Start a purchase and stop the server while "Produk sedang dikeluarkan…" shows | "Silakan ambil produk": the customer still gets the product | The developer page's journal shows 1 entry; start the server and it empties within 10 s, then the sale shows as DISPENSED once |
| TC-37 power cut | Developer page: **Power cut on the next vend**. Then buy | The agent stops right after it sends the vend command (your terminal shows it exited) | Start the agent again: the purchase is FAILED with AGENT_RESTART and the coil is not turned again (BR-14) |
| TC-10 price change | In the admin web, change the price of a product the kiosk shows. On the kiosk, tap it and **Bayar** | "Info produk diperbarui. Silakan pilih lagi." and the new price | Nothing charged; the change log has the new price |
| TC-13 restock | Hold the top-right corner of the kiosk for 3 s, log in, set the counts with − and +, **Simpan**, **Konfirmasi** | "Restock tersimpan. Terima kasih!"; blocked slots are sellable again | Restock history: one visit with a line per slot |
| TC-17 product change | In the admin web, plan another product for a slot. Do a restock visit | The new product appears right after **Konfirmasi** | The visit line marks the product change |
| Machine switched off | In the admin web, deactivate the machine, then tap a product | "Mesin offline" with "Mesin ini dinonaktifkan di panel admin." | Activate it again: the kiosk recovers by itself |
| TC-33 last contact | Stop the agent for 10 minutes | | The recap shows the machine's last contact 10 minutes ago |

The simulator mode stays until you change it, so switch back to **Dispense OK** after the failure tests.

### Step 6: the automated tests

```bash
go test ./...        # or: make test
```

They take about 10 seconds and need neither the server nor the board.

- **Board driver:** the cases from `test_board.py` against a fake board, plus one run over a real pseudo-terminal on Linux, so the serial settings themselves are tested.
- **Journal:** files survive a restart, and a damaged file is set aside.
- **Agent:** every purchase path against a fake server with the same rules as vending-server. That covers a sale, a jam, the server going away mid-vend, a restart mid-vend, a lost pay answer, an expired payment, a repeated pay, the catalog cache, a refused key, restock ordering and the image cache.

### Optional: the real board on your Mac

If you have the motor board with you, plug its USB cable into the Mac. macOS has a CH340 driver built in.

```bash
go run ./cmd/vending-machine ports              # e.g. * /dev/cu.usbserial-110
go run ./cmd/vending-machine vend 1             # turns slot 1's coil once
```

Set `"board": "serial"` in `config.json` to use it from the kiosk. `"serialPort": ""` finds the CH340 by itself.

---

## Part 2: install on the vending machine

The machine's PC runs Windows 10 with Edge, and the board is on a USB port (CH340, COM3 on VM-01). The agent runs there as a **Windows service**. It starts at boot before anyone logs in, runs without a window, and Windows restarts it 5 s after a failure. A small script opens Edge in kiosk mode when the PC logs in.

### Step 1: build the package on your Mac

```bash
make windows
```

This gives you `dist/vending-machine-windows.zip`, containing `vending-machine.exe`, `config.example.json`, `start-kiosk.cmd` and this README. The kiosk page is built into the `.exe`. Without `make`:

```bash
GOOS=windows GOARCH=amd64 go build -o vending-machine.exe ./cmd/vending-machine
```

For a 32-bit Windows, use `make windows WINARCH=386`. To check which you have: Settings → System → About → System type.

### Step 2: copy it to the machine

Unzip it to `C:\vending-machine`, so the program is `C:\vending-machine\vending-machine.exe`. A USB stick works, or a download.

### Step 3: config.json

In `C:\vending-machine`, copy `config.example.json` to `config.json` and paste VM-01's key into it, using Notepad:

```json
{
  "serverUrl": "https://35.219.98.32",
  "machineKey": "vmk_...",
  "listen": "127.0.0.1:8080",
  "board": "serial",
  "serialPort": "",
  "slotMap": { "1": "AA", "2": "BA", "3": "AB", "4": "BB", "5": "AC", "6": "BC" }
}
```

The slot map is VM-01's wiring as measured on 22 Oct. Another machine may be wired differently, and this is the only place that knows.

### Step 4: test the board directly

Close the vendor's app first: only one program can have the COM port open. Then open **Command Prompt** in `C:\vending-machine` (in File Explorer, type `cmd` in the address bar and press Enter):

```
vending-machine.exe ports
vending-machine.exe vend 1
```

- **`ports`** lists the COM ports and marks the CH340 with `*`.
- **`vend 1`** turns slot 1's coil once and prints the echo and END times, e.g. `"ackMs": 70, "endMs": 2450`.

Do this for slots 1 to 6, and check that each turns the coil you expect. Nothing is recorded on the server, so a test vend leaves the counts alone; put the product back.

### Step 5: check everything

```
vending-machine.exe check
```

Every line should say `OK`: the server and key, the board's port, and the journal.

### Step 6: install the service

Open **Command Prompt as administrator** (Start, type `cmd`, right-click, Run as administrator):

```
cd C:\vending-machine
vending-machine.exe service install
```

It installs the service **VendingMachine**, starts it, and sets it to start at every boot. Check it with `vending-machine.exe service status`, or open <http://127.0.0.1:8080/dev/> in Edge.

### Step 7: open the kiosk at logon

1. Press Win+R, type `shell:startup` and press Enter. Copy `start-kiosk.cmd` into that folder. At every logon it waits for the agent, then starts Edge full screen on the kiosk.
2. **Log in automatically:** run `netplwiz` and untick "Users must enter a user name and password to use this computer".
3. **Never sleep:** Settings → System → Power & sleep, set screen and sleep to **Never**.

Restart the PC. The kiosk should come up by itself. To get to the desktop for maintenance, press Alt+F4 to close Edge; if that doesn't work, press Ctrl+Alt+Del, open Task Manager and end Microsoft Edge.

### Step 8: the hardware test cases

| Test | What to do | Expected |
| --- | --- | --- |
| TC-36 | Buy once from each slot 1 to 6 | The matching coil turns (1 = AA … 6 = BC); each sale DISPENSED; its END time is in the log |
| TC-34 | Map slot 6 to `"CA"` (an unwired code) in `config.json`, restart the service, buy from slot 6 | After 5 s: FAILED with MOTOR_TIMEOUT, slot blocked. Put `"BC"` back afterwards |
| TC-35 | Unplug the board's USB cable, buy a product | FAILED with NO_ACK or PORT_ERROR; no coil turns. Plug it back: the next sale works, since the agent opens the port again by itself |
| TC-37 | While "Produk sedang dikeluarkan…" shows, pull the PC's power, or end `vending-machine.exe` in Task Manager (Details tab → End task; Windows starts the service again after 5 s) | After the restart: FAILED with AGENT_RESTART, and the coil is not turned again. Stopping the service normally waits for the sale to finish instead |

After changing `config.json`, restart the service: `vending-machine.exe service stop`, then `vending-machine.exe service start`.

### Updating the agent

Build a new `vending-machine.exe` on your Mac, then, as administrator on the machine:

```
vending-machine.exe service stop
(replace vending-machine.exe)
vending-machine.exe service start
```

The journal, `config.json` and the logs stay. A purchase in progress finishes before the service stops.

---

## When something goes wrong

The log is `data\logs\vending-machine.log`, next to the program (`data/logs/` on your Mac). There is one line per event, such as `msg=vend tx=... slot=3 code=AB result=DISPENSED ack_ms=70 end_ms=2450`. The developer page `/dev/` shows the same state live.

| You see | Likely cause | What to do |
| --- | --- | --- |
| "Mesin offline" | The server is unreachable | Check the internet and that the VM is up (`./deploy/deploy.sh ... status`). The kiosk recovers by itself |
| "Mesin offline" with "Kunci mesin tidak dikenal." | Wrong `machineKey` | Paste the key again; restart the agent |
| "Mesin offline" with "Mesin ini dinonaktifkan…" | The machine is deactivated in the admin web | Activate it in the admin web |
| `check`: `no CH340 board found` | USB cable, or the CH340 driver missing | Plug it in, check Device Manager → Ports, or set `"serialPort": "COM3"` |
| `check` or `vend`: `Access is denied` | Another program has the COM port: the service, or the vendor app | `vending-machine.exe service stop` while testing, and close the vendor app |
| `listen on 127.0.0.1:8080` error when you start it by hand | The service is already running and has the port | Stop the service first, or use the developer page of the running one |
| `x509: certificate` errors | The server uses a self-signed certificate (`install --self-signed`) | Set `"insecureSkipVerify": true` |
| Every sale fails with UNKNOWN_SLOT | The slot is missing from `slotMap` | Add it; `check` lists each slot with its board code |

**The journal** (`data\journal\`) holds one file per purchase that the server hasn't confirmed yet. The agent empties it by itself. Don't delete files there by hand, since each one is a sale or a failure the server still needs to hear about.

---

## How it works

### A purchase, step by step

The page calls the agent; the agent calls the server and the board. The **journal stage** column is what the agent has saved on disk before the step, which is what a restart reads back (BR-14).

| # | Step | Agent → | Journal stage |
| --- | --- | --- | --- |
| 1 | The customer taps **Bayar**: create the transaction with the catalog version on screen | server `POST /api/machine/transactions` → PENDING, or CATALOG_STALE with the fresh catalog | none |
| 2 | **Konfirmasi bayar**: the agent saves the step, then asks for PAID | server `.../pay` → PAID (retried if the answer is lost: a repeat is safe) | PAYING |
| 3 | The agent saves the transaction, **then** sends the vend command | board `AB\r` → echo within 0.5 s → END within 5 s | VENDING |
| 4 | The agent saves the board's answer | | RESULT (DISPENSED or FAILED + reason) |
| 5 | The agent reports the result; the server subtracts 1 or blocks the slot | server `.../result` | file deleted once the server confirms |
| 6 | The page shows the result; the agent reloads the catalog | server `GET /api/machine/catalog` | |

What happens when something breaks:

- **The server goes away after step 3:** the customer still sees "Silakan ambil produk". The RESULT stays in the journal and is resent every 10 s until the server confirms it. A repeated result changes nothing (TC-08).
- **A restart while in stage VENDING:** the coil may have turned, so the purchase is reported FAILED with **AGENT_RESTART** and the coil is never turned again (BR-14, TC-37).
- **No answer to pay at all (step 2):** the coil is not turned. Once the server answers, a payment it took becomes FAILED with **PAY_NO_REPLY**, and one it never got is closed as EXPIRED.
- **New sales and restock visits** wait for the journal first, so the server always applies them on top of every earlier sale.

### Catalog and offline (FR-02, FR-10)

The agent keeps the catalog in memory and the page reads it from there, so the home screen appears at once (NFR-01). It reloads the catalog at boot, after each sale and each visit, after 03:00 every day, and when the heartbeat (every 5 minutes, FR-28) reports a newer version. The page re-reads the agent's copy every 30 s while idle; that is a local call, not server traffic.

The kiosk never sells offline (D1). When the server can't be reached, the page shows "Mesin offline", checks every 5 s, and recovers by itself. Tapping a product also checks the server, so a dead connection shows before the customer pays.

### What the agent stores

Everything is under `data/`, next to `config.json`. There is no database: the server is the only place stock and sales live.

| Folder | What | Kept until |
| --- | --- | --- |
| `data/journal/` | One JSON file per purchase the server hasn't confirmed | The server confirms the result |
| `data/cache/uploads/` | Product images: each image's name changes when the image does, so a copy never goes stale | Kept |
| `data/logs/` | `vending-machine.log`, rotated at 5 MB, 3 old files kept | Rotated |

### The page's API

The page talks only to the agent, at `/api/...` on the same origin. Errors are `{ "code", "message" }`; "server unreachable" and a refused machine key are both `503 { "code": "OFFLINE" }`, with a `reason` and `message` for a refused key.

| Page calls | Agent does | Server endpoint |
| --- | --- | --- |
| `GET /api/health` | Heartbeat, live | `POST /api/machine/heartbeat` |
| `GET /api/catalog` | Answers from its cache, reloads when needed | `GET /api/machine/catalog` |
| `POST /api/transactions` | Forwards; keeps the fresh catalog of a CATALOG_STALE | `POST /api/machine/transactions` |
| `POST /api/transactions/{id}/pay` | Pay → vend → result, with the journal; answers after the coil | `.../pay`, `.../result` |
| `POST /api/transactions/{id}/cancel` | Forwards | `.../cancel` |
| `POST /api/transactions/{id}/report` | Sends a pending result first, then forwards | `.../report` |
| `POST /api/operator/login`, `.../logout`, `GET /api/operator/slots` | Forwards, with the operator's token | `/api/machine/operator/...` |
| `POST /api/restock-sessions` | Sends pending results first, then forwards; reloads the catalog | `POST /api/machine/restock-sessions` |
| `GET /uploads/...` | Product images, from its disk copy or the server | `GET /uploads/...` |
| `GET /api/agent/status`, `POST /api/agent/sim` | The developer page: status, simulator mode | (none) |

The page also runs without the agent on demo data: open `/?mock=1`, or `preview.html` for both screen orientations side by side.

### config.json

| Setting | Default | Meaning |
| --- | --- | --- |
| `serverUrl` | (required) | The server: `https://35.219.98.32`, or `http://localhost:8080` on your Mac |
| `machineKey` | (required) | This machine's key, `vmk_...` |
| `listen` | `127.0.0.1:8080` | Where the kiosk page is served. Keep `127.0.0.1`: only this PC may reach it |
| `board` | `serial` | `serial` for the motor board, `sim` for the simulator |
| `serialPort` | `""` | `""` finds the CH340; or `COM3`, `/dev/cu.usbserial-110` |
| `slotMap` | `1=AA … 6=BC` | Slot number → board code (column A–H, row A–F) |
| `simMode` | `ok` | The simulator's mode at start: `ok`, `jam` or `no_ack` |
| `simTurnMs` | `2500` | How long a simulated good turn takes |
| `insecureSkipVerify` | `false` | Only for a server with a self-signed certificate |
| `dataDir` | `data` | Journal, image cache and logs, relative to `config.json` |
| `logLevel` | `info` | `debug` also logs every page request and poll |
| `heartbeatSeconds` | `300` | FR-28: every 5 minutes |

A setting name with a typo is an error, not a silent default.

### Commands

| Command | What it does |
| --- | --- |
| `vending-machine` or `vending-machine run` | Run the agent. `-config <file>` picks another settings file |
| `vending-machine check` | Check the settings, the server and key, the board's port and the journal, then exit |
| `vending-machine ports` | List the serial ports; the CH340 is marked `*` |
| `vending-machine vend <slot>` | Turn one coil once, without the server. `-port COM5` picks a port, `-sim` uses the simulator |
| `vending-machine service install` | Windows, as administrator: install and start the service |
| `vending-machine service uninstall\|start\|stop\|status` | Windows: manage the service |
| `vending-machine version` | Print the version |

`make run`, `make check`, `make test` and `make windows` are shortcuts for the commands above.
