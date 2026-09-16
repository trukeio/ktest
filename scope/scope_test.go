package scope

import (
	"bytes"
	"math"
	"net"
	"strings"
	"testing"
	"time"
)

// dial starts a simulator on a loopback port and connects a driver to it. The
// driver and the simulator are the two halves this milestone is built from, so
// almost every test here is one talking to the other.
func dial(t *testing.T, sim *Sim, cfg Config) *Scope {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go sim.Serve(l)
	s, err := Dial(l.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func oneChannel(sig Signal) *Sim {
	return &Sim{Model: "scopesim", Acquire: time.Millisecond,
		Channels: []SimChannel{{Signal: sig, VDiv: 1, On: true}}}
}

// acquire runs one acquisition end to end, as the daemon does: arm, wait for
// INR bit 0, read.
func acquire(t *testing.T, s *Scope, ch int) *Acquisition {
	t.Helper()
	if err := s.Single(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := s.Inr()
		if err != nil {
			t.Fatal(err)
		}
		if v&InrAcquired != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the simulator never reported an acquisition")
		}
		time.Sleep(time.Millisecond)
	}
	a, err := s.Read(ch, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The readout finds #9 and never counts, which is the one thing that would
// otherwise depend on which guide revision is right: CHDR ON puts a header in
// front of the block and CHDR OFF does not, and the driver must read the same
// samples either way.
func TestReadFindsBlockWhateverTheHeader(t *testing.T) {
	for _, chdr := range []bool{false, true} {
		sim := oneChannel(Signal{Kind: "dc", Amp: 0.5})
		s := dial(t, sim, Config{})
		if chdr {
			// Undo the CHDR OFF that Dial sends, so the answer carries the
			// header a scope sends by default.
			if err := s.send("CHDR ON"); err != nil {
				t.Fatal(err)
			}
		}
		a := acquire(t, s, 1)
		if a.Len() == 0 {
			t.Fatalf("chdr %v: no samples", chdr)
		}
		if got := a.Volt(0); math.Abs(got-0.5) > 0.05 {
			t.Errorf("chdr %v: sample 0 is %g V, want 0.5", chdr, got)
		}
	}
}

// The sign rule is the place the two guide revisions part, and a reader that
// picks wrongly is out by one code on every negative sample and never says so.
// So it is a setting, and the two readings must give different answers on the
// same byte.
func TestSignRuleDiffersByOneCode(t *testing.T) {
	if got, want := SignSub256.Signed(0xFC), -4; got != want {
		t.Errorf("sub256: FC is %d, want %d", got, want)
	}
	if got, want := SignSub255.Signed(0xFC), -3; got != want {
		t.Errorf("sub255: FC is %d, want %d", got, want)
	}
	for _, c := range []byte{0, 1, 0x7F} {
		if SignSub256.Signed(c) != SignSub255.Signed(c) {
			t.Errorf("code %02X: the rules differ above zero, and must not", c)
		}
	}
	// A twenty-fifth of a division, on a 1 V/div channel: 40 mV.
	a := &Acquisition{Codes: []byte{0xFC}, Chan: ChanSettings{VDiv: 1}, Sign: SignSub256}
	b := &Acquisition{Codes: []byte{0xFC}, Chan: ChanSettings{VDiv: 1}, Sign: SignSub255}
	if got := b.Volt(0) - a.Volt(0); math.Abs(got-1.0/Codes) > 1e-12 {
		t.Errorf("the rules differ by %g V, want %g", got, 1.0/Codes)
	}
}

// Sample i is at -(tdiv x 14 / 2) + i/sara, and whether the trigger delay
// shifts it is the dialect's other disputed reading.
func TestTimeAxis(t *testing.T) {
	a := &Acquisition{Set: Settings{TDiv: 1e-3, Sara: 1e6, Trdl: 2e-3}}
	if got, want := a.First(), -7*time.Millisecond; got != want {
		t.Errorf("first sample at %v, want %v", got, want)
	}
	if got, want := a.Step(), time.Microsecond; got != want {
		t.Errorf("step %v, want %v", got, want)
	}
	a.TrigDelay = true
	if got, want := a.First(), -5*time.Millisecond; got != want {
		t.Errorf("with the trigger delay: first sample at %v, want %v", got, want)
	}
}

// A setting is clamped to the steps the instrument has, and what it holds after
// is what the recording must carry: asking for 0.3 V/div gets 0.2, and a
// daemon that recorded 0.3 would be recording the request as the fact (§4).
func TestSettingsAreClampedAndReadBack(t *testing.T) {
	var tr bytes.Buffer
	sim := oneChannel(Signal{Kind: "sine", Amp: 1, Freq: 1000})
	sim.Transcript = &tr
	s := dial(t, sim, Config{})
	if err := s.SetVDiv(1, 0.3); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTDiv(3e-4); err != nil {
		t.Fatal(err)
	}
	cs, err := s.ChanSettings(1)
	if err != nil {
		t.Fatal(err)
	}
	if cs.VDiv != 0.2 {
		t.Errorf("vdiv %g, want 0.2", cs.VDiv)
	}
	set, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if set.TDiv != 2e-4 {
		t.Errorf("tdiv %g, want 2e-4", set.TDiv)
	}
	if !strings.Contains(tr.String(), "hold C1:VDIV=0.2") {
		t.Errorf("the transcript does not say what the scope holds:\n%s", tr.String())
	}
}

// The memory depth follows the datasheet: 14 Mpts with one channel on, 7 Mpts
// each with two, and switching the second one on brings the depth back into
// range rather than leaving a value the instrument cannot hold.
func TestMemoryDepthInterleave(t *testing.T) {
	sim := &Sim{Model: "scopesim", Acquire: time.Millisecond, Channels: []SimChannel{
		{Signal: Signal{Kind: "dc"}, VDiv: 1, On: true},
		{Signal: Signal{Kind: "dc"}, VDiv: 1},
	}}
	s := dial(t, sim, Config{})
	if err := s.SetMsiz(14e6); err != nil {
		t.Fatal(err)
	}
	set, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if set.Msiz != 14e6 {
		t.Fatalf("one channel on: msiz %g, want 1.4e7", set.Msiz)
	}
	if err := s.SetTrace(2, true); err != nil {
		t.Fatal(err)
	}
	if set, err = s.Settings(); err != nil {
		t.Fatal(err)
	}
	if set.Msiz != 7e6 {
		t.Errorf("two channels on: msiz %g, want 7e6", set.Msiz)
	}
	if set.Sara > MaxSaraTwo {
		t.Errorf("two channels on: sara %g, want at most %g", set.Sara, MaxSaraTwo)
	}
}

// Sequence mode and History are the scope's only account of when it triggered,
// so the frames it filled must come back with times that differ, to the
// microsecond, on its own clock.
func TestSequenceFramesAreStamped(t *testing.T) {
	// The frequency is deliberately not commensurate with the frame interval:
	// the simulator models no trigger comparator, so its signal runs on the
	// scope's clock, and at 1 kHz two frames 4 ms apart really do hold the same
	// bytes. That is the simulator being right and the test being badly chosen.
	sim := oneChannel(Signal{Kind: "sine", Amp: 1, Freq: 333})
	sim.Interval = 2 * time.Millisecond
	s := dial(t, sim, Config{})
	if err := s.Sequence(4); err != nil {
		t.Fatal(err)
	}
	acquire(t, s, 1)
	var times []time.Duration
	for f := 1; f <= 4; f++ {
		if err := s.SelectFrame(f); err != nil {
			t.Fatal(err)
		}
		d, err := s.FrameTime()
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, d)
	}
	for i := 1; i < len(times); i++ {
		if got := times[i] - times[i-1]; got != 2*time.Millisecond {
			t.Errorf("frame %d is %v after frame %d, want 2ms", i+1, got, i)
		}
	}
	// A frame carries its own samples: two frames of one sine taken 2 ms apart
	// are not the same bytes.
	if err := s.SelectFrame(1); err != nil {
		t.Fatal(err)
	}
	a, err := s.Read(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SelectFrame(3); err != nil {
		t.Fatal(err)
	}
	b, err := s.Read(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256() == b.SHA256() {
		t.Error("two History frames of a sine hold the same bytes")
	}
}

// FTIM? is written with spaces inside it, which is the sort of thing a reader
// written from an example rather than from the answer gets wrong.
func TestFrameTimeRoundTrip(t *testing.T) {
	d, err := ParseFrameTime("00: 05: 12. 650814")
	if err != nil {
		t.Fatal(err)
	}
	want := 5*time.Minute + 12*time.Second + 650814*time.Microsecond
	if d != want {
		t.Fatalf("parsed %v, want %v", d, want)
	}
	if got := FormatFrameTime(d); got != "00: 05: 12. 650814" {
		t.Fatalf("formatted %q", got)
	}
	// And without them, since the two guide revisions do not agree about those
	// either.
	if d2, err := ParseFrameTime("00:05:12.650814"); err != nil || d2 != want {
		t.Fatalf("without spaces: %v, %v", d2, err)
	}
}

// The scope suffixes its units and writes a memory depth as 7K or 14M.
func TestParseNumber(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"5.00E-01V", 0.5},
		{"C1:VDIV 5.00E-01V", 0.5},
		{"1.00E+09Sa/s", 1e9},
		{"-2.5E-03S", -2.5e-3},
		{"7K", 7000},
		{"14M", 14e6},
		{"700", 700},
	} {
		got, err := ParseNumber(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: %g, want %g", c.in, got, c.want)
		}
	}
	if _, err := ParseNumber("ON"); err == nil {
		t.Error("ON parsed as a number")
	}
}

// The simulator's samples are the signal it was given, so a test knows what an
// acquisition holds without having to trust the thing that wrote it.
func TestSimulatedSignalIsWhatWasAskedFor(t *testing.T) {
	sim := oneChannel(Signal{Kind: "sine", Amp: 2, Freq: 100})
	sim.Channels[0].VDiv = 1
	s := dial(t, sim, Config{})
	if err := s.SetTDiv(1e-3); err != nil { // 14 ms of a 100 Hz sine
		t.Fatal(err)
	}
	a := acquire(t, s, 1)
	// The signal is free-running on the scope's clock, so where the frame
	// triggered is part of knowing what it holds.
	ft, err := s.FrameTime()
	if err != nil {
		t.Fatal(err)
	}
	dt := a.Step().Seconds()
	first := a.First().Seconds() + ft.Seconds()
	worst := 0.0
	for i := 0; i < a.Len(); i++ {
		want := 2 * math.Sin(2*math.Pi*100*(first+float64(i)*dt))
		worst = math.Max(worst, math.Abs(a.Volt(i)-want))
	}
	// One code is vdiv/25 = 40 mV, and quantisation costs half of that.
	if worst > 1.0/Codes {
		t.Errorf("worst sample is %g V from the signal, want under %g", worst, 1.0/Codes)
	}
}

// A slow transfer is the case the hardware will be, and a driver that only
// works when the whole block arrives in one read would pass every other test
// here and fail on the bench.
func TestSlowTransfer(t *testing.T) {
	sim := oneChannel(Signal{Kind: "ramp", Amp: 1, Freq: 50})
	sim.TransferRate = 4 << 20 // 4 MB/s
	s := dial(t, sim, Config{})
	if err := s.SetTDiv(1e-4); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMsiz(140e3); err != nil {
		t.Fatal(err)
	}
	a := acquire(t, s, 1)
	if a.Len() < 1000 {
		t.Fatalf("%d samples; the test needs a block big enough to arrive in pieces", a.Len())
	}
	if a.Elapsed <= 0 {
		t.Error("the readout reported no elapsed time")
	}
}

// Volts converts every sample the same way Volt converts one, and reuses the
// caller's buffer: a megapoint acquisition per trigger must not allocate one.
func TestVoltsMatchesVolt(t *testing.T) {
	a := &Acquisition{Codes: []byte{0, 1, 0x7F, 0x80, 0xFC, 0xFF},
		Chan: ChanSettings{VDiv: 0.5, Ofst: 0.1}, Sign: SignSub256}
	buf := make([]float32, 0, 6)
	got := a.Volts(buf)
	for i := range a.Codes {
		if float64(got[i]) != float64(float32(a.Volt(i))) {
			t.Errorf("sample %d: %g against %g", i, got[i], a.Volt(i))
		}
	}
	if &got[0] != &buf[:1][0] {
		t.Error("Volts allocated rather than filling the buffer it was given")
	}
}
