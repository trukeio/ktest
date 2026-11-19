package dut

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

func cfg(b Behaviour) Config {
	return Config{Behaviour: b, VMin: 9, VRestore: 9.5, Boot: 50 * time.Millisecond, Speed: 3000, GlitchSpeed: 0}
}

// An event in a scripted run: a supply reading, a reset frame, or a tick.
type ev struct {
	ms    int
	v     float64 // supply reading, when kind is 'v'
	kind  byte    // 'v', 'r' (reset frame), 't' (tick)
	state State   // the state expected after it
}

func run(t *testing.T, c Config, evs []ev) (*Sim, string) {
	t.Helper()
	var tr strings.Builder
	s, err := New(c, t0, &tr)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range evs {
		switch e.kind {
		case 'v':
			s.Supply(at(e.ms), e.v)
		case 'r':
			s.ResetFrame(at(e.ms))
		case 't':
			s.Tick(at(e.ms))
		}
		if s.State() != e.state {
			t.Fatalf("event %d (%c at %dms): state %v, want %v\n%s", i, e.kind, e.ms, s.State(), e.state, tr.String())
		}
	}
	return s, tr.String()
}

// The dip every behaviour is put through: boot at 12 V, run, dip to 6 V,
// restore to 12 V, reset, and the states each behaviour should pass through.
func TestBehaviours(t *testing.T) {
	type want struct{ dipped, restored, afterReset State }
	cases := map[Behaviour]want{
		Glitch:  {Glitched, Running, Booting},
		Restore: {Stopped, Booting, Booting},
		Reset:   {Hung, Hung, Booting},
		Never:   {Dead, Dead, Dead},
	}
	for b, w := range cases {
		t.Run(b.String(), func(t *testing.T) {
			run(t, cfg(b), []ev{
				{0, 0, 't', Off},
				{10, 12, 'v', Booting},
				{30, 0, 't', Booting},
				{60, 0, 't', Running},
				{100, 6, 'v', w.dipped},
				{200, 12, 'v', w.restored},
				// Restore's boot ends at 250 ms; a reset before it restarts it.
				{210, 0, 'r', w.afterReset},
			})
		})
	}
}

func TestHysteresis(t *testing.T) {
	run(t, cfg(Restore), []ev{
		{0, 9.4, 'v', Off}, // above vmin, below vrestore: not yet powered
		{10, 9.5, 'v', Booting},
		{100, 0, 't', Running},
		{110, 9.2, 'v', Running}, // inside the band: still running
		{120, 8.99, 'v', Stopped},
		{130, 9.49, 'v', Stopped}, // inside the band: still stopped
		{140, 9.5, 'v', Booting},
	})
}

// A dip during a boot cuts it short, for every behaviour, Never included: the
// DUT was not yet running, so there was nothing for it to fail at.
func TestDipDuringBoot(t *testing.T) {
	for _, b := range []Behaviour{Glitch, Restore, Reset, Never} {
		t.Run(b.String(), func(t *testing.T) {
			run(t, cfg(b), []ev{
				{0, 12, 'v', Booting},
				{20, 5, 'v', Off},
				{30, 12, 'v', Booting},
				{80, 0, 't', Running},
			})
		})
	}
}

func TestResetIgnored(t *testing.T) {
	_, tr := run(t, cfg(Reset), []ev{
		{0, 0, 'r', Off}, // unpowered: the supply is low
		{10, 12, 'v', Booting},
		{70, 0, 't', Running},
		{80, 5, 'v', Hung},
		{90, 0, 'r', Hung}, // supply still low
		{100, 12, 'v', Hung},
		{110, 0, 'r', Booting},
		{200, 0, 't', Running},
	})
	for _, line := range []string{
		"10:00:00.000000000Z reset ignored: supply low",
		"10:00:00.090000000Z reset ignored: supply low",
		"10:00:00.110000000Z reset",
		"10:00:00.160000000Z state running", // the boot's end, not the tick that noticed it
	} {
		if !strings.Contains(tr, line+"\n") {
			t.Errorf("transcript lacks %q:\n%s", line, tr)
		}
	}
}

