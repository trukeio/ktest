// Package dut is a simulated device under test: what it does when its supply
// dips, as a state machine with no clock and no bus of its own.
//
// It exists so that the two-rack setup (doc/two-racks-handoff.md) can be run
// and checked with no hardware. cmd/dutsim gives it a CAN interface and a
// supply to read; this package only decides. Time comes in with every event
// and is never read from the system, so that the same history of supply
// readings and reset frames always gives the same frames and the same
// transcript, which is what lets a test hold it to an exact account.
//
// The DUT boots the first time its supply reaches VRestore, talks while it
// runs, and reacts to the supply falling below VMin in the one way its
// Behaviour names. VMin and VRestore are a hysteresis: between them, nothing
// changes. How deep a dip goes does not matter, only that it went below VMin;
// a DUT that tolerates dips by design (Glitch) tolerates them to 0 V. The
// first boot is always clean, whatever the behaviour: a power-on is a reset.
package dut

import (
	_ "embed"
	"fmt"
	"io"
	"strconv"
	"time"
)

// DBC is the database for the frames the simulated DUT sends and the reset it
// listens for. It is the one cmd/dutsim encodes with, and the one to give
// ktestd with -dbc so that a recording decodes them.
//
//go:embed dutsim.dbc
var DBC []byte

// Behaviour is what the DUT does when its supply dips below VMin.
type Behaviour int

const (
	// Glitch: it keeps running, but reports GlitchSpeed instead of Speed while
	// the supply is low, and is back to normal as soon as it is restored. The
	// alive counter does not skip.
	Glitch Behaviour = iota
	// Restore: it stops talking while the supply is low, and boots again once
	// it is restored.
	Restore
	// Reset: it stops talking, and stays silent after the supply is restored
	// until it receives a reset frame.
	Reset
	// Never: it stops talking and does not recover. Only restarting the
	// simulator brings it back.
	Never
)

var behaviours = []string{"glitch", "restore", "reset", "never"}

func (b Behaviour) String() string {
	if b >= 0 && int(b) < len(behaviours) {
		return behaviours[b]
	}
	return "behaviour(" + strconv.Itoa(int(b)) + ")"
}

// ParseBehaviour reads a behaviour by the name String gives it.
func ParseBehaviour(s string) (Behaviour, error) {
	for i, n := range behaviours {
		if s == n {
			return Behaviour(i), nil
		}
	}
	return 0, fmt.Errorf("%q is not a behaviour: it is one of glitch, restore, reset or never", s)
}

// State is what the DUT is doing.
type State int

const (
	Off      State = iota // never powered, or a boot cut short by a dip
	Booting               // powered, silent until the boot time is over
	Running               // talking, reporting Speed
	Glitched              // talking, reporting GlitchSpeed (Glitch, supply low)
	Stopped               // silent until the supply is restored (Restore)
	Hung                  // silent until a reset frame (Reset)
	Dead                  // silent for good (Never)
)

var states = []string{"off", "booting", "running", "glitched", "stopped", "hung", "dead"}

func (s State) String() string {
	if s >= 0 && int(s) < len(states) {
		return states[s]
	}
	return "state(" + strconv.Itoa(int(s)) + ")"
}

// Talking says whether the DUT sends its frames in this state.
func (s State) Talking() bool { return s == Running || s == Glitched }

// Config is the DUT's character. The zero value is not a valid one: New says
// what is missing.
type Config struct {
	Behaviour Behaviour

	// VMin and VRestore, in volts: the supply is low below VMin, and restored
	// at or above VRestore.
	VMin, VRestore float64

	// Boot is how long the DUT is silent after power returns or a reset,
	// before its first frame.
	Boot time.Duration

	// Speed is the engine speed it reports when well, and GlitchSpeed the one
	// it reports while glitched, in rpm.
	Speed, GlitchSpeed float64
}

// Sim is one simulated DUT. It is not safe for concurrent use: cmd/dutsim
// feeds it from one goroutine.
type Sim struct {
	cfg Config
	tr  io.Writer

	state   State
	v       float64 // the supply as last read
	low     bool    // the supply's side of the hysteresis; true until it first reaches VRestore
	bootEnd time.Time
	counter uint8
}

