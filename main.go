// Command kimload sends a paper tape (PAP) file to a KIM-1 (or a clone
// such as the PAL-1/PAL-2) through the monitor's L command over the TTY
// serial port, showing progress and explaining what went wrong if the
// transfer fails.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	port := flag.String("port", os.Getenv("KIMLOAD_PORT"), "serial `device` (default: $KIMLOAD_PORT, or the only USB serial adapter found)")
	baud := flag.Int("baud", 1200, "baud `rate`")
	charDelay := flag.Duration("char-delay", 3*time.Millisecond, "extra pause after each character")
	lineDelay := flag.Duration("line-delay", 30*time.Millisecond, "extra pause after each record")
	goAddr := flag.String("go", "", "after loading, start the program at this hex `address` (\"auto\" = the first record's address)")
	dryRun := flag.Bool("n", false, "only check the file, don't send anything")
	verbose := flag.Bool("v", false, "show everything the board sends")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: kimload [flags] file.pap\n\n"+
			"Sends a KIM-1 paper tape file through the monitor's L command.\n"+
			"The board must be in TTY mode, showing the monitor (press RS if unsure).\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return 2
	}
	u := NewUI()
	path := flag.Arg(0)

	// --- The file ---
	f, err := os.Open(path)
	if err != nil {
		u.Fail(err.Error(), "", nil)
		return 1
	}
	tape, err := ParsePAP(f)
	f.Close()
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			u.Fail(fmt.Sprintf("%s:%d: %s", path, pe.Line, pe.Msg), "", nil)
			u.Printf("  %s %s", u.dim(fmt.Sprintf("%5d |", pe.Line)), strings.TrimRight(pe.Text, "\r\n"))
		} else {
			u.Fail(fmt.Sprintf("%s: %s", path, err), "", nil)
		}
		return 1
	}
	u.Printf("%s %s", u.bold("kimload"), filepath.Base(path))
	var segs []string
	for _, s := range tape.Segments() {
		segs = append(segs, fmt.Sprintf("$%04X-$%04X (%d B)", s.Start, s.End, s.Len()))
	}
	u.Info("tape", "%d records, %d bytes", len(tape.Records), tape.Bytes())
	u.Info("memory", "%s", strings.Join(segs, ", "))
	for _, w := range tape.Warnings {
		u.Warn("%s", w)
	}

	lines := tape.Lines()
	chars := 1 // the L command
	for _, l := range lines {
		chars += len(l) + 2
	}
	estimate := time.Duration(chars)*(time.Duration(10*float64(time.Second)/float64(*baud))+*charDelay+time.Millisecond) +
		time.Duration(len(lines))*(*lineDelay)
	u.Info("time", "about %s at %d baud", clock(estimate), *baud)

	var start uint16
	if *goAddr != "" {
		if strings.EqualFold(*goAddr, "auto") {
			start = tape.Records[0].Addr
		} else {
			v, err := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(*goAddr, "$"), "0x"), 16, 16)
			if err != nil {
				u.Fail(fmt.Sprintf("-go %s: not a hex address", *goAddr), "", nil)
				return 2
			}
			start = uint16(v)
		}
	}
	if *dryRun {
		u.OK("file is valid (dry run, nothing sent)")
		return 0
	}

	// --- The board ---
	if *port == "" {
		found := FindPorts()
		switch len(found) {
		case 1:
			*port = found[0]
		case 0:
			u.Fail("no serial port found", "", []string{"connect the board, or name the port with -port"})
			return 1
		default:
			u.Fail("more than one serial port found", "", []string{"pick one with -port: " + strings.Join(found, ", ")})
			return 1
		}
	}
	p, err := OpenPort(*port, *baud)
	if err != nil {
		u.Fail(err.Error(), "", nil)
		return 1
	}
	defer p.Close()
	u.Info("port", "%s @ %d baud", *port, *baud)

	m := NewMonitor(p, *baud)
	m.CharDelay = *charDelay
	m.LineDelay = *lineDelay
	var trace strings.Builder
	if *verbose {
		m.Trace = func(b byte) { trace.WriteByte(b) }
	}
	showTrace := func() {
		if *verbose && trace.Len() > 0 {
			u.Printf("  %s %q", u.dim("received:"), trace.String())
			trace.Reset()
		}
	}

	done := u.Step("waking the KIM-1 monitor")
	addr, calibrated, err := m.Wake()
	if err != nil {
		done(false, "")
		showTrace()
		report(u, err)
		return 1
	}
	how := "already listening"
	if calibrated {
		how = "after reset, baud rate calibrated"
	}
	done(true, fmt.Sprintf("at $%s, %s", addr, how))
	showTrace()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	bar := u.NewBar(tape, estimate)
	began := time.Now()
	err = m.Load(ctx, lines, bar.Update)
	u.clearLive()
	showTrace()
	if err != nil {
		var le *LoadError
		if errors.As(err, &le) {
			where := "the L command"
			if le.Line >= len(tape.Records) {
				where = "the terminator record"
			} else if le.Line >= 0 {
				r := tape.Records[le.Line]
				where = fmt.Sprintf("record %d/%d ($%04X, line %d of the file)", le.Line+1, len(tape.Records), r.Addr, r.Line)
			}
			le.Msg = fmt.Sprintf("%s at %s", le.Msg, where)
		}
		report(u, err)
		return 1
	}
	took := time.Since(began)
	u.OK("loaded %d bytes in %d records in %s %s", tape.Bytes(), len(tape.Records), clock(took),
		u.dim(fmt.Sprintf("(%.0f B/s)", float64(tape.Bytes())/took.Seconds())))

	if *goAddr == "" {
		u.Printf("  %s start it in the monitor with: %s", u.cyan("→"), u.bold(fmt.Sprintf("%04X G", tape.Records[0].Addr)))
		return 0
	}
	done = u.Step(fmt.Sprintf("starting the program at $%04X", start))
	if err := m.Go(start); err != nil {
		done(false, "")
		showTrace()
		report(u, err)
		return 1
	}
	done(true, "")
	return 0
}

func report(u *UI, err error) {
	var le *LoadError
	var be *BoardError
	switch {
	case errors.As(err, &le):
		u.Fail(le.Msg, le.Board, le.Hints)
	case errors.As(err, &be):
		u.Fail(be.Msg, be.Board, be.Hints)
	default:
		u.Fail(err.Error(), "", nil)
	}
}
