package main

import (
	"fmt"
	"syscall"
)

const (
	ioctlGetAttr = syscall.TCGETS
	ioctlSetAttr = syscall.TCSETS
	flagCRTSCTS  = 0x80000000
	flagCBAUD    = 0x100f
	ioctlTCSBRK  = 0x5409
)

var portPatterns = []string{"/dev/ttyUSB*", "/dev/ttyACM*"}

var speeds = map[int]uint32{
	110: syscall.B110, 300: syscall.B300, 600: syscall.B600, 1200: syscall.B1200,
	2400: syscall.B2400, 4800: syscall.B4800, 9600: syscall.B9600, 19200: syscall.B19200,
	38400: syscall.B38400, 57600: syscall.B57600, 115200: syscall.B115200,
}

func setSpeed(t *syscall.Termios, baud int) error {
	s, ok := speeds[baud]
	if !ok {
		return fmt.Errorf("unsupported baud rate %d", baud)
	}
	t.Cflag = t.Cflag&^flagCBAUD | s
	t.Ispeed = s
	t.Ospeed = s
	return nil
}

// TCSBRK with a non-zero argument is tcdrain().
func drain(fd int) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlTCSBRK, 1); e != 0 {
		return e
	}
	return nil
}
