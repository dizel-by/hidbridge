package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Kernel struct termios (asm-generic/termbits.h): NCCS = 19, no speed fields —
// the baud lives in the CBAUD bits of c_cflag.
type termios struct {
	Iflag uint32
	Oflag uint32
	Cflag uint32
	Lflag uint32
	Line  uint8
	Cc    [19]uint8
}

const (
	tcgets = 0x5401
	tcsets = 0x5402

	// c_iflag
	ignbrk = 0x0001
	brkint = 0x0002
	parmrk = 0x0008
	istrip = 0x0020
	inlcr  = 0x0040
	igncr  = 0x0080
	icrnl  = 0x0100
	ixon   = 0x0400
	// c_oflag
	opost = 0x0001
	// c_lflag
	isig   = 0x0001
	icanon = 0x0002
	echoFl = 0x0008
	echonl = 0x0040
	iexten = 0x8000
	// c_cflag
	csize   = 0x0030
	cs8     = 0x0030
	cstopb  = 0x0040
	cread   = 0x0080
	parenb  = 0x0100
	clocal  = 0x0800
	cbaud   = 0x100f
	b921600 = 0x1007

	vtime = 5
	vmin  = 6
)

// openSerial creates a serial sink and opens the port once to validate the path.
func openSerial(path string) (*serialSink, error) {
	s := &serialSink{path: path}
	if err := s.ensure(); err != nil {
		return nil, err
	}
	return s, nil
}

// configurePort opens the device and puts it into raw mode at 921600 baud (the
// baud only matters for the USB-UART bridge; a native CDC port ignores it).
func configurePort(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}

	var t termios
	if err := ioctlTermios(f.Fd(), tcgets, &t); err != nil {
		f.Close()
		return nil, fmt.Errorf("tcgets: %w", err)
	}

	t.Iflag &^= ignbrk | brkint | parmrk | istrip | inlcr | igncr | icrnl | ixon
	t.Oflag &^= opost
	t.Lflag &^= echoFl | echonl | icanon | isig | iexten
	t.Cflag &^= csize | parenb
	t.Cflag |= cs8 | cread | clocal
	t.Cflag = (t.Cflag &^ cbaud) | b921600
	t.Cc[vmin] = 1
	t.Cc[vtime] = 0

	if err := ioctlTermios(f.Fd(), tcsets, &t); err != nil {
		f.Close()
		return nil, fmt.Errorf("tcsets: %w", err)
	}
	return f, nil
}

func ioctlTermios(fd uintptr, req uintptr, t *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}
