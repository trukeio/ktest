// Package scope drives a digital storage oscilloscope over SCPI on a raw TCP
// socket, port 5025, and says what it was told and what came back.
//
// It holds no idea of what a setting should be, no limits and no leases, which
// are the daemon's (§7). What it offers is §4's accounts kept apart — a setting
// as sent, the setting the scope reports holding after it, and the samples it
// hands over — so that a scope which clamps a value to the 1-2-5 steps it has
// shows it in the recording instead of being papered over.
//
// # The dialect is a table, and the places it is disputed are settings
//
// The command spellings are a Dialect, as package psu's are. What is different
// here is that the instrument's own documentation disagrees with itself, so
// two readings that change the numbers are settings rather than constants:
//
//   - SignRule: how a code above 127 becomes negative. Siglent's programming
//     guide PG01-E02D subtracts 256; PG01-E02C and its Python example subtract
//     255. FC is -4 under one and -3 under the other, a twenty-fifth of a
//     division on every negative sample, and a reader that picks wrongly says
//     nothing. The hardware settles it against a known voltage (§12, slice 3).
//   - TrigDelay: whether the trigger delay TRDL? shifts the time axis. Both
//     revisions and E02C's Python example leave it out; third-party readers add
//     it. The hardware settles this too.
//
// Neither is given a default that hides the question: Config carries both, the
// daemon records them per acquisition, and the simulator is told which reading
// to take so that the hardware slice changes a setting rather than a design.
//
// # The readout finds #9 and never counts
//
// A waveform answer is a header, then "#9", nine ASCII digits of length, that
// many bytes — the current memory depth, one byte a sample — then 0A 0A. The
// header's length depends on CHDR, and the two guide revisions put the first
// sample at byte 22 in one and 23 in the other. So Read scans for #9 and takes
// the length from the digits after it. Counting bytes into the header is the
// one thing that would make this driver depend on which revision is right.
package scope

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"
)

// A Dialect spells the commands the driver uses. Each is a format whose
// argument 1 is the channel as the scope names it — C1 — and whose argument 2,
// where there is one, is the value.
type Dialect struct {
	Name string

	Identify, Clear, Err string
	HeaderOff            string // ask for answers without the "C1:VDIV " prefix

	SetVDiv, QueryVDiv   string
	SetOfst, QueryOfst   string
	SetTrace, QueryTrace string // a channel displayed, which decides the memory interleave

	SetTDiv, QueryTDiv string
	SetTrdl, QueryTrdl string
	QuerySara          string
	SetMsiz, QueryMsiz string

	SetTrMode, QueryTrMode string
	Arm, Stop              string
	QueryInr               string

	SetWaveSparse string // WFSU SP,%[2]s: every nth point
	Waveform      string // %[1]s:WF? DAT2

	SetSequence    string // sequence mode on with %[2]s segments, or off
	SetFrame       string // History frame %s
	QueryFrame     string
	QueryFrameTime string
}

// Siglent is the SDS1000X-E series' dialect, from programming guide PG01-E02C
// and PG01-E02D. The SDS1202X-E of §12 speaks it.
var Siglent = Dialect{
	Name:      "siglent-sds1000x-e",
	Identify:  "*IDN?",
	Clear:     "*CLS",
	Err:       "CMR?",
	HeaderOff: "CHDR OFF",

	SetVDiv:    "%[1]s:VDIV %[2]s",
	QueryVDiv:  "%[1]s:VDIV?",
	SetOfst:    "%[1]s:OFST %[2]s",
	QueryOfst:  "%[1]s:OFST?",
	SetTrace:   "%[1]s:TRA %[2]s",
	QueryTrace: "%[1]s:TRA?",

	SetTDiv:   "TDIV %s",
	QueryTDiv: "TDIV?",
	SetTrdl:   "TRDL %s",
	QueryTrdl: "TRDL?",
	QuerySara: "SARA?",
	SetMsiz:   "MSIZ %s",
	QueryMsiz: "MSIZ?",

	SetTrMode:   "TRMD %s",
	QueryTrMode: "TRMD?",
	Arm:         "ARM",
	Stop:        "STOP",
	QueryInr:    "INR?",

	SetWaveSparse: "WFSU SP,%s",
	Waveform:      "%[1]s:WF? DAT2",

	SetSequence:    "SEQUENCE %s",
	SetFrame:       "FRAM %s",
	QueryFrame:     "FRAM?",
	QueryFrameTime: "FTIM?",
}

