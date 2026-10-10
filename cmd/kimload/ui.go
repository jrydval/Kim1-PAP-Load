package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// UI prints to stderr, with colour and a live progress bar when that is a
// terminal, and plain lines otherwise (for logs and pipes).
type UI struct {
	w     io.Writer
	tty   bool
	color bool
	live  bool // a progress line is on screen
}

func NewUI() *UI {
	fi, err := os.Stderr.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
	return &UI{w: os.Stderr, tty: tty, color: tty && os.Getenv("NO_COLOR") == ""}
}

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (u *UI) bold(s string) string   { return u.paint("1", s) }
func (u *UI) dim(s string) string    { return u.paint("2", s) }
func (u *UI) green(s string) string  { return u.paint("32", s) }
func (u *UI) red(s string) string    { return u.paint("31", s) }
func (u *UI) yellow(s string) string { return u.paint("33", s) }
func (u *UI) cyan(s string) string   { return u.paint("36", s) }

func (u *UI) clearLive() {
	if u.live {
		fmt.Fprint(u.w, "\r\x1b[K")
		u.live = false
	}
}

func (u *UI) Printf(format string, a ...any) {
	u.clearLive()
	fmt.Fprintf(u.w, format+"\n", a...)
}

func (u *UI) Info(label, format string, a ...any) {
	u.Printf("  %s %s", u.dim(fmt.Sprintf("%-8s", label)), fmt.Sprintf(format, a...))
}

func (u *UI) Warn(format string, a ...any) {
	u.Printf("  %s %s", u.yellow("!"), fmt.Sprintf(format, a...))
}

func (u *UI) OK(format string, a ...any) {
	u.Printf("%s %s", u.green("✓"), fmt.Sprintf(format, a...))
}

// Fail prints an error, what the board said, and hints.
func (u *UI) Fail(msg string, board string, hints []string) {
	u.Printf("%s %s", u.red("✗"), u.bold(msg))
	if b := strings.TrimSpace(board); b != "" {
		u.Printf("  %s %s", u.dim("board said:"), fmt.Sprintf("%q", b))
	}
	for _, h := range hints {
		u.Printf("  %s %s", u.cyan("→"), h)
	}
}

// Step shows an action in progress; call the returned func with the outcome.
func (u *UI) Step(msg string) func(ok bool, result string) {
	if u.tty {
		fmt.Fprintf(u.w, "  %s %s…", u.cyan("•"), msg)
		u.live = true
	}
	return func(ok bool, result string) {
		mark := u.green("✓")
		if !ok {
			mark = u.red("✗")
		}
		u.Printf("  %s %s %s", mark, msg, u.dim(result))
	}
}

// Bar draws a progress bar that redraws in place.
type Bar struct {
	u          *UI
	total      int // characters to send
	lines      []string
	recBytes   []int // data bytes per line (0 for the terminator)
	recAddr    []uint16
	totalBytes int
	estimate   time.Duration
	start      time.Time
	lastDraw   time.Time
	lastStep   int // last 10% step printed in plain mode
}

func (u *UI) NewBar(t *Tape, estimate time.Duration) *Bar {
	b := &Bar{u: u, lines: t.Lines(), estimate: estimate, start: time.Now(), totalBytes: t.Bytes(), lastStep: -1}
	for i, l := range b.lines {
		b.total += len(l) + 2
		if i < len(t.Records) {
			b.recBytes = append(b.recBytes, len(t.Records[i].Data))
			b.recAddr = append(b.recAddr, t.Records[i].Addr)
		} else {
			b.recBytes = append(b.recBytes, 0)
			b.recAddr = append(b.recAddr, 0)
		}
	}
	b.total -= 2 // no CR LF after the terminator
	return b
}

func (b *Bar) Update(p Progress) {
	now := time.Now()
	done := p.Chars >= b.total
	if b.u.tty && !done && now.Sub(b.lastDraw) < 80*time.Millisecond {
		return
	}
	b.lastDraw = now
	frac := float64(p.Chars) / float64(b.total)
	pct := int(frac * 100)
	bytes := 0
	for i := 0; i < p.Line && i < len(b.recBytes); i++ {
		bytes += b.recBytes[i]
	}
	elapsed := now.Sub(b.start)
	left := b.estimate - elapsed
	if frac > 0.05 {
		left = time.Duration(float64(elapsed)/frac) - elapsed
	}
	if left < 0 {
		left = 0
	}
	recs := len(b.lines) - 1
	rec := fmt.Sprintf("record %d/%d", min(p.Line+1, recs), recs)
	if p.Line >= recs {
		rec = "terminator"
	}
	addr := ""
	if p.Line < recs {
		addr = fmt.Sprintf("$%04X", b.recAddr[p.Line])
	}
	if !b.u.tty {
		if step := pct / 10; step > b.lastStep {
			b.lastStep = step
			b.u.Printf("  %3d%%  %s  %s  %d/%d bytes  %s elapsed", pct, rec, addr, bytes, b.totalBytes, clock(elapsed))
		}
		return
	}
	const width = 30
	fill := int(frac * width)
	bar := b.u.green(strings.Repeat("█", fill)) + b.u.dim(strings.Repeat("░", width-fill))
	fmt.Fprintf(b.u.w, "\r\x1b[K  %s %3d%%  %s  %s  %s  %s %s",
		bar, pct, rec, b.u.cyan(addr),
		b.u.dim(fmt.Sprintf("%d/%d B", bytes, b.totalBytes)),
		clock(elapsed), b.u.dim("~"+clock(left)+" left"))
	b.u.live = true
}

func clock(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
