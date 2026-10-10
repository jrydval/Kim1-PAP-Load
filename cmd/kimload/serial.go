//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Port is a raw 8N1 serial port. Reads time out after ~100ms with no data
// (VMIN=0, VTIME=1), so a reader goroutine can notice Close.
type Port struct {
	fd     int
	closed atomic.Bool
	saved  syscall.Termios
	// reading is held during read(2). Close waits for it: once the saved
	// (blocking) settings are back, a read in progress would never return,
	// and on macOS close(2) waits for it, hanging the program on exit.
	reading sync.Mutex
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// OpenPort opens path at the given baud rate, 8 data bits, no parity, one
// stop bit, no flow control. It takes an exclusive lock, so nothing else
// (a terminal program, say) can read the board's replies at the same time.
func OpenPort(path string, baud int) (*Port, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("%s is in use by another program (a serial monitor or terminal?) - close it first", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	p := &Port{fd: fd}
	if err := ioctl(fd, syscall.TIOCEXCL, nil); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := ioctl(fd, ioctlGetAttr, unsafe.Pointer(&p.saved)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("%s is not a serial port: %w", path, err)
	}
	t := p.saved
	makeRaw(&t)
	t.Cc[syscall.VMIN] = 0
	t.Cc[syscall.VTIME] = 1
	if err := setSpeed(&t, baud); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	if err := ioctl(fd, ioctlSetAttr, unsafe.Pointer(&t)); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("configure %s: %w", path, err)
	}
	return p, nil
}

func makeRaw(t *syscall.Termios) {
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB | syscall.CSTOPB | flagCRTSCTS
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL
}

// Read returns 0, nil when nothing arrived within ~100ms.
func (p *Port) Read(b []byte) (int, error) {
	p.reading.Lock()
	defer p.reading.Unlock()
	if p.closed.Load() {
		return 0, errors.New("port closed")
	}
	n, err := syscall.Read(p.fd, b)
	if err == syscall.EINTR || err == syscall.EAGAIN {
		return 0, nil
	}
	if n < 0 {
		n = 0
	}
	return n, err
}

func (p *Port) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		n, err := syscall.Write(p.fd, b[total:])
		if err == syscall.EINTR || err == syscall.EAGAIN {
			continue
		}
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Drain waits until everything written has left the UART.
func (p *Port) Drain() error { return drain(p.fd) }

func (p *Port) Close() error {
	if p.closed.Swap(true) {
		return nil
	}
	p.reading.Lock()
	defer p.reading.Unlock()
	ioctl(p.fd, ioctlSetAttr, unsafe.Pointer(&p.saved))
	return syscall.Close(p.fd)
}

// FindPorts lists likely USB serial adapters.
func FindPorts() []string {
	var out []string
	for _, pat := range portPatterns {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	sort.Strings(out)
	return out
}