// A SignRule says how an 8-bit code above 127 becomes a negative value. The
// two readings differ by one code everywhere below zero, so the rule is
// recorded with every acquisition rather than assumed.
type SignRule uint8

const (
	// SignSub256 is PG01-E02D: a code is a two's-complement int8. FC is -4.
	SignSub256 SignRule = iota

	// SignSub255 is PG01-E02C and the Python example beside it. FC is -3.
	//
	// It is not a bijection, which is an argument against it that neither
	// document makes: it maps FF and 00 both to zero and reaches only -127, so
	// one of the 256 codes an 8-bit converter can produce means the same as
	// another and a stored volt cannot say which was sent. The simulator never
	// emits FF under this rule. A real instrument that did would settle the
	// question by itself.
	SignSub255
)

func (r SignRule) String() string {
	if r == SignSub255 {
		return "sub255"
	}
	return "sub256"
}

// ParseSignRule reads a rule as String writes it.
func ParseSignRule(s string) (SignRule, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "sub256", "256", "":
		return SignSub256, nil
	case "sub255", "255":
		return SignSub255, nil
	}
	return 0, fmt.Errorf("scope: sign rule %q: want sub256 (PG01-E02D) or sub255 (PG01-E02C)", s)
}

// Signed applies the rule to one code.
func (r SignRule) Signed(code byte) int {
	if code < 128 {
		return int(code)
	}
	if r == SignSub255 {
		return int(code) - 255
	}
	return int(code) - 256
}

// Codes is the number of vertical codes to a division. The scope's whole
// vertical arithmetic rests on it: 25 codes a division, 8 divisions on screen
// and 256 codes in a byte, so the screen is 200 of them and the rest is the
// headroom an offset uses.
const Codes = 25

// Divisions is how many horizontal divisions the screen has, which is what
// turns TDIV into the acquisition's length.
const Divisions = 14

// Settings is what the scope reports holding: the timebase, which is the
// instrument's, and the vertical settings, which are each channel's.
type Settings struct {
	TDiv float64 // seconds a division
	Trdl float64 // trigger delay, seconds; negative delays the trigger
	Sara float64 // samples a second
	Msiz float64 // memory depth in points, as the scope reports it
	Mode string  // TRMD: AUTO, NORM, SINGLE or STOP
}

// ChanSettings is one channel's vertical settings.
type ChanSettings struct {
	VDiv float64 // volts a division
	Ofst float64 // volts; the scope subtracts it, so a positive offset lowers the trace
	On   bool    // the channel is displayed, which decides the memory interleave
}

// An Acquisition is one channel's samples as the scope handed them over, with
// everything needed to place them on both axes and nothing computed away.
//
// Codes are kept as the scope sent them. Volts converts, and the daemon stores
// the volts (§13), but the codes are what the instrument measured and what an
// independent account of a run — the simulator's transcript — is written in.
type Acquisition struct {
	Channel int
	Codes   []byte

	Chan ChanSettings
	Set  Settings
	Sign SignRule
	// TrigDelay says whether Trdl was taken into the time axis. The two guide
	// revisions and the readers that follow them disagree; see the package
	// comment.
	TrigDelay bool

	// Frame and FrameTime are the scope's own account of when it triggered, from
	// History: the segment index and FTIM?, to the microsecond, on the scope's
	// clock. HaveFrameTime is false for a plain single acquisition, which
	// carries no trigger time at all.
	Frame         int
	FrameTime     time.Duration
	HaveFrameTime bool

	// At is the daemon's clock when the readout finished — hundreds of
	// milliseconds after the trigger, by a variable amount. It is recorded as
	// what it is until the clocks are tied (§11, §12 slice 3), never as the
	// trigger instant.
	At time.Time

	// Elapsed is how long the readout itself took, which is what decides how
	// often the rack can redraw and is measured rather than guessed.
	Elapsed time.Duration
}

