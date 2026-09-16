package scope

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Signal is what a simulated channel has on its input, described rather than
// captured, so that a test knows exactly what every acquisition holds.
type Signal struct {
	Kind  string  // sine, square, ramp, step, noise or dc
	Amp   float64 // volts, peak
	Freq  float64 // Hz; the step's edge is at 1/Freq
	Ofst  float64 // volts added to the whole signal
	Phase float64 // radians
	Noise float64 // volts, uniform, peak
	Seed  int64
}

// ParseSignal reads a signal as a flag writes it:
// "sine:amp=1,freq=1000,noise=0.01".
func ParseSignal(s string) (Signal, error) {
	kind, rest, _ := strings.Cut(strings.TrimSpace(s), ":")
	sig := Signal{Kind: strings.ToLower(strings.TrimSpace(kind)), Amp: 1, Freq: 1000}
	switch sig.Kind {
	case "sine", "square", "ramp", "step", "noise", "dc":
	default:
		return sig, fmt.Errorf("scope: signal %q: want sine, square, ramp, step, noise or dc", kind)
	}
	for _, kv := range strings.Split(rest, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return sig, fmt.Errorf("scope: signal %q: %q is not key=value", s, kv)
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return sig, fmt.Errorf("scope: signal %q: %q: %w", s, kv, err)
		}
		switch strings.ToLower(k) {
		case "amp":
			sig.Amp = f
		case "freq":
			sig.Freq = f
		case "ofst", "offset":
			sig.Ofst = f
		case "phase":
			sig.Phase = f
		case "noise":
			sig.Noise = f
		case "seed":
			sig.Seed = int64(f)
		default:
			return sig, fmt.Errorf("scope: signal %q: no such key %q", s, k)
		}
	}
	return sig, nil
}

func (s Signal) String() string {
	return fmt.Sprintf("%s:amp=%g,freq=%g,ofst=%g,phase=%g,noise=%g,seed=%d",
		s.Kind, s.Amp, s.Freq, s.Ofst, s.Phase, s.Noise, s.Seed)
}

// Free says whether the signal runs on the scope's own clock rather than being
// aligned to the trigger.
//
// Everything periodic does. The simulator models no trigger comparator, so a
// sine is at a different phase in every frame — which is what a free-running
// trigger really gives, and what lets a test tell two History frames apart. A
// step is the exception: a step response is defined against the trigger, so its
// edge stays at t = 0 whichever frame it is in.
func (s Signal) Free() bool { return s.Kind != "step" }

// At is the signal in volts at t seconds, before the noise, which is added per
// sample so that it stays reproducible from the frame and the sample index.
func (s Signal) At(t float64) float64 {
	ph := 2*math.Pi*s.Freq*t + s.Phase
	switch s.Kind {
	case "sine":
		return s.Ofst + s.Amp*math.Sin(ph)
	case "square":
		if math.Mod(ph/(2*math.Pi)+1e9, 1) < 0.5 {
			return s.Ofst + s.Amp
		}
		return s.Ofst - s.Amp
	case "ramp":
		return s.Ofst + s.Amp*(2*math.Mod(ph/(2*math.Pi)+1e9, 1)-1)
	case "step":
		if t >= 0 {
			return s.Ofst + s.Amp
		}
		return s.Ofst
	case "noise":
		return s.Ofst
	}
	return s.Ofst + s.Amp // dc
}

// SimChannel is one input of a simulated scope: what is on it, and the vertical
// settings it starts with.
type SimChannel struct {
	Signal Signal
	VDiv   float64
	Ofst   float64
	On     bool
}

