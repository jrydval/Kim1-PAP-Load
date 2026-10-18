package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Conn is the serial link to the board. Read returns 0, nil on a timeout.
type Conn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Drain() error
}

// Monitor talks to the KIM-1 monitor ROM in TTY mode.
//
// Two properties of the KIM-1 shape everything here. First, the echo of
// typed characters is pure hardware (the incoming line is gated straight
// onto the outgoing one), so every byte sent comes back at once whatever
// the CPU is doing; waiting for it paces the transfer and catches a dead
// or noisy line. Second, the serial port is bit-banged: the CPU has to be
// back in GETCH before the next start bit arrives, which is what the
// extra per-character and per-line delays are for.
type Monitor struct {
	conn      Conn
	in        chan byte
	errc      chan error
	baud      int
	charTime  time.Duration
	CharDelay time.Duration
	LineDelay time.Duration
	// Trace, if set, gets every byte received from the board.
	Trace func(b byte)
}

func NewMonitor(conn Conn, baud int) *Monitor {
	m := &Monitor{
		conn:     conn,
		in:       make(chan byte, 4096),
		errc:     make(chan error, 1),
		baud:     baud,
		charTime: time.Duration(10 * float64(time.Second) / float64(baud)),
	}
	go m.reader()
	return m
}

func (m *Monitor) reader() {
	buf := make([]byte, 256)
	for {
		n, err := m.conn.Read(buf)
		if err != nil {
			m.errc <- err
			return
		}
		for _, b := range buf[:n] {
			m.in <- b
		}
	}
}

var errTimeout = errors.New("timeout")

// recv returns the next byte from the board, or errTimeout at the deadline.
func (m *Monitor) recv(deadline time.Time) (byte, error) {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case b := <-m.in:
		if m.Trace != nil {
			m.Trace(b)
		}
		return b, nil
	case err := <-m.errc:
		return 0, fmt.Errorf("serial port: %w", err)
	case <-t.C:
		return 0, errTimeout
	}
}

// clean maps a received byte to text the way a teletype would print it:
// bit 7 is ignored, NUL and RUBOUT (padding) vanish.
func clean(b byte) (byte, bool) {
	b &= 0x7F
	return b, b != 0 && b != 0x7F
}

// collect reads board output until done says so or the time is up.
func (m *Monitor) collect(d time.Duration, done func(string) bool) (string, error) {
	var s strings.Builder
	deadline := time.Now().Add(d)
	for {
		b, err := m.recv(deadline)
		if err == errTimeout {
			return s.String(), nil
		}
		if err != nil {
			return s.String(), err
		}
		if c, ok := clean(b); ok {
			s.WriteByte(c)
			if done != nil && done(s.String()) {
				return s.String(), nil
			}
		}
	}
}

