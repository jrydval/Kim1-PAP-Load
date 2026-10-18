# Kim1-PAP-Load: KIM-1 PAP Load Util in Golang

`kimload` sends a paper tape (`.pap` / `.ptp`) file to a **KIM-1**, or to a
clone such as the **PAL-1 / PAL-2**, over the TTY serial port. It drives the
monitor's `L` command for you, shows a live progress bar, and tells you in
plain words what went wrong if the transfer fails.

```
kimload TinyBasic_2000.ptp
  tape     96 records, 2304 bytes
  memory   $2000-$28FF (2304 B)
  time     about 1:15 at 1200 baud
  port     /dev/cu.usbserial-AH06LA82 @ 1200 baud
  ✓ waking the KIM-1 monitor at $2000, already listening
  ██████████████░░░░░░░░░░░░░░░░  47%  record 46/96  $2438  1080/2304 B  0:35 ~0:39 left
```

It is a single static binary with no dependencies outside the Go standard
library.

## Features

- **Checks the file before sending it.** It verifies every record's length and
  checksum. Errors point at the line and column. You get warnings when the
  terminator record is missing or wrong (a correct one is generated) and when
  data would overwrite the monitor ROM or the RIOT I/O registers.
- **Wakes the monitor.** If the monitor is already listening, a CR is enough.
  After a reset it sends a RUBOUT so the KIM-1 can measure the baud rate.
- **Paces the transfer.** It waits for the board's hardware echo of every
  character and adds small per-character and per-record delays, so the
  bit-banged serial routine in the monitor doesn't miss bits.
- **Explains failures.** For a checksum error, no echo, a garbled reply or a
  running program instead of the monitor, it shows which record failed, what
  the board printed and what to try next.
- **Verifies the load** on request (`-verify`). It reads the memory back with
  the monitor's `Q` command and compares it byte by byte with the file.
- **Can start the program** after loading (`-go auto` or `-go 0200`).
- **Plays well with scripts.** Without a terminal it prints plain progress
  lines instead of the animated bar. `NO_COLOR` is respected.

## Install

You need Go 1.27 or newer.

```sh
go install github.com/jrydval/Kim1-PAP-Load/cmd/kimload@latest
```

Or from a clone:

```sh
git clone https://github.com/jrydval/Kim1-PAP-Load.git
cd Kim1-PAP-Load
go build ./cmd/kimload
```

Supported platforms: **macOS** and **Linux**. The serial port is driven
directly through termios, so there is nothing else to install.

## Usage

```
kimload [flags] file.pap
```

1. Connect the board through a USB-serial adapter and switch it to **TTY**
   mode.
2. Close any terminal program that has the port open. `kimload` needs it
   exclusively.
3. Run:

```sh
kimload program.pap                 # load, then print how to start it
kimload -go auto program.pap        # load and start at the first record's address
kimload -port /dev/ttyUSB0 prog.pap # pick the port explicitly
kimload -verify program.pap         # load, then read it back and compare
kimload -n program.pap              # just check the file, send nothing
```

If exactly one USB serial adapter is connected, it is found automatically
(`/dev/cu.usbserial*`, `/dev/cu.usbmodem*`, … on macOS and `/dev/ttyUSB*`,
`/dev/ttyACM*` on Linux). You can also set it once with
`export KIMLOAD_PORT=/dev/…`.

### Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-port device` | `$KIMLOAD_PORT` or auto-detect | Serial device |
| `-baud rate` | `1200` | Baud rate |
| `-char-delay d` | `3ms` | Extra pause after each character |
| `-line-delay d` | `30ms` | Extra pause after each record |
| `-verify` | – | Read the memory back after loading and compare it with the file |
| `-go address` | – | Start the program after loading; hex address or `auto` |
| `-n` | – | Dry run: validate the file only |
| `-v` | – | Show everything the board sends |

Exit status is `0` on success, `1` on a failed transfer or an invalid file,
and `2` on bad usage.

## The PAP format

Each data record is one line:

```
;LLAAAADD…DDCCCC
```

| Field | Meaning |
|-------|---------|
| `LL` | number of data bytes (hex) |
| `AAAA` | load address |
| `DD…` | data bytes |
| `CCCC` | 16-bit checksum: the sum of `LL`, both address bytes and all data bytes |

The tape ends with a record with zero data bytes. Its address field holds
the number of data records, for example `;0000600060`. As on the real
loader, anything before the `;` on a line, blank lines and NUL leader are
ignored.

## Verifying a load

With `-verify`, `kimload` reads back every memory range the tape wrote. It
uses the monitor's `Q` (punch paper tape) command: it stores the end address
at `$17F7/$17F8`, opens the start address and types `Q`. The board then
prints the memory as PAP records, which are checked like any tape and
compared with the file:

```
  ✓ verifying $2000-$28FF
✓ verified 2304 bytes, all match
```

If bytes differ, it lists the first few with the expected and the read value.
When all of them read the same value (typically `$FF`), it asks whether
there is RAM at that address. A readback costs about 2.8 characters per
byte, so 2 KB takes roughly a minute at 1200 baud. `Q` always prints whole
24-byte records, so up to 23 bytes past the end of a range are read too.
Because the verify itself uses `$17F7/$17F8`, those two bytes are not
compared.

## Troubleshooting

| Message | What to do |
|---------|------------|
| *the KIM-1 monitor does not answer* | Press **RS** on the board, check the TTY/keypad switch, the cable and power. |
| *the reply looks garbled* | The monitor is calibrated to a different baud rate. Press **RS** and try again. |
| *the board rejected the record (checksum error)* | A bit was lost in transit. Retry with a longer delay, e.g. `-char-delay 10ms`. |
| *answered with a program prompt* | A program (such as Tiny BASIC) is running. Press **RS** to return to the monitor. |
| *… is in use by another program* | Close the serial terminal that has the port open. |
| *N of M bytes differ from the file* | Load again. If the same addresses fail, check that there is (working) RAM there. |

Run with `-v` to see exactly what the board sent back.

## How it works

On the KIM-1 the echo of typed characters is done in hardware: the incoming
serial line is gated straight to the outgoing one. Every byte sent therefore
comes back right away, whatever the CPU is doing. `kimload` waits for each
echo, which paces the transfer and catches a dead or noisy line at once. The
serial port itself is bit-banged by the 6502, so the CPU must be back in
`GETCH` before the next start bit arrives. The configurable delays exist for
that reason.

## Development

```sh
go vet ./...
go test ./...
```

The tests use a simulated KIM-1 monitor, so they need no hardware.

## License

[MIT](LICENSE)