// Sim is an oscilloscope that exists only as a TCP listener speaking the
// dialect above, so that the driver, the daemon and the checker can be
// exercised with no hardware.
//
// It is deterministic and honest in the ways that matter here:
//
//   - A setting is clamped to the 1-2-5 steps the instrument has, and the
//     transcript says what it holds after, so that what was asked for and what
//     was applied differ exactly where a real scope makes them differ.
//   - The memory depth follows the datasheet, not the programming guide, whose
//     MSIZ entry words the interleaving the other way round: 14 Mpts with one
//     channel on, 7 Mpts each with two. Where the documents disagree the
//     simulator is told which reading to take, so the hardware slice changes a
//     setting rather than a design.
//   - The disputed readings — the sign rule and the trigger delay — are
//     settings, for the same reason.
//   - It keeps Sequence mode and History with their frame times, since those
//     are the scope's only account of when it triggered.
//
// Transcript, if set, receives the simulator's own account of the run: every
// command it was given, what it holds after each setting, and one line per
// acquisition handed over with its settings, its frame time and the sha256 of
// its bytes. Nothing that writes Logb writes it, which is what makes it able to
// falsify a recording rather than agree with it.
type Sim struct {
	Model      string
	Channels   []SimChannel
	Sign       SignRule
	TrigDelay  bool
	Transcript io.Writer

	// TransferRate, in bytes a second, slows a readout down. At 14 Mpts the
	// transfer is the slow part of everything here, and the rate is to be
	// measured on the hardware rather than guessed; this is how the daemon is
	// exercised against a slow one before there is one.
	TransferRate float64

	// Acquire is how long an acquisition takes to complete after ARM, and
	// Interval how far apart Sequence segments are triggered.
	Acquire  time.Duration
	Interval time.Duration

	// Epoch is where the scope's own clock reads zero. What FTIM? counts from
	// is not said by any of the documents, which is exactly why the daemon
	// records the frame time beside a userspace stamp and never in place of one.
	Epoch time.Time

	Now func() time.Time

	mu      sync.Mutex
	started bool
	tdiv    float64
	trdl    float64
	msiz    float64
	mode    string
	chdr    bool
	sparse  int
	seq     int // Sequence segments per arming; 0 is Sequence off
	frames  []simFrame
	frame   int // the History frame selected, from 1
	inr     uint32
	armedAt time.Time
	armed   bool
	acqs    int
	cmr     int
}

// simFrame is one acquired segment: when the scope says it triggered, on its
// own clock.
type simFrame struct {
	at time.Duration
}

// The steps the instrument has. A setting between them is clamped to the
// nearest below, as a knob does.
var (
	vdivSteps = steps125(500e-6, 10)
	tdivSteps = steps125(1e-9, 100)
	// Memory depths, for one channel on and for two. Interleaving follows the
	// datasheet and the user manual, not the guide's MSIZ entry.
	msizOne = []float64{14e3, 140e3, 1.4e6, 14e6}
	msizTwo = []float64{7e3, 70e3, 700e3, 7e6}
)

// MaxSaraOne and MaxSaraTwo are the ADC's rate with one channel on and with
// both: two channels share one 1 GSa/s converter.
const (
	MaxSaraOne = 1e9
	MaxSaraTwo = 500e6
)

func steps125(lo, hi float64) []float64 {
	var out []float64
	for d := math.Pow(10, math.Floor(math.Log10(lo))); d <= hi*1.0000001; d *= 10 {
		for _, m := range []float64{1, 2, 5} {
			v := d * m
			if v >= lo*0.9999999 && v <= hi*1.0000001 {
				out = append(out, v)
			}
		}
	}
	return out
}

// snap is the largest step at or below v, or the smallest step when v is below
// every one of them.
func snap(v float64, steps []float64) float64 {
	best := steps[0]
	for _, s := range steps {
		if s <= v*1.0000001 {
			best = s
		}
	}
	return best
}

