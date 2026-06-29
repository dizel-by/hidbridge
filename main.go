package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// kvm owns all devices and the global grab state. While "grabbed" the daemon
// holds EVIOCGRAB on every device, so the local desktop sees no keyboard/mouse —
// events go only here (forwarded to the ESP32). toggleKey (an evdev keycode)
// flips that on/off; releasing hands input back to the local session.
type kvm struct {
	mu        sync.Mutex
	devs      []*device
	grabbed   bool
	pressed   map[uint16]bool // currently-held keycodes, across all devices
	rep       *Reporter
	toggleKey uint16
	verbose   bool
}

func newKVM(devs []*device, rep *Reporter, toggleKey uint16, verbose bool) *kvm {
	return &kvm{devs: devs, pressed: make(map[uint16]bool), rep: rep, toggleKey: toggleKey, verbose: verbose}
}

// applyGrab grabs or releases every device. Must hold k.mu.
func (k *kvm) applyGrab(on bool) {
	if on {
		for i, d := range k.devs {
			if err := d.setGrab(true); err != nil {
				// Roll back any grabs already taken so we never half-grab.
				fmt.Fprintf(os.Stderr, "grab %s: %v (aborting, releasing)\n", d.path, err)
				for _, prev := range k.devs[:i] {
					prev.setGrab(false)
				}
				k.grabbed = false
				return
			}
		}
		k.grabbed = true
		k.rep.Reset() // start clean
		fmt.Println(">>> KVM ON  — input grabbed, sway sees nothing (forwarding to ESP32)")
		return
	}

	for _, d := range k.devs {
		if err := d.setGrab(false); err != nil {
			fmt.Fprintf(os.Stderr, "ungrab %s: %v\n", d.path, err)
		}
	}
	k.grabbed = false
	k.rep.Reset() // release any held keys/buttons on the target
	fmt.Println(">>> KVM OFF — input released back to local session")
}

// handle updates modifier state and returns true if this event was the toggle
// hotkey (and therefore should not be forwarded/printed).
func (k *kvm) handle(ev inputEvent) bool {
	if ev.Type != evKey {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	switch ev.Value {
	case keyDown:
		k.pressed[ev.Code] = true
	case keyUp:
		delete(k.pressed, ev.Code)
	}

	if ev.Code == k.toggleKey && ev.Value == keyDown {
		k.applyGrab(!k.grabbed)
		return true
	}
	// Swallow key-up/repeat of the toggle key too, so it never leaks out.
	if ev.Code == k.toggleKey {
		return true
	}
	return false
}

func (k *kvm) isGrabbed() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.grabbed
}

// forceRelease ungrabs everything immediately (e.g. the bridge was unplugged) so
// the local machine never stays locked out. Grab does NOT come back on its own —
// the user must press the toggle key again.
func (k *kvm) forceRelease(reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.grabbed {
		return
	}
	for _, d := range k.devs {
		d.setGrab(false)
	}
	k.grabbed = false
	k.rep.ClearState() // target is gone — just drop local state, don't send
	fmt.Printf(">>> %s — input released; press %s to re-grab\n", reason, keyName(k.toggleKey))
}

// watchLink polls the bridge link while grabbed and releases the grab the moment
// the bridge drops off this machine (USB node gone / BLE or TCP disconnected), so
// you're never left locked out.
func (k *kvm) watchLink(sink Sink) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if k.isGrabbed() && !sink.Alive() {
			k.forceRelease("bridge disconnected")
		}
	}
}

// readDevice reads input_event records and feeds them to the kvm controller.
// This is where, later, we'll build HID reports and ship them to the ESP32
// (only while grabbed).
func readDevice(k *kvm, d *device) {
	defer d.file.Close()

	role := "input"
	switch {
	case d.kbd && d.mice:
		role = "kbd+mouse"
	case d.kbd:
		role = "keyboard"
	case d.mice:
		role = "mouse"
	}
	fmt.Printf("• %-9s %-40s (%s)\n", role, d.name, d.path)

	buf := make([]byte, eventSize)
	for {
		if _, err := io.ReadFull(d.file, buf); err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF && !errors.Is(err, os.ErrClosed) {
				fmt.Fprintf(os.Stderr, "read %s: %v\n", d.path, err)
			}
			return
		}
		ev := inputEvent{
			Sec:   int64(binary.LittleEndian.Uint64(buf[0:8])),
			Usec:  int64(binary.LittleEndian.Uint64(buf[8:16])),
			Type:  binary.LittleEndian.Uint16(buf[16:18]),
			Code:  binary.LittleEndian.Uint16(buf[18:20]),
			Value: int32(binary.LittleEndian.Uint32(buf[20:24])),
		}
		if k.handle(ev) {
			continue // toggle hotkey, swallow it
		}
		// Only forward to the HID sink while grabbed — that's KVM mode.
		if k.isGrabbed() {
			k.rep.Handle(ev)
			if k.verbose {
				printEvent(d, ev)
			}
		}
	}
}

