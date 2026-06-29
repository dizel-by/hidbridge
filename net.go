package main

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

var errNotConnected = errors.New("net: not connected")

// netSink sends framed HID reports to the ESP32 over TCP. The connection is
// established lazily and re-established on failure, so the daemon survives the
// ESP32 rebooting or Wi-Fi blips without restarting. TCP_NODELAY keeps latency
// low (no Nagle batching of tiny reports).
type netSink struct {
	addr string

	mu       sync.Mutex
	conn     net.Conn
	lastDial time.Time
}

func dialNet(addr string) *netSink {
	s := &netSink{addr: addr}
	s.mu.Lock()
	s.ensure() // best-effort connect up front
	s.mu.Unlock()
	go s.maintain() // keep the connection up so Alive() reflects reality
	return s
}

// maintain re-dials in the background while disconnected, so the link is ready
// (and Alive() accurate) without depending on outgoing writes.
func (s *netSink) maintain() {
	for {
		s.mu.Lock()
		s.ensure()
		s.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
}

// readUntilClosed blocks reading c (the ESP32 sends nothing back) so a dropped
// TCP link — ESP reboot, Wi-Fi loss — is noticed promptly, not only on a write.
func (s *netSink) readUntilClosed(c net.Conn) {
	var buf [64]byte
	for {
		if _, err := c.Read(buf[:]); err != nil {
			s.mu.Lock()
			if s.conn == c {
				s.conn = nil
			}
			s.mu.Unlock()
			c.Close()
			return
		}
	}
}

func (s *netSink) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil
}

// ensure returns a live connection, dialing if needed. Must hold s.mu.
func (s *netSink) ensure() net.Conn {
	if s.conn != nil {
		return s.conn
	}
	// Rate-limit reconnect attempts so a down ESP32 doesn't spin the CPU.
	if time.Since(s.lastDial) < 500*time.Millisecond {
		return nil
	}
	s.lastDial = time.Now()
	c, err := net.DialTimeout("tcp", s.addr, 2*time.Second)
	if err != nil {
		logErr("net: connect %s: %v", s.addr, err)
		return nil
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	s.conn = c
	go s.readUntilClosed(c) // detect drops promptly
	fmt.Printf("net: connected to %s\n", s.addr)
	return c
}

func (s *netSink) write(buf []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.ensure()
	if c == nil {
		return errNotConnected
	}
	if _, err := c.Write(buf); err != nil {
		// Drop the dead connection; next write triggers a redial.
		c.Close()
		s.conn = nil
		return err
	}
	return nil
}

func (s *netSink) Keyboard(rep [8]byte) error { return s.write(keyboardFrame(rep)) }

func (s *netSink) Mouse(buttons byte, dx, dy int16, wheel int8) error {
	return s.write(mouseFrame(buttons, dx, dy, wheel))
}

func (s *netSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}