func (s *Sim) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sim) init() {
	if s.started {
		return
	}
	s.started = true
	s.tdiv, s.mode, s.chdr, s.sparse, s.frame = 1e-3, "STOP", true, 1, 1
	if s.Acquire <= 0 {
		s.Acquire = 10 * time.Millisecond
	}
	if s.Interval <= 0 {
		s.Interval = time.Millisecond
	}
	if s.Epoch.IsZero() {
		s.Epoch = s.now()
	}
	for i := range s.Channels {
		if s.Channels[i].VDiv <= 0 {
			s.Channels[i].VDiv = 1
		}
	}
	// The depth comes last, because which depths exist depends on how many
	// channels are displayed.
	s.msiz = s.depths()[0]

	// What the instrument holds before anyone has set anything. Without it the
	// transcript would begin mid-run, and a reader of it could not tell a value
	// the simulator started with from one a daemon invented.
	s.note("hold TDIV=%s", num(s.tdiv))
	s.note("hold TRDL=%s", num(s.trdl))
	s.note("hold MSIZ=%s", Points(s.msiz))
	for i, c := range s.Channels {
		name := ChanName(i + 1)
		s.note("hold %s:VDIV=%s", name, num(c.VDiv))
		s.note("hold %s:OFST=%s", name, num(c.Ofst))
		s.note("hold %s:TRA=%s", name, onOff(c.On))
	}
}

// Serve accepts connections until the listener closes. Connections share the
// instrument, as they do on a real one.
func (s *Sim) Serve(l net.Listener) error {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

func (s *Sim) handle(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		for cmd := range strings.SplitSeq(sc.Text(), ";") {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}
			resp, ok := s.Exec(cmd)
			if !ok {
				continue
			}
			if err := s.writeSlowly(c, resp); err != nil {
				return
			}
		}
	}
}

// writeSlowly hands a response over at TransferRate, so that a daemon can be
// exercised against a readout that takes as long as a full-depth one does.
func (s *Sim) writeSlowly(c net.Conn, b []byte) error {
	rate := s.TransferRate
	if rate <= 0 || len(b) < 4096 {
		_, err := c.Write(b)
		return err
	}
	const chunk = 64 << 10
	per := time.Duration(float64(chunk) / rate * float64(time.Second))
	for len(b) > 0 {
		n := min(chunk, len(b))
		if _, err := c.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
		if len(b) > 0 {
			time.Sleep(per)
		}
	}
	return nil
}

// Exec runs one command and returns its answer, with ok false for a command
// that is not a query and so answers nothing.
func (s *Sim) Exec(cmd string) (resp []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.note("cmd %s", cmd)
	s.ripen()

	head, args, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	head = strings.ToUpper(strings.TrimSpace(head))
	args = strings.TrimSpace(args)
	query := strings.HasSuffix(head, "?")
	head = strings.TrimSuffix(head, "?")

	// A channel-scoped command: C1:VDIV.
	ch := 0
	if c, rest, isChan := strings.Cut(head, ":"); isChan && strings.HasPrefix(c, "C") {
		n, err := strconv.Atoi(c[1:])
		if err != nil || n < 1 || n > len(s.Channels) {
			s.fail(2)
			return nil, false
		}
		ch, head = n, rest
	}

	switch head {
	case "*IDN":
		return s.answer("", fmt.Sprintf("ktest,%s,0,0.1", s.Model)), true
	case "*CLS":
		s.cmr = 0
		return nil, false
	case "CMR":
		v := s.cmr
		s.cmr = 0
		return s.answer("CMR", strconv.Itoa(v)), true
	case "CHDR":
		if query {
			return s.answer("CHDR", onOff(s.chdr)), true
		}
		s.chdr = !strings.EqualFold(args, "OFF")
		return nil, false
	}

	if ch > 0 {
		return s.chanCmd(ch, head, args, query)
	}
	return s.instCmd(head, args, query)
}

