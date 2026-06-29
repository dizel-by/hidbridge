# hidbridge

A tiny DIY **KVM**: a Linux daemon captures your keyboard & mouse and an
**ESP32-S3** replays them to another computer as a **real USB keyboard + mouse**.
The target needs **no software, no drivers, no pairing** — it just sees a USB HID
device. Handy for controlling a machine you can't (or don't want to) install
anything on: a Mac, a BIOS/UEFI screen, a locked box, a KVM-less server, etc.

Transport between the PC and the ESP32 is your choice: **USB cable**, **BLE**, or
**Wi-Fi**. The target only sees the ESP32-S3's native USB HID device.

```text
+-------------------- your Linux PC --------------------+
|                                                       |
|  keyboard --.                                         |
|    mouse --+--> /dev/input/event*                     |
|                    |                                  |
|                    | raw evdev                        |
|                    v                                  |
|             +----------------+                        |
|             |   hidbridge    |                        |
|             |   daemon (Go)  |                        |
|             +--------+-------+                        |
|                      |                                |
|                      | HID reports                    |
|                      v                                |
|               framed bytes                            |
|                  |                                    |
|                  +-- USB-serial cable                 |
|                  +-- BLE GATT                         |
|                  `-- Wi-Fi TCP                        |
|                                                       |
+---------------------------+---------------------------+
                            |
                            |  0xAB | type | payload | xor
                            v
+------------------------- ESP32-S3 --------------------+
|                                                       |
|   UART0 / NimBLE / TCP -> frame parser -> TinyUSB     |
|                                                       |
+---------------------------+---------------------------+
                            |
                            |  native USB-OTG HID
                            v
                   +---------------------+
                   |   target computer   |
                   |   Mac / BIOS / etc. |
                   +---------------------+
```

## No software on the target

The target side runs **nothing** — no agent, no driver, no pairing, no settings.
It just sees a standard USB keyboard + mouse, so it works literally anywhere a USB
keyboard works: a normal OS, a login or lock screen, a bootloader, even the
**BIOS/UEFI setup** screen. That's the whole point — it's a real HID device, not
a software remote-control.

## How it works

**PC daemon (Go, `*.go`).** Reads input devices directly via raw **evdev**
(`/dev/input/event*`; you must be in the `input` group — no root needed). A
hotkey toggles **KVM mode**: while on, the daemon `EVIOCGRAB`s the devices so the
local desktop no longer receives them, keeps the full keyboard/mouse **state**,
and emits compact HID reports to the chosen transport. Toggle off and input
returns to your machine.

**Firmware (ESP-IDF, `firmware/esp32s3_hid_idf/`).** A dumb bridge: it parses the
framed reports from any transport and hands them to **TinyUSB**, which presents a
keyboard + mouse to the target over the ESP32-S3's native USB. See the
[firmware README](firmware/esp32s3_hid_idf/README.md) for build/flash/wiring.

**Wire protocol.** Each frame is `0xAB` (sync) · `type` · `payload` · `xor`
checksum. Types are `1` = keyboard report (8 B), `2` = mouse report (6 B),
`3` = radio control (1 B), and `4` = Wi-Fi credentials. Keyboard reports are full
state snapshots; mouse reports carry signed deltas plus buttons/wheel. The ESP32
does not interpret host input, it only validates frames and passes reports to
TinyUSB.

**Auto-release.** While in KVM mode the daemon watches the bridge link; if it drops
(USB unplugged, BLE/TCP lost) the grab is released immediately so you're never
locked out, and it won't re-grab until you press the toggle again.

**Radios.** BLE and Wi-Fi can be enabled/disabled independently over the cable
(persisted in NVS on the board) — useful to avoid Wi-Fi/BLE coexistence:

```sh
./hidbridge -serial -set-wifi off    # e.g. give BLE the radio to itself
./hidbridge -serial -set-ble off     # or give Wi-Fi the radio to itself
```

| Transport | Flag | Feel | Notes |
|-----------|------|------|-------|
| Cable (USB-serial) | `-serial` | **Like using the PC directly** — no perceptible lag | most reliable; the bridge is plugged into the PC |
| BLE | `-ble` | **Minimal latency**, smooth | wireless; steady ~7.5 ms, low jitter; the PC keeps its own Wi-Fi |
| Wi-Fi (TCP) | `-net` | **Usable, but not super comfortable** | range / whole-house; try `-coalesce=4` to `-coalesce=8` if mouse motion feels jittery |

## Hardware

- An **ESP32-S3** dev board (native USB-OTG required — the S3/S2 have it; the
  classic ESP32 does **not**). Two USB ports on the board is ideal: the **native
  USB** goes to the target, the **UART bridge** goes to the PC (and is used for
  flashing).
- A Linux PC with the input devices you want to forward. Wireless transports also
  need a Bluetooth adapter (BLE) or the board on your Wi-Fi (Wi-Fi).

## Quick start

```sh
# 1. Firmware — see firmware/esp32s3_hid_idf/README.md (Docker, no local IDF):
cd firmware/esp32s3_hid_idf
./idf.sh build
./idf.sh -p /dev/ttyUSB0 flash      # flash via the UART port