// New checks a configuration and returns a DUT that is off, with an unpowered
// supply. If tr is not nil, the DUT writes its own account of the run to it:
// the configuration, every supply crossing, every reset frame, and every change
// of state, one line each with the time it happened. Nothing that writes Logb
// writes it, which is what lets a check hold a recording to it.
func New(cfg Config, t time.Time, tr io.Writer) (*Sim, error) {
	switch {
	case cfg.Behaviour < Glitch || cfg.Behaviour > Never:
		return nil, fmt.Errorf("%v is not a behaviour", cfg.Behaviour)
	case !(cfg.VMin > 0):
		return nil, fmt.Errorf("vmin is %gV: a DUT with no undervoltage threshold has nothing to react to", cfg.VMin)
	case cfg.VRestore < cfg.VMin:
		return nil, fmt.Errorf("vrestore %gV is below vmin %gV: the supply would be low and restored at once",
			cfg.VRestore, cfg.VMin)
	case cfg.Boot < 0:
		return nil, fmt.Errorf("boot is %v: a boot cannot end before it begins", cfg.Boot)
	case cfg.Speed < 0 || cfg.Speed > 65535 || cfg.GlitchSpeed < 0 || cfg.GlitchSpeed > 65535:
		return nil, fmt.Errorf("speeds are %g and %g rpm: EngineSpeed holds 0 to 65535", cfg.Speed, cfg.GlitchSpeed)
	}
	s := &Sim{cfg: cfg, tr: tr, low: true}
	s.log(t, "config behaviour=%v vmin=%gV vrestore=%gV boot=%v speed=%grpm glitch-speed=%grpm",
		cfg.Behaviour, cfg.VMin, cfg.VRestore, cfg.Boot, cfg.Speed, cfg.GlitchSpeed)
	s.log(t, "state off")
	return s, nil
}

// State is what the DUT is doing now.
func (s *Sim) State() State { return s.state }

// Voltage is the supply as the DUT last read it, which it reports in
// DutSupply while it talks.
func (s *Sim) Voltage() float64 { return s.v }

// Supply is a reading of the DUT's supply voltage, taken at t.
func (s *Sim) Supply(t time.Time, v float64) {
	s.settle(t)
	s.v = v
	switch {
	case !s.low && v < s.cfg.VMin:
		s.low = true
		s.log(t, "supply low %gV", v)
		switch s.state {
		case Booting:
			// A boot the supply cut short is no boot: it starts again when the
			// supply returns, whatever the behaviour, as the first one did.
			s.set(t, Off)
		case Running:
			s.set(t, [...]State{Glitch: Glitched, Restore: Stopped, Reset: Hung, Never: Dead}[s.cfg.Behaviour])
		}
	case s.low && v >= s.cfg.VRestore:
		s.low = false
		s.log(t, "supply ok %gV", v)
		switch s.state {
		case Off, Stopped:
			s.boot(t)
		case Glitched:
			s.set(t, Running)
		}
	}
}

// ResetFrame is a reset frame received at t. It reboots a DUT that is hung, or
// running, provided its supply is not low; anything else it is ignored, and the
// transcript says so.
func (s *Sim) ResetFrame(t time.Time) {
	s.settle(t)
	switch {
	case s.low:
		s.log(t, "reset ignored: supply low")
	case s.state == Hung || s.state.Talking():
		s.log(t, "reset")
		s.boot(t)
	default:
		s.log(t, "reset ignored: %v", s.state)
	}
}

// Tick is a transmit slot at t. If the DUT is talking, it returns the alive
// counter and the speed to send, and ok true; the counter then advances.
func (s *Sim) Tick(t time.Time) (counter uint8, speed float64, ok bool) {
	s.settle(t)
	if !s.state.Talking() {
		return 0, 0, false
	}
	counter, speed = s.counter, s.cfg.Speed
	if s.state == Glitched {
		speed = s.cfg.GlitchSpeed
	}
	s.counter = (s.counter + 1) & 15
	return counter, speed, true
}

// settle ends a boot whose time is up, stamped with the time it ended rather
// than the time it was noticed.
func (s *Sim) settle(t time.Time) {
	if s.state == Booting && !t.Before(s.bootEnd) {
		s.counter = 0
		s.set(s.bootEnd, Running)
	}
}

func (s *Sim) boot(t time.Time) {
	s.bootEnd = t.Add(s.cfg.Boot)
	s.set(t, Booting)
}

func (s *Sim) set(t time.Time, st State) {
	s.state = st
	s.log(t, "state %v", st)
}

// Note writes a line of the caller's into the transcript, in its format: what
// happened to the DUT that the DUT did not decide, such as being stopped.
func (s *Sim) Note(t time.Time, format string, a ...any) { s.log(t, format, a...) }

// TimeFormat is the transcript's time stamp: UTC, to the nanosecond, fixed
// width so that the lines sort as text.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

func (s *Sim) log(t time.Time, format string, a ...any) {
	if s.tr != nil {
		fmt.Fprintf(s.tr, "%s %s\n", t.UTC().Format(TimeFormat), fmt.Sprintf(format, a...))
	}
}