func (s *Sim) chanCmd(ch int, head, args string, query bool) ([]byte, bool) {
	c := &s.Channels[ch-1]
	name := ChanName(ch)
	switch head {
	case "VDIV":
		if query {
			return s.answer(name+":VDIV", volts(c.VDiv)), true
		}
		v, err := ParseNumber(args)
		if err != nil {
			s.fail(1)
			return nil, false
		}
		c.VDiv = snap(v, vdivSteps)
		if v < vdivSteps[0]*0.9999999 || v > vdivSteps[len(vdivSteps)-1]*1.0000001 {
			s.fail(3)
		}
		s.note("hold %s:VDIV=%s", name, num(c.VDiv))
	case "OFST":
		if query {
			return s.answer(name+":OFST", volts(c.Ofst)), true
		}
		v, err := ParseNumber(args)
		if err != nil {
			s.fail(1)
			return nil, false
		}
		// The offset the instrument can reach is a multiple of the volts a
		// division, which is what makes an offset set before a volts/div land
		// somewhere else than the same offset set after it.
		limit := 100 * c.VDiv
		if math.Abs(v) > limit {
			s.fail(3)
			v = math.Copysign(limit, v)
		}
		c.Ofst = v
		s.note("hold %s:OFST=%s", name, num(c.Ofst))
	case "TRA", "TRACE":
		if query {
			return s.answer(name+":TRA", onOff(c.On)), true
		}
		c.On = !strings.EqualFold(args, "OFF") && args != "0"
		s.clampDepth()
		s.note("hold %s:TRA=%s", name, onOff(c.On))
	case "WF":
		if !query {
			s.fail(1)
			return nil, false
		}
		return s.waveform(ch), true
	default:
		s.fail(1)
		return nil, false
	}
	return nil, false
}

func (s *Sim) instCmd(head, args string, query bool) ([]byte, bool) {
	switch head {
	case "TDIV":
		if query {
			return s.answer("TDIV", seconds(s.tdiv)), true
		}
		v, err := ParseNumber(args)
		if err != nil {
			s.fail(1)
			return nil, false
		}
		s.tdiv = snap(v, tdivSteps)
		s.note("hold TDIV=%s", num(s.tdiv))
	case "TRDL":
		if query {
			return s.answer("TRDL", seconds(s.trdl)), true
		}
		v, err := ParseNumber(args)
		if err != nil {
			s.fail(1)
			return nil, false
		}
		s.trdl = v
		s.note("hold TRDL=%s", num(s.trdl))
	case "SARA":
		return s.answer("SARA", fmt.Sprintf("%.2ESa/s", s.sara())), true
	case "MSIZ":
		if query {
			return s.answer("MSIZ", Points(s.msiz)), true
		}
		v, err := ParseNumber(args)
		if err != nil {
			s.fail(1)
			return nil, false
		}
		s.msiz = snap(v, s.depths())
		s.note("hold MSIZ=%s", Points(s.msiz))
	case "TRMD":
		if query {
			return s.answer("TRMD", s.mode), true
		}
		m := strings.ToUpper(args)
		switch m {
		case "AUTO", "NORM", "NORMAL", "SINGLE", "STOP":
			s.mode = m
			s.note("hold TRMD=%s", m)
		default:
			s.fail(2)
		}
	case "ARM":
		s.arm()
	case "STOP":
		s.mode, s.armed = "STOP", false
		s.note("hold TRMD=STOP")
	case "INR":
		v := s.inr
		s.inr = 0 // reading clears it, as the guide says
		return s.answer("INR", strconv.Itoa(int(v))), true
	case "SEQUENCE":
		if query {
			if s.seq > 0 {
				return s.answer("SEQUENCE", fmt.Sprintf("ON,%d", s.seq)), true
			}
			return s.answer("SEQUENCE", "OFF"), true
		}
		on, count, _ := strings.Cut(args, ",")
		if strings.EqualFold(strings.TrimSpace(on), "OFF") {
			s.seq = 0
		} else if n, err := strconv.Atoi(strings.TrimSpace(count)); err == nil && n > 0 && n <= 80000 {
			s.seq = n
		} else if strings.TrimSpace(count) == "" {
			s.seq = max(s.seq, 1)
		} else {
			s.fail(3)
			return nil, false
		}
		s.note("hold SEQUENCE=%d", s.seq)
	case "FRAM":
		if query {
			return s.answer("FRAM", strconv.Itoa(s.frame)), true
		}
		n, err := strconv.Atoi(args)
		if err != nil || n < 1 {
			s.fail(2)
			return nil, false
		}
		if n > len(s.frames) {
			s.fail(3)
			return nil, false
		}
		s.frame = n
		s.note("hold FRAM=%d", n)
	case "FTIM":
		if len(s.frames) == 0 {
			s.fail(3)
			return nil, false
		}
		return s.answer("FTIM", FormatFrameTime(s.frames[s.frame-1].at)), true
	case "WFSU":
		if query {
			return s.answer("WFSU", fmt.Sprintf("SP,%d,NP,0,FP,0", s.sparse)), true
		}
		f := strings.Split(args, ",")
		for i := 0; i+1 < len(f); i += 2 {
			if strings.EqualFold(strings.TrimSpace(f[i]), "SP") {
				if n, err := strconv.Atoi(strings.TrimSpace(f[i+1])); err == nil && n >= 0 {
					s.sparse = max(1, n)
					s.note("hold WFSU:SP=%d", s.sparse)
				}
			}
		}
	default:
		s.fail(1)
		return nil, false
	}
	return nil, false
}

