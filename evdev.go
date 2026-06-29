package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// Linux input_event (amd64 layout: time is two 64-bit words).
type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

const eventSize = int(unsafe.Sizeof(inputEvent{})) // 24 on amd64

// Event types.
const (
	evSyn = 0x00
	evKey = 0x01
	evRel = 0x02
	evAbs = 0x03
	evMsc = 0x04
)

// Relative axes.
const (
	relX      = 0x00
	relY      = 0x01
	relHWheel = 0x06
	relWheel  = 0x08
)

// Key value (EV_KEY).
const (
	keyUp     = 0
	keyDown   = 1
	keyRepeat = 2
)

// ioctl number construction (asm-generic).
const (
	iocNRBits    = 8
	iocTypeBits  = 8
	iocSizeBits  = 14
	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
	iocRead      = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNRShift) | (size << iocSizeShift)
}

const iocWrite = 1

func eviocgname(size uintptr) uintptr    { return ioc(iocRead, 'E', 0x06, size) }
func eviocgbit(ev, size uintptr) uintptr { return ioc(iocRead, 'E', 0x20+ev, size) }

// EVIOCGRAB = _IOW('E', 0x90, int): take exclusive access to the device so the
// kernel stops delivering its events to libinput/sway. Pass 1 to grab, 0 to release.
func eviocgrab() uintptr { return ioc(iocWrite, 'E', 0x90, 4) }

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// device represents an opened evdev node we want to read from.
type device struct {
	path string
	name string
	file *os.File
	kbd  bool
	mice bool
}

// setGrab toggles exclusive access (EVIOCGRAB) on the device.
//
// EVIOCGRAB is special: the kernel reads the ioctl argument *as the value*
// (non-zero => grab, zero => ungrab), NOT as a pointer to an int. Passing a
// pointer makes ungrab impossible (the address is always non-zero), so we pass
// the value directly here instead of going through ioctl().
func (d *device) setGrab(on bool) error {
	var v uintptr
	if on {
		v = 1
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.file.Fd(), eviocgrab(), v)
	if errno != 0 {
		return errno
	}
	return nil
}

func testBit(buf []byte, bit int) bool {
	return buf[bit/8]&(1<<(uint(bit)%8)) != 0
}

func deviceName(f *os.File) string {
	buf := make([]byte, 256)
	if err := ioctl(f.Fd(), eviocgname(uintptr(len(buf))), unsafe.Pointer(&buf[0])); err != nil {
		return "unknown"
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n])
}

// classify reports whether the device looks like a keyboard and/or a mouse.
func classify(f *os.File) (kbd, mouse bool) {
	const keyMax = 0x2ff
	keyBits := make([]byte, keyMax/8+1)
	if err := ioctl(f.Fd(), eviocgbit(evKey, uintptr(len(keyBits))), unsafe.Pointer(&keyBits[0])); err == nil {
		// A real keyboard exposes letter keys; KEY_A=30, KEY_Z=44.
		if testBit(keyBits, keyA) && testBit(keyBits, keyZ) {
			kbd = true
		}
		if testBit(keyBits, btnLeft) {
			mouse = true
		}
	}
	relBits := make([]byte, 2)
	if err := ioctl(f.Fd(), eviocgbit(evRel, uintptr(len(relBits))), unsafe.Pointer(&relBits[0])); err == nil {
		if testBit(relBits, relX) && testBit(relBits, relY) {
			mouse = true
		}
	}
	return
}

var usbIfaceRe = regexp.MustCompile(`^[0-9]+-[0-9.]+:[0-9]+\.[0-9]+$`)

// usbDeviceKey identifies the physical USB device an event node belongs to, so
// sibling HID interfaces of the same device (keyboard + its Consumer/System
// Control nodes, etc.) can be grouped. The key is the sysfs path up to and
// including the USB device directory (e.g. ".../3-1.1"). Returns "" for non-USB
// devices, which then group only with themselves.
func usbDeviceKey(eventPath string) string {
	real, err := filepath.EvalSymlinks(filepath.Join("/sys/class/input", filepath.Base(eventPath)))
	if err != nil {
		return ""
	}
	parts := strings.Split(real, "/")
	for i, p := range parts {
		if i > 0 && usbIfaceRe.MatchString(p) {
			return strings.Join(parts[:i], "/") // ends at the USB device dir
		}
	}
	return ""
}

// discover opens the input devices named in cfg.Devices, plus every sibling HID
// interface of the same physical device — so listing a keyboard automatically
// also captures its Consumer/System Control nodes (where keys like Calculator
// live), without having to name each one.
func discover(cfg *Config) ([]*device, error) {
	paths, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	type cand struct {
		path, name, key string
		f               *os.File
	}
	var cands []cand
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue // not in `input` group or busy — skip silently
		}
		cands = append(cands, cand{p, deviceName(f), usbDeviceKey(p), f})
	}

	// Physical devices that have at least one explicitly-named node.
	wantedKeys := map[string]bool{}
	for _, c := range cands {
		if c.key != "" && cfg.wants(c.name) {
			wantedKeys[c.key] = true
		}
	}

	var devs []*device
	for _, c := range cands {
		keep := cfg.wants(c.name) || (c.key != "" && wantedKeys[c.key])
		if !keep {
			c.f.Close()
			continue
		}
		kbd, mouse := classify(c.f)
		devs = append(devs, &device{path: c.path, name: c.name, file: c.f, kbd: kbd, mice: mouse})
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("none of the configured devices found/readable under /dev/input (check \"devices\" in config.json and that you're in the `input` group)")
	}
	return devs, nil
}