func printEvent(d *device, ev inputEvent) {
	switch ev.Type {
	case evKey:
		if isButton(ev.Code) {
			fmt.Printf("  [%s] MOUSE %-10s %s\n", d.name, keyName(ev.Code), valueName(ev.Value))
			return
		}
		if ev.Value == keyRepeat {
			return
		}
		usage := hidUsage[ev.Code]
		fmt.Printf("  [%s] KEY   %-10s %-4s code=%-3d  HID=0x%02X\n",
			d.name, keyName(ev.Code), valueName(ev.Value), ev.Code, usage)
	case evRel:
		switch ev.Code {
		case relX:
			fmt.Printf("  [%s] MOVE  x %+d\n", d.name, ev.Value)
		case relY:
			fmt.Printf("  [%s] MOVE  y %+d\n", d.name, ev.Value)
		case relWheel:
			fmt.Printf("  [%s] WHEEL v %+d\n", d.name, ev.Value)
		case relHWheel:
			fmt.Printf("  [%s] WHEEL h %+d\n", d.name, ev.Value)
		}
	}
}

// optFlag is a transport flag with an optional value: the bare form (e.g.
// "-ble") uses def, while "-ble=NAME" overrides it. set reports whether the flag
// was given at all.
type optFlag struct {
	set bool
	val string
	def string
}

func (o *optFlag) String() string   { return o.val }
func (o *optFlag) IsBoolFlag() bool { return true } // allows the bare "-flag" form
func (o *optFlag) Set(s string) error {
	o.set = true
	if s == "true" { // bare flag
		o.val = o.def
	} else {
		o.val = s
	}
	return nil
}

// configPathFromArgs pulls -config out of os.Args before flag.Parse, so the
// config can be loaded in time to supply flag defaults.
func configPathFromArgs() string {
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "-config" || a == "--config":
			if i+1 < len(os.Args) {
				return os.Args[i+1]
			}
		case strings.HasPrefix(a, "-config="):
			return strings.TrimPrefix(a, "-config=")
		case strings.HasPrefix(a, "--config="):
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return ""
}

func onOff(s string) (on, ok bool) {
	switch strings.ToLower(s) {
	case "on", "1", "true", "yes":
		return true, true
	case "off", "0", "false", "no":
		return false, true
	}
	return false, false
}