# 2. Daemon:
cd ../..
cp config.example.json config.json   # then edit "devices" for your machine
go build -o hidbridge .
./hidbridge -ble            # or -serial / -net
```

Plug the board's **native USB** into the target. Run the daemon, press the
**toggle hotkey** (Calculator key by default) to grab input and forward it; press
again to release.

Each transport flag has a sensible default and an optional override:

```sh
./hidbridge -serial            # default /dev/ttyUSB0
./hidbridge -ble               # device named "hidbridge"
./hidbridge -net               # hidbridge.local:3232
./hidbridge -ble=other-name    # override with =value
```

## Configuration

All machine-specific settings live in **`config.json`** (no recompiling). Copy
the template and edit it:

```sh
cp config.example.json config.json
```

```jsonc
{
  "devices": [                 // exact device names (EVIOCGNAME) to grab/forward
    "Logitech Wireless Receiver Mouse",
    "My Keyboard"
  ],
  "toggleKey": 140,            // evdev keycode to toggle KVM (140 = KEY_CALC / Calculator)
  "invertScroll": true,        // invert vertical wheel (handy for macOS targets)
  "serial": "/dev/ttyUSB0",    // default for bare -serial
  "ble": "hidbridge",          // default for bare -ble
  "net": "hidbridge.local:3232"// default for bare -net
}
```

List **one node per physical device** — sibling HID interfaces of the same USB
device (the *Consumer Control* / *System Control* nodes, where keys like
Calculator and media keys live) are pulled in automatically, so the toggle key is
captured without naming them.

`config.json` is loaded from the working dir or `~/.config/hidbridge/config.json`
(or `-config PATH`); it's gitignored. Anything omitted keeps a built-in default.
To find your device names, run with no transport (it prints what it finds) or
`cat /proc/bus/input/devices`.

**Wi-Fi credentials** are set over the cable and stored in NVS on the board:

```sh
./hidbridge -serial -wifi-ssid "MyNet" -wifi-pass "secret"
```

You can also bake defaults into `firmware/esp32s3_hid_idf/.env`; see the firmware
README.

## Diagnostics

```sh
./hidbridge               # no transport: prints decoded reports (debug)
./hidbridge -v            # also print events while forwarding
./hidbridge -selftest -serial   # send a test mouse wiggle + keypress
./hidbridge -blescan      # list nearby BLE devices
./hidbridge -blebench hidbridge # measure BLE write throughput for 3 seconds
./hidbridge -net -coalesce=6    # batch mouse motion every 6 ms for Wi-Fi
```

## Status

A personal project that does its job on the author's setup (Linux/sway, an
ESP32-S3, a Mac as the target). It's intentionally small and hackable rather than
plug-and-play — expect to tweak `config.go` and the hotkey for your machine.

The target just sees a standard USB HID device, which is why it works everywhere
a USB keyboard does. Note: very old, legacy BIOSes that only speak the HID *boot
protocol* may not pick up the report-protocol descriptor used here; modern
UEFI is fine. (Boot-protocol support can be added to the firmware if you hit
this.)

## License

See [LICENSE](LICENSE).