// Len is how many samples the acquisition holds.
func (a *Acquisition) Len() int { return len(a.Codes) }

// Step is the interval between samples, 1/SARA.
func (a *Acquisition) Step() time.Duration {
	if a.Set.Sara <= 0 {
		return 0
	}
	return time.Duration(math.Round(1e9 / a.Set.Sara))
}

// First is sample 0's position relative to the trigger: half the screen before
// it, and the trigger delay too where the dialect's disputed reading says so.
func (a *Acquisition) First() time.Duration {
	t := -a.Set.TDiv * Divisions / 2
	if a.TrigDelay {
		t += a.Set.Trdl
	}
	return time.Duration(math.Round(t * 1e9))
}

// Duration is how much time the samples cover.
func (a *Acquisition) Duration() time.Duration { return time.Duration(a.Len()) * a.Step() }

// Volt converts one sample to volts: code x (vdiv / 25) - offset.
func (a *Acquisition) Volt(i int) float64 {
	return float64(a.Sign.Signed(a.Codes[i]))*(a.Chan.VDiv/Codes) - a.Chan.Ofst
}

// Volts converts every sample, appending into dst so that a caller can keep one
// buffer for the life of a run rather than allocating a megapoint slice per
// acquisition.
func (a *Acquisition) Volts(dst []float32) []float32 {
	dst = dst[:0]
	if cap(dst) < len(a.Codes) {
		dst = make([]float32, 0, len(a.Codes))
	}
	scale := a.Chan.VDiv / Codes
	for _, c := range a.Codes {
		dst = append(dst, float32(float64(a.Sign.Signed(c))*scale-a.Chan.Ofst))
	}
	return dst
}

// SHA256 is the hash of the bytes the scope handed over, which is how an
// acquisition is identified against an account written by something that never
// saw the recording.
func (a *Acquisition) SHA256() string {
	sum := sha256.Sum256(a.Codes)
	return hex.EncodeToString(sum[:])
}

// ChanName is a channel as the scope names it: C1.
func ChanName(ch int) string { return "C" + strconv.Itoa(ch) }

// A Scope is one connection to one instrument. It is not safe for concurrent
// use: SCPI over a socket is a conversation, and two interleaved ones get each
// other's answers.
type Scope struct {
	d       Dialect
	timeout time.Duration
	conn    net.Conn
	r       *bufio.Reader

	sign      SignRule
	trigDelay bool
}

// Config is what a connection needs beyond the address: the dialect, and the
// two readings the documentation leaves open.
type Config struct {
	Dialect   Dialect
	Timeout   time.Duration
	Sign      SignRule
	TrigDelay bool
}

// ErrNoBlock reports an answer that carried no #9 length block, which is what a
// scope says when it has nothing to hand over or when the query was refused.
var ErrNoBlock = errors.New("scope: the answer carries no #9 length block")