// quiet discards input until the line has been silent for d.
func (m *Monitor) quiet(d time.Duration) error {
	for {
		_, err := m.recv(time.Now().Add(d))
		if err == errTimeout {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (m *Monitor) send(s string) error {
	if _, err := m.conn.Write([]byte(s)); err != nil {
		return fmt.Errorf("serial port: %w", err)
	}
	return m.conn.Drain()
}

// promptRe matches the monitor's "AAAA DD " address/data prompt.
var promptRe = regexp.MustCompile(`([0-9A-F]{4}) [0-9A-F]{2} $`)

func hasPrompt(s string) bool { return promptRe.MatchString(s) }

// BoardError is a failure with advice on what to do about it.
type BoardError struct {
	Msg   string
	Board string // what the board printed, if anything
	Hints []string
}

func (e *BoardError) Error() string { return e.Msg }

// Wake makes sure the monitor is listening and returns the address it is
// showing. A CR gets a fresh prompt from a monitor that is already
// calibrated; after a reset it first needs a RUBOUT to measure the baud
// rate, and answers with "KIM".
func (m *Monitor) Wake() (addr string, calibrated bool, err error) {
	if err := m.quiet(150 * time.Millisecond); err != nil {
		return "", false, err
	}
	if err := m.send("\r"); err != nil {
		return "", false, err
	}
	got, err := m.collect(1500*time.Millisecond, hasPrompt)
	if err != nil {
		return "", false, err
	}
	if hasPrompt(got) {
		return promptRe.FindStringSubmatch(got)[1], false, nil
	}
	if strings.Contains(got, ":") || strings.Contains(got, ">") {
		return "", false, &BoardError{
			Msg:   "the board answered with a program prompt, not the KIM monitor",
			Board: got,
			Hints: []string{"a program (Tiny BASIC?) is running - press RS on the board and try again"},
		}
	}
	if err := m.send("\x7f"); err != nil {
		return "", false, err
	}
	got2, err := m.collect(2500*time.Millisecond, hasPrompt)
	if err != nil {
		return "", false, err
	}
	if hasPrompt(got2) {
		return promptRe.FindStringSubmatch(got2)[1], true, nil
	}
	board := strings.TrimSpace(got + got2)
	hints := []string{
		"press RS on the board, so the monitor re-measures the baud rate",
		"check that the TTY/keypad switch is set to TTY",
	}
	if board == "" || board == "\x7f" || board == "\r" {
		hints = append([]string{"no echo at all: check the port, the cable and that the board is powered"}, hints...)
	} else {
		hints = append([]string{"the reply looks garbled: the board may be calibrated to a different baud rate"}, hints...)
	}
	return "", false, &BoardError{Msg: "the KIM-1 monitor does not answer", Board: board, Hints: hints}
}

// Progress is reported after every character sent.
type Progress struct {
	Line  int // index of the line being sent
	Chars int // characters sent so far, over all lines
}

// LoadError says which record the transfer stopped at.
type LoadError struct {
	BoardError
	Line int // index into the lines passed to Load
}

// Load sends the lines (records, terminator last) to the monitor's L
// command and checks the board's verdict.
func (m *Monitor) Load(ctx context.Context, lines []string, progress func(Progress)) error {
	if err := m.quiet(100 * time.Millisecond); err != nil {
		return err
	}
	if err := m.sendEcho('L', -1); err != nil {
		return err
	}
	time.Sleep(m.LineDelay)
	chars := 0
	for i, line := range lines {
		last := i == len(lines)-1
		text := line + "\r\n"
		if last {
			// The monitor answers right after the last checksum digit;
			// anything typed after that would be taken as a command.
			text = line
		}
		for j := 0; j < len(text); j++ {
			if ctx.Err() != nil {
				return &LoadError{Line: i, BoardError: BoardError{
					Msg:   "interrupted",
					Hints: []string{"the board's loader is still waiting for data - press RS before the next attempt"},
				}}
			}
			if err := m.sendEcho(text[j], i); err != nil {
				return err
			}
			chars++
			if progress != nil {
				progress(Progress{Line: i, Chars: chars})
			}
			time.Sleep(m.CharDelay)
		}
		if !last {
			time.Sleep(m.LineDelay)
		}
	}
	got, err := m.collect(3*time.Second, func(s string) bool {
		return strings.Contains(s, "ERR") || hasPrompt(s)
	})
	if err != nil {
		return err
	}
	switch {
	case strings.Contains(got, "ERR"):
		return m.rejected(len(lines)-1, got)
	case strings.Contains(got, "KIM"):
		return nil
	}
	return &LoadError{Line: len(lines) - 1, BoardError: BoardError{
		Msg:   "the board did not confirm the end of the tape",
		Board: got,
		Hints: []string{"the data may still have loaded; check it in the monitor"},
	}}
}

// sendEcho sends one character and waits for the board's hardware echo.
// line is the index of the record being sent (-1 for the L command).
func (m *Monitor) sendEcho(c byte, line int) error {
	if err := m.send(string(c)); err != nil {
		return err
	}
	deadline := time.Now().Add(20*m.charTime + 300*time.Millisecond)
	for {
		b, err := m.recv(deadline)
		if err == errTimeout {
			return &LoadError{Line: line, BoardError: BoardError{
				Msg: fmt.Sprintf("no echo from the board for %s", printable(c)),
				Hints: []string{
					"is the board powered and the cable connected?",
					"check that the TTY/keypad switch is set to TTY",
				},
			}}
		}
		if err != nil {
			return err
		}
		if b == c {
			return nil
		}
		if _, ok := clean(b); !ok {
			continue
		}
		// Not our echo: the monitor is talking. Hear it out.
		rest, _ := m.collect(500*time.Millisecond, hasPrompt)
		got := string(b&0x7F) + rest
		if strings.Contains(got, "ERR") {
			return m.rejected(line, got)
		}
		return &LoadError{Line: line, BoardError: BoardError{
			Msg:   fmt.Sprintf("unexpected reply while sending %s", printable(c)),
			Board: got,
			Hints: []string{
				"the line may be noisy, or the board left the loader",
				"press RS and try again, perhaps with a longer -char-delay",
			},
		}}
	}
}

func (m *Monitor) rejected(line int, got string) error {
	return &LoadError{Line: line, BoardError: BoardError{
		Msg:   "the board rejected the record (checksum error)",
		Board: got,
		Hints: []string{
			"the record arrived corrupted - the CPU probably missed a bit",
			"try again with a longer -char-delay (e.g. -char-delay 10ms)",
			"the records before this one are loaded; the board is back in the monitor",
		},
	}}
}

// Go starts the program at addr from the monitor prompt.
func (m *Monitor) Go(addr uint16) error {
	if err := m.command(fmt.Sprintf("%04X ", addr), addr); err != nil {
		return err
	}
	if err := m.send("G"); err != nil {
		return err
	}
	return m.quiet(200 * time.Millisecond)
}

// command types s at the monitor prompt and waits for the prompt that
// shows addr.
func (m *Monitor) command(s string, addr uint16) error {
	if err := m.send(s); err != nil {
		return err
	}
	want := fmt.Sprintf("%04X", addr)
	got, err := m.collect(2*time.Second, func(s string) bool {
		p := promptRe.FindStringSubmatch(s)
		return p != nil && p[1] == want
	})
	if err != nil {
		return err
	}
	if p := promptRe.FindStringSubmatch(got); p == nil || p[1] != want {
		return &BoardError{
			Msg:   fmt.Sprintf("the monitor did not answer %q with address %s", strings.TrimSpace(s), want),
			Board: got,
			Hints: []string{"press RS on the board and try again"},
		}
	}
	return nil
}

// Dump-related monitor locations and the shape of what Q prints.
const (
	eaAddr       = 0x17F7 // end address for Q, low byte first
	dumpRecBytes = 24     // Q always punches full 24-byte records
	dumpRecChars = 67     // CR LF, 6 NULs, ';', count, address, data, checksum
)

var dumpEndRe = regexp.MustCompile(`;00[0-9A-F]{8}`)

// DumpTime estimates how long Dump takes for n bytes.
func DumpTime(n, baud int) time.Duration {
	recs := (n + dumpRecBytes - 1) / dumpRecBytes
	chars := (recs+1)*dumpRecChars + 3*8 // the records and the commands that set up Q
	return time.Duration(chars) * time.Duration(10*float64(time.Second)/float64(baud))
}

// Dump reads memory from start to end (inclusive) with the monitor's Q
// command, which punches it as paper tape: 24-byte records from the open
// address for as long as that is below the end address stored at
// $17F7/$17F8, then a terminator. The last record can run past end. Dump
// overwrites $17F7/$17F8; afterwards the monitor waits for a command
// without printing a prompt.
func (m *Monitor) Dump(start, end uint16, progress func(bytes int)) (map[uint16]byte, error) {
	ea := end + 1
	if end == 0xFFFF {
		ea = 0xFFFF
	}
	if err := m.quiet(100 * time.Millisecond); err != nil {
		return nil, err
	}
	for _, c := range []struct {
		s    string
		addr uint16
	}{
		{fmt.Sprintf("%04X ", eaAddr), eaAddr},
		{fmt.Sprintf("%02X.", byte(ea)), eaAddr + 1},
		{fmt.Sprintf("%02X.", byte(ea>>8)), eaAddr + 2},
		{fmt.Sprintf("%04X ", start), start},
	} {
		if err := m.command(c.s, c.addr); err != nil {
			return nil, err
		}
	}
	if err := m.send("Q"); err != nil {
		return nil, err
	}
	n := int(end) - int(start) + 1
	got, err := m.collect(2*DumpTime(n, m.baud)+3*time.Second, func(s string) bool {
		if progress != nil && strings.HasSuffix(s, ";") {
			progress(min(strings.Count(s, ";")-1, (n+dumpRecBytes-1)/dumpRecBytes) * dumpRecBytes)
		}
		return dumpEndRe.MatchString(s)
	})
	if err != nil {
		return nil, err
	}
	if !dumpEndRe.MatchString(got) {
		return nil, &BoardError{
			Msg:   "the memory dump stopped before its end",
			Board: lastLine(got),
			Hints: []string{"press RS on the board and try again"},
		}
	}
	tape, err := ParsePAP(strings.NewReader(got))
	if err != nil {
		return nil, &BoardError{
			Msg:   "the memory dump arrived corrupted: " + err.Error(),
			Hints: []string{"the line from the board is noisy - try again"},
		}
	}
	mem := make(map[uint16]byte, n)
	for _, r := range tape.Records {
		for i, d := range r.Data {
			mem[r.Addr+uint16(i)] = d
		}
	}
	return mem, nil
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	return s[strings.LastIndexAny(s, "\r\n")+1:]
}

func printable(c byte) string {
	switch {
	case c == '\r':
		return "CR"
	case c == '\n':
		return "LF"
	case c < 0x20 || c >= 0x7F:
		return fmt.Sprintf("$%02X", c)
	}
	return fmt.Sprintf("'%c'", c)
}
