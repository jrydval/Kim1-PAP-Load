package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const sample = "\r\n" +
	";1820004C85204CBD204C5A1E4CA01E4CD0285F1882800486C3900508BF\r\n" +
	";18201886C391C260B1C2A0006062216421D82105223322FD219F230A1C\r\n" +
	";0000020002\r\n"

func TestParsePAP(t *testing.T) {
	tape, err := ParsePAP(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(tape.Records) != 2 || tape.Bytes() != 48 || len(tape.Warnings) != 0 {
		t.Fatalf("got %d records, %d bytes, warnings %v", len(tape.Records), tape.Bytes(), tape.Warnings)
	}
	if r := tape.Records[1]; r.Addr != 0x2018 || r.Line != 3 || r.Data[0] != 0x86 {
		t.Fatalf("record 2 = %+v", r)
	}
	if segs := tape.Segments(); len(segs) != 1 || segs[0] != (Segment{0x2000, 0x202F}) {
		t.Fatalf("segments = %v", segs)
	}
	want := []string{
		";1820004C85204CBD204C5A1E4CA01E4CD0285F1882800486C3900508BF",
		";18201886C391C260B1C2A0006062216421D82105223322FD219F230A1C",
		";0000020002",
	}
	if got := tape.Lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines =\n%s", strings.Join(got, "\n"))
	}
}

func TestParsePAPErrors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{";0320000102030009\n", "checksum mismatch"},
		{";03200001020300\n", "should have 16 hex digits"},
		{";0320000102G30029\n", "invalid character 'G' at column 12"},
		{";032\n", "too short"},
		{";04FFFE01020304020B\n", "runs past $FFFF"},
		{"\n\n", "no data records"},
	} {
		_, err := ParsePAP(strings.NewReader(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParsePAP(%q) = %v, want %q", tc.in, err, tc.want)
		}
	}
}

func TestParsePAPWarnings(t *testing.T) {
	tape, err := ParsePAP(strings.NewReader("garbage\n;0320000102030029\n"))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(tape.Warnings, "|")
	if !strings.Contains(w, "no ';'") || !strings.Contains(w, "no terminator") {
		t.Fatalf("warnings = %v", tape.Warnings)
	}
	if got := tape.Lines()[1]; got != ";0000010001" {
		t.Fatalf("generated terminator = %s", got)
	}
}

// fakeKIM imitates the monitor ROM in TTY mode: every byte is echoed by
// "hardware", CR prints a new prompt, RUBOUT after reset prints KIM, and
// L runs the paper tape loader (ERR on a bad checksum, KIM at the end).
type fakeKIM struct {
	mu      sync.Mutex
	out     []byte
	reset   bool // waiting for RUBOUT
	tb      bool // a program with a ':' prompt is running
	loading bool
	rec     string
	corrupt int // corrupt the n-th data record (1-based), 0 = never
	nrec    int
	mem     map[uint16]byte
}

func (k *fakeKIM) Read(b []byte) (int, error) {
	k.mu.Lock()
	n := copy(b, k.out)
	k.out = k.out[n:]
	k.mu.Unlock()
	if n == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	return n, nil
}

func (k *fakeKIM) Drain() error { return nil }

func (k *fakeKIM) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, c := range b {
		k.out = append(k.out, c) // hardware echo
		switch {
		case k.tb:
			if c == '\r' {
				k.say("\r\n:\x91")
			}
		case k.reset:
			if c == 0x7F {
				k.reset = false
				k.say("\r\nKIM\r\n0000 00 ")
			}
		case k.loading:
			k.load(c)
		case c == '\r':
			k.say("\r\n2000 4C ")
		case c == 'L':
			k.loading = true
		}
	}
	return len(b), nil
}

func (k *fakeKIM) say(s string) { k.out = append(k.out, s...) }