// arm starts an acquisition. It completes after Acquire, and in Sequence mode
// fills seq segments at Interval, each stamped on the scope's own clock.
func (s *Sim) arm() {
	s.armed, s.armedAt = true, s.now()
	s.frames = nil
	s.frame = 1
	s.note("hold ARM=%s", s.mode)
}

// ripen completes an acquisition whose time has come. Everything the scope does
// on its own happens here, at the moment someone asks it something, which keeps
// the simulator deterministic under a test's own clock.
func (s *Sim) ripen() {
	if !s.armed || s.now().Sub(s.armedAt) < s.Acquire {
		return
	}
	s.armed = false
	n := max(1, s.seq)
	base := s.armedAt.Add(s.Acquire).Sub(s.Epoch)
	for i := 0; i < n; i++ {
		s.frames = append(s.frames, simFrame{at: base + time.Duration(i)*s.Interval})
	}
	s.frame = 1
	s.inr |= InrAcquired
	if s.seq > 0 {
		s.inr |= InrSegment
	}
	if s.mode == "SINGLE" {
		s.mode = "STOP"
	}
}

// depths are the memory depths available, which depend on how many channels are
// displayed: two channels share one converter and halve each other's memory.
func (s *Sim) depths() []float64 {
	if s.on() > 1 {
		return msizTwo
	}
	return msizOne
}

func (s *Sim) on() int {
	n := 0
	for _, c := range s.Channels {
		if c.On {
			n++
		}
	}
	return n
}

// clampDepth brings the memory depth back into the set the current number of
// displayed channels allows, which is what switching a second channel on does
// on the instrument: the two share one memory and each is left with half of
// it, so 14 Mpts becomes 7 and 140 kpts becomes 70.
func (s *Sim) clampDepth() {
	if d := snap(s.msiz, s.depths()); d != s.msiz {
		s.msiz = d
		s.note("hold MSIZ=%s", Points(s.msiz))
	}
}

// sara is the rate the scope actually runs at: as fast as the converter allows,
// unless the screen's fourteen divisions would then need more memory than the
// depth holds, in which case it is the largest 1-2-5 rate that fits.
func (s *Sim) sara() float64 {
	maxRate := MaxSaraOne
	if s.on() > 1 {
		maxRate = MaxSaraTwo
	}
	span := s.tdiv * Divisions
	if span <= 0 {
		return maxRate
	}
	if span*maxRate <= s.msiz {
		return maxRate
	}
	return snap(s.msiz/span, steps125(1, maxRate))
}

// points is how many samples one acquisition holds.
func (s *Sim) points() int {
	n := int(math.Round(s.tdiv * Divisions * s.sara()))
	return max(1, min(n, int(s.msiz)))
}

