package main

import (
	"fmt"
	"os"
	"sync"
)

// Wire framing (PC -> ESP32):
//
//	0xAB  sync
//	type  1 = keyboard, 2 = mouse
//	...   payload (fixed length per type)
//	csum  XOR of type and every payload byte
//
// Keyboard payload: 8 bytes (boot keyboard report: mods, 0, k0..k5).
// Mouse payload:    6 bytes (buttons, dxLo, dxHi, dyLo, dyHi, wheel) — int16 LE.
const (
	frameSync    = 0xAB
	typeKeyboard = 0x01
	typeMouse    = 0x02
	typeControl  = 0x03 // 1-byte payload: enable/disable the ESP32 radios
	typeConfig   = 0x04 // length-prefixed payload: Wi-Fi credentials
)

// Control command bits (must match the firmware).
const (
	ctlBleOn   = 0x01
	ctlBleSet  = 0x02
	ctlWifiOn  = 0x04
	ctlWifiSet = 0x08
)

// Sink consumes HID reports. serialSink frames them onto a serial port, netSink
// onto a TCP connection, debugSink just prints them.
type Sink interface {
	Keyboard(report [8]byte) error
	Mouse(buttons byte, dx, dy int16, wheel int8) error
	// Alive reports whether the bridge is still connected to this machine
	// (USB node present / BLE or TCP link up). Polled to auto-release the grab.
	Alive() bool
	Close() error
}

func logErr(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// frame builds a wire frame: sync, type, payload, checksum.
func frame(typ byte, payload []byte) []byte {
	buf := make([]byte, 0, len(payload)+3)
	buf = append(buf, frameSync, typ)
	csum := typ
	for _, b := range payload {
		csum ^= b
	}
	buf = append(buf, payload...)
	buf = append(buf, csum)
	return buf
}

func keyboardFrame(rep [8]byte) []byte { return frame(typeKeyboard, rep[:]) }

func mouseFrame(buttons byte, dx, dy int16, wheel int8) []byte {
	return frame(typeMouse, []byte{
		buttons,
		byte(dx), byte(uint16(dx) >> 8),
		byte(dy), byte(uint16(dy) >> 8),
		byte(wheel),
	})
}

// debugSink prints reports instead of sending them. Used when no transport is
// set, so the whole pipeline can be exercised without hardware.
type debugSink struct{}

func (debugSink) Keyboard(rep [8]byte) error {
	fmt.Printf("  KBD   mod=%02X keys=% X\n", rep[0], rep[2:])
	return nil
}

func (debugSink) Mouse(buttons byte, dx, dy int16, wheel int8) error {
	fmt.Printf("  MOUSE btn=%02X dx=%-5d dy=%-5d wheel=%d\n", buttons, dx, dy, wheel)
	return nil
}

func (debugSink) Alive() bool  { return true }
func (debugSink) Close() error { return nil }

// serialSink frames reports and writes them to the serial device. It reopens the
// port on demand, so unplugging and replugging the bridge recovers without
// restarting the daemon (a write that fails drops the fd; the next one reopens).
type serialSink struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

func (s *serialSink) ensure() error {
	if s.f != nil {
		return nil
	}
	f, err := configurePort(s.path)
	if err != nil {
		return err
	}
	s.f = f
	return nil
}

func (s *serialSink) write(buf []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensure(); err != nil {
		return err
	}
	if _, err := s.f.Write(buf); err != nil {
		s.f.Close()
		s.f = nil // reopen on the next write (handles unplug/replug)
		return err
	}
	return nil
}

// Alive reports whether the serial device node still exists — when the bridge is
// unplugged from USB, its /dev/ttyUSB* node disappears.
func (s *serialSink) Alive() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

func (s *serialSink) Keyboard(rep [8]byte) error { return s.write(keyboardFrame(rep)) }

func (s *serialSink) Mouse(buttons byte, dx, dy int16, wheel int8) error {
	return s.write(mouseFrame(buttons, dx, dy, wheel))
}

func (s *serialSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		err := s.f.Close()
		s.f = nil
		return err
	}
	return nil
}