func (k *fakeKIM) load(c byte) {
	if k.rec == "" && c != ';' {
		return
	}
	k.rec += string(c)
	if len(k.rec) < 3 {
		return
	}
	var n int
	fmt.Sscanf(k.rec[1:3], "%02X", &n)
	if len(k.rec) < 1+2+4+2*n+4 {
		return
	}
	rec := k.rec
	k.rec = ""
	if n > 0 {
		k.nrec++
		if k.nrec == k.corrupt {
			rec = rec[:9] + "0" + rec[10:]
		}
	}
	r, _, perr := parseRecord(rec, 0)
	if perr != nil {
		k.loading = false
		k.say("ERR KIM\r\n2000 4C ")
		return
	}
	if r == nil {
		k.loading = false
		k.say("KIM\r\n0002 A8 ")
		return
	}
	for i, d := range r.Data {
		k.mem[r.Addr+uint16(i)] = d
	}
}

func newTestMonitor(k *fakeKIM) *Monitor {
	if k.mem == nil {
		k.mem = map[uint16]byte{}
	}
	m := NewMonitor(k, 115200)
	m.CharDelay, m.LineDelay = 0, 0
	return m
}

func TestWake(t *testing.T) {
	addr, cal, err := newTestMonitor(&fakeKIM{}).Wake()
	if err != nil || addr != "2000" || cal {
		t.Fatalf("Wake() = %q, %v, %v", addr, cal, err)
	}
	addr, cal, err = newTestMonitor(&fakeKIM{reset: true}).Wake()
	if err != nil || addr != "0000" || !cal {
		t.Fatalf("Wake() after reset = %q, %v, %v", addr, cal, err)
	}
	_, _, err = newTestMonitor(&fakeKIM{tb: true}).Wake()
	var be *BoardError
	if !errors.As(err, &be) || !strings.Contains(be.Hints[0], "press RS") {
		t.Fatalf("Wake() with a program running = %v", err)
	}
}

func TestLoad(t *testing.T) {
	tape, _ := ParsePAP(strings.NewReader(sample))
	k := &fakeKIM{}
	m := newTestMonitor(k)
	var last Progress
	if err := m.Load(context.Background(), tape.Lines(), func(p Progress) { last = p }); err != nil {
		t.Fatal(err)
	}
	if k.mem[0x2000] != 0x4C || k.mem[0x202F] != 0x23 || len(k.mem) != 48 {
		t.Fatalf("memory: %d bytes, $2000=%02X $202F=%02X", len(k.mem), k.mem[0x2000], k.mem[0x202F])
	}
	if want := len(tape.Lines()[0]) + len(tape.Lines()[1]) + 4 + len(tape.Lines()[2]); last.Chars != want || last.Line != 2 {
		t.Fatalf("last progress = %+v, want %d chars on line 2", last, want)
	}
}

func TestLoadReportsChecksumError(t *testing.T) {
	tape, _ := ParsePAP(strings.NewReader(sample))
	k := &fakeKIM{corrupt: 2}
	err := newTestMonitor(k).Load(context.Background(), tape.Lines(), nil)
	var le *LoadError
	if !errors.As(err, &le) || le.Line != 1 || !strings.Contains(le.Msg, "checksum") || !strings.Contains(le.Board, "ERR") {
		t.Fatalf("Load() = %#v", err)
	}
	if k.loading {
		t.Fatal("the fake should have left the loader")
	}
}

type deadLine struct{}

func (deadLine) Read([]byte) (int, error)    { time.Sleep(5 * time.Millisecond); return 0, nil }
func (deadLine) Write(b []byte) (int, error) { return len(b), nil }
func (deadLine) Drain() error                { return nil }

func TestLoadNoEcho(t *testing.T) {
	m := NewMonitor(deadLine{}, 115200)
	err := m.Load(context.Background(), []string{";0000000000"}, nil)
	var le *LoadError
	if !errors.As(err, &le) || le.Line != -1 || !strings.Contains(le.Msg, "no echo") {
		t.Fatalf("Load() = %v", err)
	}
}

func TestLoadInterrupted(t *testing.T) {
	tape, _ := ParsePAP(strings.NewReader(sample))
	ctx, cancel := context.WithCancel(context.Background())
	m := newTestMonitor(&fakeKIM{})
	err := m.Load(ctx, tape.Lines(), func(p Progress) {
		if p.Chars == 20 {
			cancel()
		}
	})
	var le *LoadError
	if !errors.As(err, &le) || le.Msg != "interrupted" || le.Line != 0 {
		t.Fatalf("Load() = %v", err)
	}
}
