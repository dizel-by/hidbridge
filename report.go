package main

import (
	"slices"
	"sync"
	"time"
)

// Reporter turns the stream of evdev events into HID reports and pushes them to
// a Sink. It keeps the full keyboard/mouse state on the PC side so the ESP32 can
// stay a dumb bridge: it just forwards whatever report bytes it receives.
//
// Events from several devices arrive on separate goroutines, so all state is
// guarded by mu. We batch on EV_SYN (SYN_REPORT), matching evdev's frame model:
// e.g. REL_X + REL_Y + SYN becomes a single mouse report.
type Reporter struct {
	mu          sync.Mutex
	sink        Sink
	invertWheel bool
	coalesce    time.Duration // >0: mouse motion is flushed by a ticker, not per-event

	// keyboard state
	mods     byte
	keys     []uint16 // active HID usages (non-modifier), in press order
	kbdDirty bool

	// mouse state
	buttons    byte
	dx, dy     int32
	wheel      int32
	mouseDirty bool // any motion pending
	mouseBtn   bool // a button change is pending (must flush immediately)
}

func NewReporter(sink Sink, invertWheel bool, coalesce time.Duration) *Reporter {
	r := &Reporter{
		sink:        sink,
		invertWheel: invertWheel,
		coalesce:    coalesce,
		keys:        make([]uint16, 0, 6),
	}
	if coalesce > 0 {
		go r.coalesceLoop()
	}
	return r
}

// coalesceLoop flushes accumulated mouse motion at a steady cadence. Summing
// relative deltas is lossless, so this cuts the Wi-Fi packet rate without losing
// any movement — it just smooths bursty delivery into even steps.
func (r *Reporter) coalesceLoop() {
	t := time.NewTicker(r.coalesce)
	defer t.Stop()
	for range t.C {
		r.mu.Lock()
		if r.dx != 0 || r.dy != 0 || r.wheel != 0 {
			r.flushMouse()
		}
		r.mu.Unlock()
	}
}

func clamp16(v int32) int16 {
	switch {
	case v > 32767:
		return 32767
	case v < -32768:
		return -32768
	}
	return int16(v)
}

func clamp8(v int32) int8 {
	switch {
	case v > 127:
		return 127
	case v < -128:
		return -128
	}
	return int8(v)
}

func (r *Reporter) addKey(usage uint16) {
	if slices.Contains(r.keys, usage) {
		return
	}
	r.keys = append(r.keys, usage)
}

func (r *Reporter) removeKey(usage uint16) {
	for i, k := range r.keys {
		if k == usage {
			r.keys = append(r.keys[:i], r.keys[i+1:]...)
			return
		}
	}
}

// keyboardReport builds the 8-byte boot keyboard report from current state.
func (r *Reporter) keyboardReport() [8]byte {
	var rep [8]byte
	rep[0] = r.mods
	if len(r.keys) > 6 {
		// Too many keys held: phantom/rollover -> 0x01 in all six slots.
		for i := 2; i < 8; i++ {
			rep[i] = 0x01
		}
		return rep
	}
	for i, k := range r.keys {
		rep[2+i] = byte(k)
	}
	return rep
}

// Handle feeds one evdev event into the state machine.
func (r *Reporter) Handle(ev inputEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch ev.Type {
	case evKey:
		if bit, ok := mouseButtonBit[ev.Code]; ok {
			switch ev.Value {
			case keyDown:
				r.buttons |= bit
			case keyUp:
				r.buttons &^= bit
			}
			r.mouseDirty = true
			r.mouseBtn = true
			return
		}
		if ev.Value == keyRepeat {
			return // HID holds keys implicitly; ignore auto-repeat
		}
		if bit, ok := modBit[ev.Code]; ok {
			if ev.Value == keyDown {
				r.mods |= bit
			} else {
				r.mods &^= bit
			}
			r.kbdDirty = true
			return
		}
		if usage, ok := hidUsage[ev.Code]; ok {
			if ev.Value == keyDown {
				r.addKey(usage)
			} else {
				r.removeKey(usage)
			}
			r.kbdDirty = true
		}

	case evRel:
		switch ev.Code {
		case relX:
			r.dx += ev.Value
		case relY:
			r.dy += ev.Value
		case relWheel:
			r.wheel += ev.Value
		}
		r.mouseDirty = true

	case evSyn:
		if r.kbdDirty {
			r.flushKeyboard()
		}
		switch {
		case r.coalesce == 0:
			// No coalescing: flush motion + buttons per evdev frame.
			if r.mouseDirty {
				r.flushMouse()
			}
		case r.mouseBtn:
			// Coalescing: clicks are latency-sensitive, flush them now;
			// pure motion is left to the ticker.
			r.flushMouse()
		}
	}
}

// flushKeyboard sends the current keyboard report. Must hold r.mu.
func (r *Reporter) flushKeyboard() {
	if err := r.sink.Keyboard(r.keyboardReport()); err != nil {
		logErr("keyboard report: %v", err)
	}
	r.kbdDirty = false
}

// flushMouse sends the current buttons + accumulated motion, then resets motion.
// Must hold r.mu.
func (r *Reporter) flushMouse() {
	w := r.wheel
	if r.invertWheel {
		w = -w
	}
	dx, dy, wheel := clamp16(r.dx), clamp16(r.dy), clamp8(w)
	if err := r.sink.Mouse(r.buttons, dx, dy, wheel); err != nil {
		logErr("mouse report: %v", err)
	}
	r.dx, r.dy, r.wheel = 0, 0, 0
	r.mouseDirty = false
	r.mouseBtn = false
}

// ClearState drops all held keys/buttons locally without touching the sink. Used
// on a forced release (the target is gone, so there's nothing to send to).
func (r *Reporter) ClearState() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mods = 0
	r.keys = r.keys[:0]
	r.buttons = 0
	r.dx, r.dy, r.wheel = 0, 0, 0
	r.kbdDirty, r.mouseDirty, r.mouseBtn = false, false, false
}

// Reset clears all state and sends neutral reports, so the target never ends up
// with stuck keys/buttons when KVM is toggled off (or freshly on).
func (r *Reporter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mods = 0
	r.keys = r.keys[:0]
	r.buttons = 0
	r.dx, r.dy, r.wheel = 0, 0, 0
	r.kbdDirty, r.mouseDirty, r.mouseBtn = false, false, false
	if err := r.sink.Keyboard([8]byte{}); err != nil {
		logErr("keyboard reset: %v", err)
	}
	if err := r.sink.Mouse(0, 0, 0, 0); err != nil {
		logErr("mouse reset: %v", err)
	}
}