// setRadios sends a control frame over serial to enable/disable the ESP32 radios
// (the board persists the choice in NVS and reboots). Empty ble/wifi = unchanged.
func setRadios(path, ble, wifi string) {
	var cmd byte
	if ble != "" {
		on, ok := onOff(ble)
		if !ok {
			fmt.Fprintln(os.Stderr, "-set-ble must be on|off")
			os.Exit(1)
		}
		cmd |= ctlBleSet
		if on {
			cmd |= ctlBleOn
		}
	}
	if wifi != "" {
		on, ok := onOff(wifi)
		if !ok {
			fmt.Fprintln(os.Stderr, "-set-wifi must be on|off")
			os.Exit(1)
		}
		cmd |= ctlWifiSet
		if on {
			cmd |= ctlWifiOn
		}
	}

	ss, err := openSerial(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open serial %s: %v\n", path, err)
		os.Exit(1)
	}
	defer ss.Close()
	if err := ss.write(frame(typeControl, []byte{cmd})); err != nil {
		fmt.Fprintf(os.Stderr, "send control: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(100 * time.Millisecond) // let the bytes drain before we close
	fmt.Printf("radio control sent to %s (ble=%q wifi=%q) — the board will reboot\n", path, ble, wifi)
}

// setWifiCreds sends the Wi-Fi SSID/password to the ESP32 over serial as a config
// frame; the board saves them to NVS, enables Wi-Fi, and reboots.
func setWifiCreds(path, ssid, pass string) {
	if len(ssid) > 32 || len(pass) > 63 {
		fmt.Fprintln(os.Stderr, "ssid must be <=32 and password <=63 bytes")
		os.Exit(1)
	}
	data := []byte{byte(len(ssid))}
	data = append(data, ssid...)
	data = append(data, byte(len(pass)))
	data = append(data, pass...)
	payload := append([]byte{byte(len(data))}, data...) // length-prefixed

	ss, err := openSerial(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open serial %s: %v\n", path, err)
		os.Exit(1)
	}
	defer ss.Close()
	if err := ss.write(frame(typeConfig, payload)); err != nil {
		fmt.Fprintf(os.Stderr, "send config: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("wifi credentials sent to %s (ssid=%q) — the board will reboot and join\n", path, ssid)
}

func main() {
	// Load machine-specific settings first so flag defaults come from the config.
	cfg, cfgPath, err := loadConfig(configPathFromArgs())
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	serial := &optFlag{def: cfg.Serial}
	netAddr := &optFlag{def: cfg.Net}
	bleName := &optFlag{def: cfg.BLE}
	flag.String("config", "", "path to config.json (machine-specific settings)")
	flag.Var(serial, "serial", "serial transport; bare -serial uses "+serial.def+", or -serial=/dev/ttyUSB0")
	flag.Var(netAddr, "net", "Wi-Fi/TCP transport; bare -net uses "+netAddr.def+", or -net=host:port")
	flag.Var(bleName, "ble", "BLE transport; bare -ble uses \""+bleName.def+"\", or -ble=NAME")
	bleScan := flag.Bool("blescan", false, "scan and list nearby BLE devices, then exit")
	bleBench := flag.String("blebench", "", "connect to BLE device and measure write throughput, then exit")
	setBle := flag.String("set-ble", "", "turn the ESP32 BLE radio on|off over serial (persists + reboots board), then exit")
	setWifi := flag.String("set-wifi", "", "turn the ESP32 Wi-Fi radio on|off over serial (persists + reboots board), then exit")
	wifiSSID := flag.String("wifi-ssid", "", "save Wi-Fi SSID to the ESP32 over serial (persists + reboots), then exit; pair with -wifi-pass")
	wifiPass := flag.String("wifi-pass", "", "Wi-Fi password to save alongside -wifi-ssid")
	verbose := flag.Bool("v", false, "also print decoded events while forwarding")
	selftest := flag.Bool("selftest", false, "send a test mouse+key sequence to -serial and exit")
	invertScroll := flag.Bool("invert-scroll", cfg.InvertScroll, "invert vertical scroll wheel direction")
	coalesceMs := flag.Int("coalesce", 0, "coalesce mouse motion into one report every N ms (0=off; try 4-8 for Wi-Fi)")
	flag.Parse()

	if *bleScan {
		scanBLE()
		return
	}
	if *bleBench != "" {
		benchBLE(*bleBench)
		return
	}
	serialPath := func() string {
		if serial.set {
			return serial.val
		}
		return cfg.Serial
	}
	if *setBle != "" || *setWifi != "" {
		setRadios(serialPath(), *setBle, *setWifi)
		return
	}
	if *wifiSSID != "" {
		setWifiCreds(serialPath(), *wifiSSID, *wifiPass)
		return
	}

	devs, err := discover(&cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	nTransports := 0
	for _, set := range []bool{serial.set, netAddr.set, bleName.set} {
		if set {
			nTransports++
		}
	}
	if nTransports > 1 {
		fmt.Fprintln(os.Stderr, "error: choose only one of -serial / -net / -ble")
		os.Exit(1)
	}

	var sink Sink = debugSink{}
	switch {
	case serial.set:
		ss, err := openSerial(serial.val)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open serial %s: %v\n", serial.val, err)
			os.Exit(1)
		}
		sink = ss
		fmt.Printf("transport: serial %s @ 921600 raw\n", serial.val)
	case netAddr.set:
		sink = dialNet(netAddr.val)
		fmt.Printf("transport: tcp %s\n", netAddr.val)
	case bleName.set:
		bs, err := dialBLE(bleName.val)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ble: %v\n", err)
			os.Exit(1)
		}
		sink = bs
		fmt.Printf("transport: ble %q\n", bleName.val)
	default:
		fmt.Println("transport: none (debug mode — reports are printed, not sent)")
	}
	defer sink.Close()

	if *selftest {
		fmt.Println("selftest: wiggling mouse, then tapping 'a' …")
		for i := range 60 {
			dx := int16(12)
			if i%2 == 1 {
				dx = -12
			}
			sink.Mouse(0, dx, 0, 0)
			time.Sleep(15 * time.Millisecond)
		}
		sink.Keyboard([8]byte{0, 0, 0x04}) // 'a' down
		time.Sleep(40 * time.Millisecond)
		sink.Keyboard([8]byte{}) // release
		fmt.Println("selftest done")
		return
	}

	k := newKVM(devs, NewReporter(sink, *invertScroll, time.Duration(*coalesceMs)*time.Millisecond), cfg.ToggleKey, *verbose)
	if cfgPath != "" {
		fmt.Printf("config: %s\n", cfgPath)
	} else {
		fmt.Println("config: none (built-in defaults; set \"devices\" in config.json — see config.example.json)")
	}
	fmt.Printf("hidbridge KVM daemon — press %s (keycode %d) to toggle grab, Ctrl-C to quit\n",
		keyName(cfg.ToggleKey), cfg.ToggleKey)
	fmt.Println("starting in LOCAL mode (not grabbed)")
	fmt.Println("devices:")

	for _, d := range devs {
		go readDevice(k, d)
	}
	go k.watchLink(sink) // release the grab if the bridge link drops
	fmt.Println("---")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	fmt.Println("\nshutting down…")
	// Release grab before exit so we never leave the session without input.
	// (The kernel also drops the grab when the fds close on exit, but be explicit.)
	k.mu.Lock()
	if k.grabbed {
		k.applyGrab(false)
	}
	k.mu.Unlock()
	// Reader goroutines are blocked in a blocking read on a character device;
	// closing won't reliably interrupt them, so just exit and let the OS reap.
	os.Exit(0)
}