// Dial connects and asks for answers without their header, so that a number is
// a number. It clears the scope's error state, so that what is read later is
// what this connection caused.
func Dial(addr string, cfg Config) (*Scope, error) {
	if cfg.Dialect.Name == "" {
		cfg.Dialect = Siglent
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	conn, err := net.DialTimeout("tcp", addr, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	s := &Scope{d: cfg.Dialect, timeout: cfg.Timeout, conn: conn, r: bufio.NewReaderSize(conn, 1<<16),
		sign: cfg.Sign, trigDelay: cfg.TrigDelay}
	for _, cmd := range []string{cfg.Dialect.Clear, cfg.Dialect.HeaderOff} {
		if cmd == "" {
			continue
		}
		if err := s.send(cmd); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Scope) Close() error { return s.conn.Close() }

// Sign and TrigDelay are the disputed readings this connection takes, for the
// recording to state per acquisition rather than leave to a reader to guess.
func (s *Scope) Sign() SignRule  { return s.sign }
func (s *Scope) TrigDelay() bool { return s.trigDelay }

func (s *Scope) send(format string, a ...any) error {
	s.conn.SetWriteDeadline(time.Now().Add(s.timeout))
	_, err := fmt.Fprintf(s.conn, format+"\n", a...)
	return err
}

func (s *Scope) query(format string, a ...any) (string, error) {
	if err := s.send(format, a...); err != nil {
		return "", err
	}
	s.conn.SetReadDeadline(time.Now().Add(s.timeout))
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// number reads a numeric answer. The scope suffixes its units — 5.00E-01V,
// 1.00E+09Sa/s — and writes a memory depth as 7K or 14M, so the suffix is
// taken off rather than parsed into.
func (s *Scope) number(format string, a ...any) (float64, error) {
	resp, err := s.query(format, a...)
	if err != nil {
		return 0, err
	}
	v, err := ParseNumber(resp)
	if err != nil {
		return 0, fmt.Errorf("scope: %q answered %q: %w", fmt.Sprintf(format, a...), resp, err)
	}
	return v, nil
}

// do, ask and askNumber take a command the dialect spells whole, with no
// channel and no value to put in it. They exist because a command that needs no
// formatting should not go through a format string.
func (s *Scope) do(cmd string) error                   { return s.send("%s", cmd) }
func (s *Scope) ask(cmd string) (string, error)        { return s.query("%s", cmd) }
func (s *Scope) askNumber(cmd string) (float64, error) { return s.number("%s", cmd) }

// ParseNumber reads one of the scope's numbers: an optional header it was asked
// not to send, a number, and a unit or a magnitude suffix.
func ParseNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	// A header the scope sent anyway: "C1:VDIV 5.00E-01V".
	if i := strings.LastIndexByte(s, ' '); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	// The number is the longest prefix that parses; what follows is the unit.
	end := 0
	for end < len(s) {
		c := s[end]
		if c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.' {
			end++
			continue
		}
		if (c == 'e' || c == 'E') && end+1 < len(s) {
			n := s[end+1]
			if n >= '0' && n <= '9' || n == '+' || n == '-' {
				end += 2
				continue
			}
		}
		break
	}
	if end == 0 {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	v, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, err
	}
	switch strings.TrimSpace(s[end:]) {
	case "K", "k":
		v *= 1e3
	case "M":
		v *= 1e6
	case "G":
		v *= 1e9
	}
	return v, nil
}

// num spells a value for a command: shortest exact form, and never an exponent
// where a plain decimal will do.
func num(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Identify is the scope's *IDN? answer, for the recording's provenance (§11).
func (s *Scope) Identify() (string, error) { return s.ask(s.d.Identify) }

func (s *Scope) SetVDiv(ch int, v float64) error {
	return s.send(s.d.SetVDiv, ChanName(ch), num(v))
}
func (s *Scope) SetOfst(ch int, v float64) error {
	return s.send(s.d.SetOfst, ChanName(ch), num(v))
}
func (s *Scope) SetTrace(ch int, on bool) error {
	return s.send(s.d.SetTrace, ChanName(ch), onOff(on))
}
func (s *Scope) SetTDiv(v float64) error { return s.send(s.d.SetTDiv, num(v)) }
func (s *Scope) SetTrdl(v float64) error { return s.send(s.d.SetTrdl, num(v)) }
func (s *Scope) SetMsiz(v float64) error { return s.send(s.d.SetMsiz, Points(v)) }

// Points spells a memory depth the way the scope's MSIZ takes it: 7K, 14M.
func Points(v float64) string {
	switch {
	case v >= 1e6 && math.Mod(v, 1e6) == 0:
		return strconv.FormatFloat(v/1e6, 'f', -1, 64) + "M"
	case v >= 1e3 && math.Mod(v, 1e3) == 0:
		return strconv.FormatFloat(v/1e3, 'f', -1, 64) + "K"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func onOff(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}

// Settings reads the timebase and the acquisition state the scope reports
// holding.
func (s *Scope) Settings() (Settings, error) {
	var st Settings
	var err error
	if st.TDiv, err = s.askNumber(s.d.QueryTDiv); err != nil {
		return st, err
	}
	if st.Trdl, err = s.askNumber(s.d.QueryTrdl); err != nil {
		return st, err
	}
	if st.Sara, err = s.askNumber(s.d.QuerySara); err != nil {
		return st, err
	}
	if st.Msiz, err = s.askNumber(s.d.QueryMsiz); err != nil {
		return st, err
	}
	st.Mode, err = s.ask(s.d.QueryTrMode)
	return st, err
}

// ChanSettings reads one channel's vertical settings.
func (s *Scope) ChanSettings(ch int) (ChanSettings, error) {
	var c ChanSettings
	var err error
	if c.VDiv, err = s.number(s.d.QueryVDiv, ChanName(ch)); err != nil {
		return c, err
	}
	if c.Ofst, err = s.number(s.d.QueryOfst, ChanName(ch)); err != nil {
		return c, err
	}
	resp, err := s.query(s.d.QueryTrace, ChanName(ch))
	if err != nil {
		return c, err
	}
	c.On = strings.EqualFold(strings.TrimSpace(resp), "ON") || strings.TrimSpace(resp) == "1"
	return c, nil
}

// Single arms one acquisition: TRMD SINGLE, then ARM.
func (s *Scope) Single() error {
	if err := s.send(s.d.SetTrMode, "SINGLE"); err != nil {
		return err
	}
	return s.do(s.d.Arm)
}

// Stop ends an acquisition in progress. Stopping a scope harms nothing, which
// is why a disarm leaves it where it is and does not switch anything off: a
// stop is for outputs (§7).
func (s *Scope) Stop() error { return s.do(s.d.Stop) }

// Inr reads the internal status register, which reading clears. Bit 0 says an
// acquisition has completed, bit 4 that a Sequence segment has, bit 13 that the
// trigger is ready.
func (s *Scope) Inr() (uint32, error) {
	v, err := s.askNumber(s.d.QueryInr)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// INR bits, as the guide numbers them.
const (
	InrAcquired = 1 << 0
	InrSegment  = 1 << 4
	InrReady    = 1 << 13
)

// Sequence turns Sequence mode on with n segments, or off with n of 0. In
// Sequence mode the scope fills a segment per trigger with no readout between
// them and stamps each, which is its only account of when it triggered.
func (s *Scope) Sequence(n int) error {
	if n <= 0 {
		return s.send(s.d.SetSequence, "OFF")
	}
	if err := s.send(s.d.SetSequence, "ON"); err != nil {
		return err
	}
	return s.send("SEQUENCE ON,%d", n)
}

// SelectFrame selects a History frame, from 1.
func (s *Scope) SelectFrame(n int) error { return s.send(s.d.SetFrame, strconv.Itoa(n)) }

// Frame is the History frame the scope has selected, which is what a readout
// returns. It is 1 outside Sequence mode, and asking rather than assuming is
// what keeps an acquisition's record true when someone has been at the front
// panel.
func (s *Scope) Frame() (int, error) {
	v, err := s.askNumber(s.d.QueryFrame)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

// FrameTime is the selected frame's acquire time on the scope's own clock,
// which is not the daemon's and whose origin the guide does not say. It is
// recorded beside a userspace stamp and never in place of one (§11).
func (s *Scope) FrameTime() (time.Duration, error) {
	resp, err := s.ask(s.d.QueryFrameTime)
	if err != nil {
		return 0, err
	}
	return ParseFrameTime(resp)
}

// ParseFrameTime reads FTIM?'s answer, which the scope writes with spaces
// inside it: "00: 05: 12. 650814".
func ParseFrameTime(s string) (time.Duration, error) {
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if i := strings.LastIndexByte(clean, ' '); i >= 0 {
		clean = clean[i+1:]
	}
	parts := strings.Split(clean, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("scope: frame time %q: want hh:mm:ss.uuuuuu", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("scope: frame time %q: %w", s, err)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("scope: frame time %q: %w", s, err)
	}
	sec, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return 0, fmt.Errorf("scope: frame time %q: %w", s, err)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(math.Round(sec*1e6))*time.Microsecond, nil
}

// FormatFrameTime writes a frame time as the scope does, spaces and all.
func FormatFrameTime(d time.Duration) string {
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	us := int64(d/time.Microsecond) % 60_000_000
	return fmt.Sprintf("%02d: %02d: %02d. %06d", h, m, us/1e6, us%1e6)
}

// Sparse asks for every nth point. It is a preview, never a reduction: every
// nth point is the aliasing and the lost glitch an envelope exists to prevent
// (§10.4), so the daemon leaves it at 1 and decimates what it read.
func (s *Scope) Sparse(n int) error { return s.send(s.d.SetWaveSparse, strconv.Itoa(n)) }

// Read reads one channel's samples, with the settings in force around them so
// that the acquisition carries its own axis rather than borrowing whatever the
// scope holds by the time someone asks.
//
// The settings are read before the samples and the channel's after them, which
// is the order that makes a change during the readout show up as a mismatch a
// caller can see rather than as an axis quietly taken from the wrong timebase.
func (s *Scope) Read(ch int, now func() time.Time) (*Acquisition, error) {
	if now == nil {
		now = time.Now
	}
	start := now()
	set, err := s.Settings()
	if err != nil {
		return nil, err
	}
	cs, err := s.ChanSettings(ch)
	if err != nil {
		return nil, err
	}
	if err := s.send(s.d.Waveform, ChanName(ch)); err != nil {
		return nil, err
	}
	codes, err := s.readBlock()
	if err != nil {
		return nil, err
	}
	after, err := s.ChanSettings(ch)
	if err != nil {
		return nil, err
	}
	if after != cs {
		return nil, fmt.Errorf("scope: %s changed under the readout: vdiv %g->%g, offset %g->%g",
			ChanName(ch), cs.VDiv, after.VDiv, cs.Ofst, after.Ofst)
	}
	end := now()
	return &Acquisition{Channel: ch, Codes: codes, Chan: cs, Set: set,
		Sign: s.sign, TrigDelay: s.trigDelay, At: end, Elapsed: end.Sub(start)}, nil
}

// readBlock scans for #9, takes the nine digits after it as the length, and
// reads that many bytes and the two newlines that close them.
func (s *Scope) readBlock() ([]byte, error) {
	s.conn.SetReadDeadline(time.Now().Add(s.timeout))
	// The header is short and bounded; a long run without #9 means the answer
	// is not a waveform, and saying so beats reading a socket for ever.
	const maxHeader = 256
	var prev byte
	for i := 0; ; i++ {
		if i > maxHeader {
			return nil, ErrNoBlock
		}
		c, err := s.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if prev == '#' && c == '9' {
			break
		}
		prev = c
	}
	var digits [9]byte
	if _, err := io.ReadFull(s.r, digits[:]); err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(string(digits[:]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("scope: #9 followed by %q, which is not a length", digits)
	}
	if n > MaxPoints {
		return nil, fmt.Errorf("scope: a block of %d bytes, past the %d this driver will read", n, MaxPoints)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return nil, err
	}
	// The two newlines that close the block. A scope that sends something else
	// has left the conversation out of step, and the next query would read this
	// instead of its own answer.
	var tail [2]byte
	if _, err := io.ReadFull(s.r, tail[:]); err != nil {
		return nil, err
	}
	if tail != [2]byte{'\n', '\n'} {
		return nil, fmt.Errorf("scope: a block of %d bytes closed with %q, not two newlines", n, tail)
	}
	return buf, nil
}

// MaxPoints bounds one readout. The SDS1202X-E holds 14 Mpts with one channel
// on; this leaves room for a deeper instrument and still refuses a length that
// is a misread header rather than a memory depth.
const MaxPoints = 64 << 20
