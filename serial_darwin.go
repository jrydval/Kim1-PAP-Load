package main

import (
	"syscall"
	"unsafe"
)

const (
	ioctlGetAttr = syscall.TIOCGETA
	ioctlSetAttr = syscall.TIOCSETA
	flagCRTSCTS  = 0x00030000 // CCTS_OFLOW | CRTS_IFLOW
)

var portPatterns = []string{"/dev/cu.usbserial*", "/dev/cu.usbmodem*", "/dev/cu.SLAB_USBtoUART*", "/dev/cu.wchusbserial*"}

// On macOS speed_t is the plain baud rate, so any value works.
func setSpeed(t *syscall.Termios, baud int) error {
	t.Ispeed = uint64(baud)
	t.Ospeed = uint64(baud)
	return nil
}

func drain(fd int) error { return ioctl(fd, syscall.TIOCDRAIN, unsafe.Pointer(nil)) }