// waveform builds the selected frame's samples for one channel and answers with
// the block the scope sends: #9, nine digits of length, the bytes, two newlines.
func (s *Sim) waveform(ch int) []byte {
	c := s.Channels[ch-1]
	n := s.points() / s.sparse
	dt := 1 / s.sara()
	first := -s.tdiv * Divisions / 2
	if s.TrigDelay {
		first += s.trdl
	}
	// The frame's trigger on the scope's own clock places the signal, so two
	// frames of the same settings hold different samples and a test can tell
	// which acquisition it is looking at.
	var frameAt time.Duration
	if len(s.frames) > 0 {
		frameAt = s.frames[s.frame-1].at
	}
	rng := rand.New(rand.NewSource(c.Signal.Seed ^ int64(ch)<<32 ^ int64(frameAt)))
	scale := Codes / c.VDiv
	lo, hi := -128, 127
	if s.Sign == SignSub255 {
		lo = -127
	}
	codes := make([]byte, n)
	for i := range codes {
		t := first + float64(i*s.sparse)*dt
		if c.Signal.Free() {
			t += frameAt.Seconds()
		}
		v := c.Signal.At(t)
		if c.Signal.Noise != 0 {
			v += c.Signal.Noise * (2*rng.Float64() - 1)
		}
		code := int(math.Round((v + c.Ofst) * scale))
		code = min(max(code, lo), hi)
		if code < 0 {
			if s.Sign == SignSub255 {
				code += 255
			} else {
				code += 256
			}
		}
		codes[i] = byte(code)
	}

	s.acqs++
	a := &Acquisition{Channel: ch, Codes: codes,
		Chan: ChanSettings{VDiv: c.VDiv, Ofst: c.Ofst, On: c.On},
		Set:  Settings{TDiv: s.tdiv, Trdl: s.trdl, Sara: s.sara(), Msiz: s.msiz, Mode: s.mode},
		Sign: s.Sign, TrigDelay: s.TrigDelay}
	ftim := "none"
	if len(s.frames) > 0 {
		ftim = strings.ReplaceAll(FormatFrameTime(frameAt), " ", "")
	}
	s.note("acq n=%d ch=%d points=%d sparse=%d vdiv=%s ofst=%s tdiv=%s sara=%s trdl=%s msiz=%s "+
		"sign=%s trdelay=%t frame=%d ftim=%s sha256=%s",
		s.acqs, ch, len(codes), s.sparse, num(c.VDiv), num(c.Ofst), num(s.tdiv), num(s.sara()),
		num(s.trdl), Points(s.msiz), s.Sign, s.TrigDelay, s.frame, ftim, a.SHA256())

	out := make([]byte, 0, len(codes)+32)
	if s.chdr {
		out = append(out, (ChanName(ch) + ":WF ALL,")...)
	}
	out = append(out, fmt.Sprintf("#9%09d", len(codes))...)
	out = append(out, codes...)
	return append(out, '\n', '\n')
}

// answer spells a response the way the instrument does, with its header when
// CHDR is on and without it when the driver has asked for it off.
func (s *Sim) answer(head, body string) []byte {
	if s.chdr && head != "" {
		return []byte(head + " " + body + "\n")
	}
	return []byte(body + "\n")
}

// fail sets the command error register, which CMR? reads and clears: 1 an
// unrecognised command, 2 an illegal header or parameter, 3 a parameter out of
// range.
func (s *Sim) fail(code int) { s.cmr = code }

// note writes one line of the simulator's own account of the run.
func (s *Sim) note(format string, a ...any) {
	if s.Transcript == nil {
		return
	}
	fmt.Fprintf(s.Transcript, "%s %s\n", s.now().Format("15:04:05.000000"), fmt.Sprintf(format, a...))
}

func volts(v float64) string   { return fmt.Sprintf("%.2EV", v) }
func seconds(v float64) string { return fmt.Sprintf("%.2ES", v) }
