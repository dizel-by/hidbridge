package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"tinygo.org/x/bluetooth"
)

// Nordic-UART-style UUIDs (must match the ESP32 firmware). We only write to the
// RX characteristic (central -> peripheral), with Write Without Response for low
// latency.
const (
	nusServiceUUID = "6e400001-b5a3-f393-e0a9-e50e24dcca9e"
	nusRxUUID      = "6e400002-b5a3-f393-e0a9-e50e24dcca9e"
)

// bleSink sends framed HID reports to the ESP32 over BLE GATT. BLE's scheduled
// ~7.5 ms connection interval gives low, steady latency — much less jittery than
// 2.4 GHz Wi-Fi through a router — while the laptop keeps its normal Wi-Fi. It
// reconnects in the background if the link drops.
type bleSink struct {
	name            string
	svcUUID, rxUUID bluetooth.UUID

	mu        sync.Mutex
	dev       bluetooth.Device
	char      bluetooth.DeviceCharacteristic
	connected bool
	dialing   bool
}

// benchBLE measures how many report writes/sec the BLE path sustains. Diagnostic.
func benchBLE(name string) {
	s, err := dialBLE(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ble: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ble: benchmarking writes for 3s …")
	start := time.Now()
	n, errs := 0, 0
	var maxGap time.Duration
	last := start
	for time.Since(start) < 3*time.Second {
		dx := int16(1)
		if n%2 == 0 {
			dx = -1
		}
		if err := s.Mouse(0, dx, 0, 0); err != nil {
			errs++
		}
		now := time.Now()
		if g := now.Sub(last); g > maxGap {
			maxGap = g
		}
		last = now
		n++
	}
	d := time.Since(start)
	fmt.Printf("ble: %d writes in %v = %.0f writes/s, %d errors, worst gap %v\n",
		n, d.Round(time.Millisecond), float64(n)/d.Seconds(), errs, maxGap.Round(time.Microsecond))
}

// scanBLE lists every advertising BLE device for 10s. Diagnostic for -ble.
func scanBLE() {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		fmt.Fprintf(os.Stderr, "enable BLE adapter: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("ble: scanning 10s (Ctrl-C to stop early)…")
	seen := map[string]bool{}
	go func() {
		time.Sleep(10 * time.Second)
		adapter.StopScan()
	}()
	adapter.Scan(func(a *bluetooth.Adapter, r bluetooth.ScanResult) {
		key := r.Address.String()
		if !seen[key] {
			seen[key] = true
			fmt.Printf("  %s  RSSI=%-4d  name=%q\n", key, r.RSSI, r.LocalName())
		}
	})
	fmt.Println("ble: scan done")
}

func dialBLE(name string) (*bleSink, error) {
	if err := bluetooth.DefaultAdapter.Enable(); err != nil {
		return nil, fmt.Errorf("enable BLE adapter: %w", err)
	}
	svcUUID, err := bluetooth.ParseUUID(nusServiceUUID)
	if err != nil {
		return nil, err
	}
	rxUUID, err := bluetooth.ParseUUID(nusRxUUID)
	if err != nil {
		return nil, err
	}
	s := &bleSink{name: name, svcUUID: svcUUID, rxUUID: rxUUID}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

// connect scans for the device by name, connects, and opens its RX characteristic.
func (s *bleSink) connect() error {
	adapter := bluetooth.DefaultAdapter
	fmt.Printf("ble: scanning for %q …\n", s.name)
	resCh := make(chan bluetooth.ScanResult, 1)
	go adapter.Scan(func(a *bluetooth.Adapter, r bluetooth.ScanResult) {
		if r.LocalName() == s.name {
			a.StopScan()
			select {
			case resCh <- r:
			default:
			}
		}
	})

	var res bluetooth.ScanResult
	select {
	case res = <-resCh:
	case <-time.After(20 * time.Second):
		adapter.StopScan()
		return fmt.Errorf("ble: device %q not found within 20s", s.name)
	}

	dev, err := adapter.Connect(res.Address, bluetooth.ConnectionParams{})
	if err != nil {
		return fmt.Errorf("ble: connect: %w", err)
	}
	svcs, err := dev.DiscoverServices([]bluetooth.UUID{s.svcUUID})
	if err != nil || len(svcs) == 0 {
		return fmt.Errorf("ble: service not found: %w", err)
	}
	chars, err := svcs[0].DiscoverCharacteristics([]bluetooth.UUID{s.rxUUID})
	if err != nil || len(chars) == 0 {
		return fmt.Errorf("ble: characteristic not found: %w", err)
	}

	s.mu.Lock()
	s.dev, s.char, s.connected = dev, chars[0], true
	s.mu.Unlock()
	fmt.Println("ble: connected")
	return nil
}

// reconnect re-runs connect() in the background, one attempt at a time.
func (s *bleSink) reconnect() {
	s.mu.Lock()
	if s.dialing || s.connected {
		s.mu.Unlock()
		return
	}
	s.dialing = true
	s.mu.Unlock()
	go func() {
		err := s.connect()
		s.mu.Lock()
		s.dialing = false
		s.mu.Unlock()
		if err != nil {
			logErr("ble: reconnect: %v", err)
		}
	}()
}

func (s *bleSink) write(buf []byte) error {
	s.mu.Lock()
	char, ok := s.char, s.connected
	s.mu.Unlock()
	if !ok {
		s.reconnect()
		return errNotConnected
	}
	if _, err := char.WriteWithoutResponse(buf); err != nil {
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
		s.reconnect()
		return err
	}
	return nil
}

func (s *bleSink) Keyboard(rep [8]byte) error { return s.write(keyboardFrame(rep)) }

func (s *bleSink) Mouse(buttons byte, dx, dy int16, wheel int8) error {
	return s.write(mouseFrame(buttons, dx, dy, wheel))
}

// Alive confirms with BlueZ that the device is still connected; if it dropped, it
// flags the sink and kicks a background reconnect.
func (s *bleSink) Alive() bool {
	s.mu.Lock()
	dev, ok := s.dev, s.connected
	s.mu.Unlock()
	if !ok {
		return false
	}
	if connected, err := dev.Connected(); err == nil && !connected {
		s.mu.Lock()
		s.connected = false
		s.mu.Unlock()
		s.reconnect()
		return false
	}
	return true
}

func (s *bleSink) Close() error { return nil }
