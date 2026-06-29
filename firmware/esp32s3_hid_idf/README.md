# hidbridge ESP32-S3 HID bridge (ESP-IDF)

Dumb USB-HID bridge: receives framed HID reports from the PC daemon and forwards
them to the target computer as a USB keyboard + mouse. One firmware, three
transports, all active at once: **cable (UART)**, **BLE**, and **Wi-Fi (TCP)**.

```
PC --USB(UART) | BLE | Wi-Fi--> ESP32-S3 --USB-OTG(HID)--> TARGET PC
```

The daemon picks the transport with its flags:

```sh
./hidbridge -serial /dev/ttyUSB0       # cable
./hidbridge -ble hidbridge                # bluetooth LE
./hidbridge -net hidbridge.local:3232     # wi-fi
```

Cable and BLE are always on. **Wi-Fi starts only if an SSID is configured.**

The easiest way is to set credentials at runtime over the cable — they're stored
in NVS, so you can flash a generic firmware and configure it without rebuilding:

```sh
./hidbridge -serial -wifi-ssid "MyNet" -wifi-pass "secret"   # saves to NVS, reboots, joins
```

Alternatively, bake a build-time default into a `.env` file (gitignored):

```sh
cp .env.example .env     # then edit WIFI_SSID / WIFI_PASSWORD
```

`main/CMakeLists.txt` injects `.env` at build time; **NVS (set over serial)
overrides it**. If neither is set (SSID `changeme`/empty) Wi-Fi stays off and it's
cable+BLE only. (The Kconfig `CONFIG_HIDBRIDGE_WIFI_SSID` is the lowest-priority
fallback.)

Note: running BLE and Wi-Fi at the same time shares the one 2.4 GHz radio
(coexistence), which can add a little latency to the active link. For lowest
latency, BLE alone gives a steady ~7.5 ms; Wi-Fi is handy for range/whole-house
but jitters through the router.

## Build & flash (ESP-IDF >= 5.0)

```sh
cd firmware/esp32s3_hid_idf
idf.py set-target esp32s3
idf.py menuconfig        # optional: "hidbridge" -> BLE device name
idf.py build
idf.py -p /dev/ttyUSB0 flash
```

No local IDF install needed — use the Docker wrapper: `./idf.sh build`.
Component `espressif/esp_tinyusb` is pulled automatically; NimBLE ships with IDF.

## Wiring

- **Native USB-OTG port** (GPIO19/20) → the **target** computer (the HID device).
- **UART port** (onboard USB-UART bridge, UART0 / GPIO43,44) → the **host PC**
  (used for cable transport and for flashing).

On most ESP32-S3 dev boards these are two separate USB connectors.

## BLE

The firmware advertises as the configured name (default `hidbridge`) and exposes a
Nordic-UART-style service; the daemon writes report frames to its RX
characteristic (Write Without Response, no pairing required). Connect with
`./hidbridge -ble hidbridge`.

**Low latency:** BlueZ defaults to a lazy 30-50 ms LE connection interval, which
would make the mouse feel laggy. ~1 s after connecting the ESP32 requests a fast
**7.5 ms** interval (an immediate request at connect time is ignored by BlueZ);
the Linux kernel accepts it at stock settings — no debugfs/`conn_min_interval`
tweak needed. Verify with `sudo btmon | grep -i 'connection interval'` (expect
`7.50 msec` ~1 s after connect).

## Enabling / disabling the radios

BLE and Wi-Fi can be turned on/off independently at runtime over the cable — handy
to drop coexistence (e.g. disable BLE so Wi-Fi has the 2.4 GHz radio to itself, or
disable Wi-Fi for pure BLE). The choice is stored in NVS and survives reboots; the
board reboots to apply it.

```sh
./hidbridge -serial -set-ble off            # BLE off
./hidbridge -serial -set-wifi off           # Wi-Fi off
./hidbridge -serial -set-ble on -set-wifi on
```

Sent as a control frame (`type 3`) on the same serial link; the cable transport and
this control channel are always available. Default (blank NVS) is both on (Wi-Fi
still also needs an SSID).

## Console / logs

Disabled (`CONFIG_ESP_CONSOLE_UART_NONE=y`) because UART0 carries binary data.

## Device names seen by the target

From the USB string descriptors in `main.c` (`hid_string_descriptor`):

- Manufacturer: `hidbridge`  ·  Product: `hidbridge KVM keyboard`  ·  Serial: `0001`

`VID:PID` defaults to Espressif's (`303A:xxxx`). To masquerade as a specific
device, set a custom `tusb_desc_device_t` via `.device_descriptor`.

## Partitions

NimBLE + TinyUSB use a custom `partitions.csv` (1.7 MB app). The image is ~0.5 MB,
so it fits comfortably (works on 2 MB flash and up).