// The counter runs modulo 16 while talking, does not skip through a glitch,
// and restarts at 0 on a reboot. Speed follows the glitch.
func TestFrames(t *testing.T) {
	s, err := New(cfg(Glitch), t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Supply(at(0), 12)
	if _, _, ok := s.Tick(at(10)); ok {
		t.Fatal("talking while booting")
	}
	ms := 50
	tick := func() (uint8, float64) {
		t.Helper()
		c, v, ok := s.Tick(at(ms))
		if !ok {
			t.Fatalf("silent at %dms in %v", ms, s.State())
		}
		ms += 10
		return c, v
	}
	for i := range 20 {
		if c, v := tick(); c != uint8(i%16) || v != 3000 {
			t.Fatalf("tick %d: counter %d speed %g", i, c, v)
		}
	}
	s.Supply(at(ms), 5)
	if c, v := tick(); c != 20%16 || v != 0 {
		t.Fatalf("glitched: counter %d speed %g, want 4 and 0", c, v)
	}
	s.Supply(at(ms), 12)
	if c, v := tick(); c != 21%16 || v != 3000 {
		t.Fatalf("restored: counter %d speed %g, want 5 and 3000", c, v)
	}
	s.ResetFrame(at(ms))
	ms += 50
	if c, _ := tick(); c != 0 {
		t.Fatalf("after a reset the counter is %d, want 0", c)
	}
}

// Purity: the same history gives the same transcript, byte for byte.
func TestDeterministic(t *testing.T) {
	script := []ev{
		{0, 12, 'v', Booting}, {60, 0, 't', Running}, {70, 3, 'v', Hung},
		{80, 12, 'v', Hung}, {90, 0, 'r', Booting}, {150, 0, 't', Running},
	}
	_, a := run(t, cfg(Reset), script)
	_, b := run(t, cfg(Reset), script)
	if a != b {
		t.Fatalf("two runs of one history differ:\n%s\n---\n%s", a, b)
	}
	if !strings.HasPrefix(a, "2026-11-19T10:00:00.000000000Z config behaviour=reset vmin=9V vrestore=9.5V boot=50ms") {
		t.Fatalf("transcript does not open with the configuration:\n%s", a)
	}
}

func TestRefused(t *testing.T) {
	for _, c := range []struct {
		mutate func(*Config)
		want   string
	}{
		{func(c *Config) { c.VMin = 0 }, "no undervoltage threshold"},
		{func(c *Config) { c.VRestore = 8 }, "below vmin"},
		{func(c *Config) { c.Boot = -1 }, "cannot end before it begins"},
		{func(c *Config) { c.Speed = 70000 }, "holds 0 to 65535"},
		{func(c *Config) { c.Behaviour = 7 }, "is not a behaviour"},
	} {
		x := cfg(Restore)
		c.mutate(&x)
		if _, err := New(x, t0, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("got %v, want an error saying %q", err, c.want)
		}
	}
	if _, err := ParseBehaviour("class3"); err == nil {
		t.Error("an unknown behaviour was accepted")
	}
}

func TestCodec(t *testing.T) {
	c, err := NewCodec()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.Alive(13); err != nil || len(p) != 1 || p[0] != 13 {
		t.Errorf("alive 13: % x, %v", p, err)
	}
	if p, err := c.Speed(3000); err != nil || len(p) != 2 || p[0] != 0xb8 || p[1] != 0x0b {
		t.Errorf("speed 3000: % x, %v", p, err)
	}
	if !c.IsReset(0x700, false, []byte{1}) {
		t.Error("DutReset with Request = 1 is not a reset")
	}
	for _, f := range []struct {
		id  uint32
		ext bool
		p   []byte
	}{{0x700, false, []byte{2}}, {0x700, true, []byte{1}}, {0x701, false, []byte{1}}, {0x700, false, []byte{1, 0}}} {
		if c.IsReset(f.id, f.ext, f.p) {
			t.Errorf("%#x ext=%v % x taken for a reset", f.id, f.ext, f.p)
		}
	}
}
