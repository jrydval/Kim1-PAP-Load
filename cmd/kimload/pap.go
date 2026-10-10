package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Record is one data record of a KIM-1 paper tape (PAP) file:
//
//	;LLAAAADD..DDCCCC
//
// LL is the data byte count, AAAA the load address, DD the data and CCCC
// a 16-bit checksum: the sum of LL, both address bytes and every data byte.
type Record struct {
	Line int // 1-based line number in the source file
	Addr uint16
	Data []byte
}

// End returns the address of the record's last byte.
func (r Record) End() uint16 { return r.Addr + uint16(len(r.Data)) - 1 }

// PAP renders the record as a paper tape line (without line ending),
// always in upper case, which is what the monitor's hex parser expects.
func (r Record) PAP() string {
	var b strings.Builder
	sum := uint16(len(r.Data)) + r.Addr>>8 + r.Addr&0xFF
	fmt.Fprintf(&b, ";%02X%04X", len(r.Data), r.Addr)
	for _, d := range r.Data {
		fmt.Fprintf(&b, "%02X", d)
		sum += uint16(d)
	}
	fmt.Fprintf(&b, "%04X", sum)
	return b.String()
}

// TerminatorPAP renders the closing record: zero data bytes, with the
// number of data records in the address field.
func TerminatorPAP(count int) string {
	n := uint16(count)
	return fmt.Sprintf(";00%04X%04X", n, n>>8+n&0xFF)
}

// Tape is a parsed PAP file.
type Tape struct {
	Records  []Record
	Warnings []string
}

// Bytes returns the total number of data bytes on the tape.
func (t *Tape) Bytes() int {
	n := 0
	for _, r := range t.Records {
		n += len(r.Data)
	}
	return n
}

// Lines returns every line to send, terminator included.
func (t *Tape) Lines() []string {
	lines := make([]string, 0, len(t.Records)+1)
	for _, r := range t.Records {
		lines = append(lines, r.PAP())
	}
	return append(lines, TerminatorPAP(len(t.Records)))
}

// Segment is a contiguous range of memory the tape writes.
type Segment struct{ Start, End uint16 }

func (s Segment) Len() int { return int(s.End) - int(s.Start) + 1 }

// Segments merges the records into contiguous address ranges, in tape order.
func (t *Tape) Segments() []Segment {
	var segs []Segment
	for _, r := range t.Records {
		if n := len(segs); n > 0 && segs[n-1].End+1 == r.Addr {
			segs[n-1].End = r.End()
			continue
		}
		segs = append(segs, Segment{r.Addr, r.End()})
	}
	return segs
}

// ParseError points at the offending place in the source file.
type ParseError struct {
	Line int
	Msg  string
	Text string // the offending line
}

func (e *ParseError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// ParsePAP reads a PAP file. Everything before the first ';' on a line is
// ignored, as are blank lines and NUL leader, just like the KIM-1 loader
// does. A missing terminator record is not an error: one is generated.
func ParsePAP(r io.Reader) (*Tape, error) {
	t := &Tape{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	sawEnd := false
	for line := 1; sc.Scan(); line++ {
		raw := sc.Text()
		text := strings.TrimSpace(strings.Trim(raw, "\x00\x7f"))
		if text == "" {
			continue
		}
		semi := strings.IndexByte(text, ';')
		if semi < 0 {
			t.Warnings = append(t.Warnings, fmt.Sprintf("line %d: no ';', ignored", line))
			continue
		}
		if sawEnd {
			t.Warnings = append(t.Warnings, fmt.Sprintf("line %d: record after the terminator, ignored", line))
			continue
		}
		rec, count, err := parseRecord(text[semi:], line)
		if err != nil {
			err.Text = raw
			return nil, err
		}
		if rec == nil {
			sawEnd = true
			if count != len(t.Records) {
				t.Warnings = append(t.Warnings, fmt.Sprintf(
					"line %d: terminator says %d records, file has %d (a correct terminator will be sent)",
					line, count, len(t.Records)))
			}
			continue
		}
		t.Records = append(t.Records, *rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(t.Records) == 0 {
		return nil, fmt.Errorf("no data records found")
	}
	if !sawEnd {
		t.Warnings = append(t.Warnings, "no terminator record (;00…), one will be generated")
	}
	for _, r := range t.Records {
		if r.Addr <= 0x1FFF && r.End() >= 0x1800 {
			t.Warnings = append(t.Warnings, fmt.Sprintf("line %d: $%04X-$%04X overlaps the monitor ROM ($1800-$1FFF)", r.Line, r.Addr, r.End()))
		} else if r.Addr <= 0x173F && r.End() >= 0x1700 {
			t.Warnings = append(t.Warnings, fmt.Sprintf("line %d: $%04X-$%04X overlaps RIOT I/O registers ($1700-$173F)", r.Line, r.Addr, r.End()))
		}
	}
	return t, nil
}

// parseRecord parses one ";..." record. It returns a nil record (and the
// record count from the address field) for the terminator.
func parseRecord(s string, line int) (*Record, int, *ParseError) {
	fail := func(format string, a ...any) (*Record, int, *ParseError) {
		return nil, 0, &ParseError{Line: line, Msg: fmt.Sprintf(format, a...)}
	}
	hex := s[1:]
	for i, c := range hex {
		if !strings.ContainsRune("0123456789ABCDEFabcdef", c) {
			return fail("invalid character %q at column %d", c, i+2+strings.IndexByte(s, ';'))
		}
	}
	if len(hex) < 10 {
		return fail("record too short (%d hex digits, need at least 10)", len(hex))
	}
	byteAt := func(i int) byte { v, _ := strconv.ParseUint(hex[2*i:2*i+2], 16, 8); return byte(v) }
	n := int(byteAt(0))
	if want := 2 + 4 + 2*n + 4; len(hex) != want {
		return fail("length byte says %d data bytes, so the record should have %d hex digits, it has %d", n, want, len(hex))
	}
	addr := uint16(byteAt(1))<<8 | uint16(byteAt(2))
	sum := uint16(n) + uint16(byteAt(1)) + uint16(byteAt(2))
	data := make([]byte, n)
	for i := range data {
		data[i] = byteAt(3 + i)
		sum += uint16(data[i])
	}
	got := uint16(byteAt(3+n))<<8 | uint16(byteAt(4+n))
	if got != sum {
		return fail("checksum mismatch: record has %04X, computed %04X", got, sum)
	}
	if n == 0 {
		return nil, int(addr), nil
	}
	if int(addr)+n > 0x10000 {
		return fail("record at $%04X with %d bytes runs past $FFFF", addr, n)
	}
	return &Record{Line: line, Addr: addr, Data: data}, 0, nil
}
