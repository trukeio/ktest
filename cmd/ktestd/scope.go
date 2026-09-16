//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rveen/logb"

	"github.com/rveen/ktest/record"
	dso "github.com/rveen/ktest/scope"
)

// A scopeDriver is one oscilloscope as the daemon drives it: a goroutine that
// owns the connection, as a supply's does, so that a readout of tens of
// megabytes can never hold up the loop that records the bus.
//
// The loop it feeds decides what may be done (§7) and records what was done;
// this goroutine arms, waits, reads and says what came back. Acquisition itself
// is not gated by arming: a scope is an input, and reading one harms nothing.
// Arming gates its settings, which are writes like any other (§4), and the stop
// leaves the instrument exactly as it is — stopping an acquisition is not the
// kind of thing a stop exists for.
type scopeDriver struct {
	name     string
	addr     string
	channels int
	cfg      dso.Config

	// poll is how often the goroutine looks at the instrument: often enough
	// that the gap between an acquisition completing and its readout starting
	// is small, and not so often that it is all the scope answers.
	poll time.Duration

	cmds   chan scopeCmd
	events chan<- scopeEvent
}

// A scopeSpec is one -scope flag: name=host:port[,channels][,full=ch1+ch2].
type scopeSpec struct {
	name     string
	addr     string
	channels int
	// full names the channels whose samples go into the recording at full rate.
	// Everything else gets the envelope only, which is §10.4's default the
	// right way round: continuous full-rate capture is a decision someone made,
	// not a surprise disk fill during an overnight run.
	full map[int]bool
}

type scopeFlags []scopeSpec

func (f *scopeFlags) String() string {
	var s []string
	for _, x := range *f {
		s = append(s, fmt.Sprintf("%s=%s,%d", x.name, x.addr, x.channels))
	}
	return strings.Join(s, " ")
}

func (f *scopeFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return errors.New(`want name=host:port[,channels][,full=ch1+ch2], e.g. scope1=192.168.1.30:5025,2,full=ch1`)
	}
	if err := instrumentName("scope", name); err != nil {
		return err
	}
	spec := scopeSpec{name: name, channels: 2, full: map[int]bool{}}
	parts := strings.Split(rest, ",")
	spec.addr = parts[0]
	for _, p := range parts[1:] {
		switch key, value, hasValue := strings.Cut(p, "="); {
		case hasValue && key == "full":
			for _, c := range strings.Split(value, "+") {
				n, err := strconv.Atoi(strings.TrimPrefix(c, "ch"))
				if err != nil || n < 1 || n > 16 {
					return fmt.Errorf("scope %s: full=%q; name channels as ch1+ch2", name, value)
				}
				spec.full[n] = true
			}
		case hasValue:
			return fmt.Errorf("scope %s: no such option %q", name, key)
		default:
			c, err := strconv.Atoi(p)
			if err != nil || c < 1 || c > 16 {
				return fmt.Errorf("scope %s: %q channels; a scope has 1 to 16", name, p)
			}
			spec.channels = c
		}
	}
	for n := range spec.full {
		if n > spec.channels {
			return fmt.Errorf("scope %s: full=ch%d, and it has %d channels", name, n, spec.channels)
		}
	}
	for _, x := range *f {
		if x.name == name {
			return fmt.Errorf("scope %s is named twice", name)
		}
	}
	*f = append(*f, spec)
	return nil
}

// instrumentName checks a name against §4: it begins every channel's name, so
// it may not contain the dot that separates the parts of one, and it must fit
// the recording.
func instrumentName(kind, name string) error {
	if len(name) > 16 || strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return fmt.Errorf("%s name %q: up to 16 letters, digits, '-' or '_'", kind, name)
	}
	return nil
}

type scopeCmd struct {
	part  string // "ch1.vert" or "timebase"
	what  string // the quantity: vdiv, ofst, on, tdiv, trdl, msiz
	value float64
	seq   uint32
	reply chan reply
}

type scopeEventKind int

const (
	scConnected scopeEventKind = iota
	scDisconnected
	scVert      // one channel's vertical settings, as read back
	scTimebase  // the timebase and depth, as read back
	scAcquired  // one channel's samples
	scAcqFailed // a readout that did not finish
)

// vertState and timeState are what the scope reports holding.
type vertState struct {
	vdiv, ofst float64
	on         bool
}

type timeState struct {
	tdiv, trdl, msiz, sara float64
	mode                   string
}

type scopeEvent struct {
	s    *scopeDriver
	kind scopeEventKind
	t    time.Time
	ch   int
	seq  uint32
	why  uint8

	vert vertState
	tb   timeState

	// run is the trigger this acquisition belongs to: every channel read out of
	// one arming carries the same number, because they are one acquisition.
	run  uint32
	last bool // the last channel of this trigger, so the loop can close the run
	acq  *dso.Acquisition

	// missed is how many acquisitions this goroutine threw away because the
	// loop was not taking them, counted here and recorded there. A gap that
	// does not say it is a gap is the worst failure this system can produce
	// (§10.5).
	missed uint32

	idn string
	err error
}

var (
	errScopeNotConnected = errors.New("the scope is not connected")
	errScopeBusy         = errors.New("the scope has too many writes waiting; try again")
	errScopeInstrument   = errors.New("the scope stopped answering")
)

// run owns the connection until stop is closed.
//
// It is a small state machine rather than a blocking sequence, so that a write
// is taken between acquisitions instead of waiting behind one: arm, watch INR
// for the acquisition completing, read every displayed channel, again.
func (s *scopeDriver) run(stop <-chan struct{}) {
	var conn *dso.Scope
	var retry <-chan time.Time = time.After(0)
	reported := false // a disconnection is recorded once, not at every retry
	armed := false
	var run uint32
	var missed uint32
	var lastVert []vertState
	var lastTB timeState

	tick := time.NewTicker(s.poll)
	defer tick.Stop()

	emit := func(e scopeEvent) bool {
		e.s = s
		if e.t.IsZero() {
			e.t = time.Now()
		}
		e.missed, missed = missed, 0
		select {
		case s.events <- e:
			return true
		case <-stop:
			return false
		}
	}
	// offer is emit for an acquisition, which may be thrown away rather than
	// waited on: the loop records the bus, and a scope must never be the reason
	// it stops. What was thrown away is carried to the next event.
	offer := func(e scopeEvent) bool {
		e.s = s
		if e.t.IsZero() {
			e.t = time.Now()
		}
		e.missed = missed
		select {
		case s.events <- e:
			missed = 0
			return true
		default:
			missed++
			return false
		}
	}
	drop := func(err error) {
		if conn != nil {
			conn.Close()
			conn = nil
		}
		armed = false
		if !reported {
			emit(scopeEvent{kind: scDisconnected, err: err})
			reported = true
		}
		retry = time.After(2 * time.Second)
	}
	readSettings := func(why uint8, onlyChanged bool) error {
		set, err := conn.Settings()
		if err != nil {
			return err
		}
		tb := timeState{tdiv: set.TDiv, trdl: set.Trdl, msiz: set.Msiz, sara: set.Sara, mode: set.Mode}
		if !onlyChanged || tb != lastTB {
			emit(scopeEvent{kind: scTimebase, why: why, tb: tb})
		}
		lastTB = tb
		for ch := 1; ch <= s.channels; ch++ {
			cs, err := conn.ChanSettings(ch)
			if err != nil {
				return err
			}
			v := vertState{vdiv: cs.VDiv, ofst: cs.Ofst, on: cs.On}
			if !onlyChanged || v != lastVert[ch-1] {
				emit(scopeEvent{kind: scVert, ch: ch, why: why, vert: v})
			}
			lastVert[ch-1] = v
		}
		return nil
	}
	do := func(cmd scopeCmd) {
		var err error
		if conn == nil {
			err = errScopeNotConnected
		} else if err = s.exec(conn, cmd, lastVert, &lastTB, emit); err != nil {
			drop(err)
			err = fmt.Errorf("%w: %v", errScopeInstrument, err)
		}
		if cmd.reply != nil {
			cmd.reply <- reply{seq: cmd.seq, err: err}
		}
	}

	for {
		select {
		case <-stop:
			if conn != nil {
				// The instrument is left exactly as it is. A scope that is
				// mid-acquisition when the daemon stops has nothing to be saved
				// from, and stopping it would be an act nobody asked for (§7).
				conn.Close()
			}
			return

		case <-retry:
			retry = nil
			c, err := dso.Dial(s.addr, s.cfg)
			if err == nil {
				var idn string
				if idn, err = c.Identify(); err == nil {
					conn, reported, armed = c, false, false
					lastVert = make([]vertState, s.channels)
					lastTB = timeState{}
					emit(scopeEvent{kind: scConnected, idn: idn})
					// The scope's own sparsing is turned off: every nth point is
					// the aliasing an envelope exists to prevent (§10.4), and
					// what the rack draws is decimated here, from every sample.
					if err = c.Sparse(1); err == nil {
						if err = readSettings(evConnected, false); err == nil {
							continue
						}
					}
				}
				c.Close()
			}
			drop(err)

		case cmd := <-s.cmds:
			do(cmd)

		case <-tick.C:
			if conn == nil {
				continue
			}
			if !armed {
				if err := conn.Single(); err != nil {
					drop(err)
					continue
				}
				armed = true
				continue
			}
			inr, err := conn.Inr()
			if err != nil {
				drop(err)
				continue
			}
			if inr&dso.InrAcquired == 0 {
				continue
			}
			armed = false
			run++
			if err := s.readOut(conn, run, offer); err != nil {
				if !offer(scopeEvent{kind: scAcqFailed, run: run, err: err}) {
					// The failure could not even be handed over. It is still
					// counted, by offer, and the next event carries it.
				}
				drop(err)
			}
		}
	}
}

// readOut reads every displayed channel of one acquisition and offers each to
// the loop. The last one says so, which is how the loop knows the run is
// complete and may be closed.
func (s *scopeDriver) readOut(conn *dso.Scope, run uint32, offer func(scopeEvent) bool) error {
	var want []int
	for ch := 1; ch <= s.channels; ch++ {
		cs, err := conn.ChanSettings(ch)
		if err != nil {
			return err
		}
		if cs.On {
			want = append(want, ch)
		}
	}
	if len(want) == 0 {
		return nil
	}
	// The scope's own account of when it triggered, read once for the
	// acquisition. It is on the scope's clock, whose origin the guide does not
	// say, so it is carried beside the daemon's stamp and never in place of it.
	ft, ftErr := conn.FrameTime()
	// Which frame the readout will return, asked rather than assumed: it is 1
	// outside Sequence mode, and someone at the front panel can make it another.
	frame, frameErr := conn.Frame()
	for i, ch := range want {
		a, err := conn.Read(ch, nil)
		if err != nil {
			return err
		}
		if ftErr == nil {
			a.FrameTime, a.HaveFrameTime = ft, true
		}
		if frameErr == nil {
			a.Frame = frame
		}
		offer(scopeEvent{kind: scAcquired, ch: ch, run: run, acq: a, t: a.At, last: i == len(want)-1})
	}
	return nil
}

// exec carries out one setting and reports what the scope holds after it. Its
// error is the connection's; a value the instrument clamped is not an error at
// all, and shows up as the readback differing from what was asked (§4).
func (s *scopeDriver) exec(conn *dso.Scope, cmd scopeCmd, lastVert []vertState,
	lastTB *timeState, emit func(scopeEvent) bool) error {

	ch := chanOf(cmd.part)
	var err error
	switch cmd.what {
	case "vdiv":
		err = conn.SetVDiv(ch, cmd.value)
	case "ofst":
		err = conn.SetOfst(ch, cmd.value)
	case "on":
		err = conn.SetTrace(ch, cmd.value != 0)
	case "tdiv":
		err = conn.SetTDiv(cmd.value)
	case "trdl":
		err = conn.SetTrdl(cmd.value)
	case "msiz":
		err = conn.SetMsiz(cmd.value)
	default:
		return fmt.Errorf("no such setting %q", cmd.what)
	}
	if err != nil {
		return err
	}

	// Both accounts are read back, whichever was written: a channel switched on
	// halves the memory depth, and a timebase change moves the sample rate, so
	// a write to one of them changes what the other reports holding.
	set, err := conn.Settings()
	if err != nil {
		return err
	}
	tb := timeState{tdiv: set.TDiv, trdl: set.Trdl, msiz: set.Msiz, sara: set.Sara, mode: set.Mode}
	*lastTB = tb
	emit(scopeEvent{kind: scTimebase, seq: cmd.seq, why: evApplied, tb: tb})
	for c := 1; c <= s.channels; c++ {
		cs, err := conn.ChanSettings(c)
		if err != nil {
			return err
		}
		v := vertState{vdiv: cs.VDiv, ofst: cs.Ofst, on: cs.On}
		if v != lastVert[c-1] || c == ch {
			emit(scopeEvent{kind: scVert, ch: c, seq: cmd.seq, why: evApplied, vert: v})
		}
		lastVert[c-1] = v
	}
	return nil
}

// Buckets is how many columns an acquisition is reduced to for the rack: a
// chart is at most a couple of thousand pixels wide, and three thousand
// min/max pairs is 48 kB against a megapoint acquisition's 4 MB (§10.4).
const Buckets = 3000

// scopeChan is one channel of one scope as the loop records it.
type scopeChan struct {
	vert    *teeStream // the settings, set beside applied
	samples *teeWave   // full rate, in the recording only; nil unless configured
	env     *teeWave   // the min/max envelope, in both writers
	set     vertState
	known   [3]bool // vdiv, ofst and on have been set since the daemon started
	applied vertState
}

// scopeInst is a scope's state in the loop.
type scopeInst struct {
	drv       *scopeDriver
	timebase  *teeStream
	acq       *teeStream
	chans     []*scopeChan
	setTB     timeState
	knownTB   [3]bool // tdiv, trdl and msiz have been set since the daemon started
	appliedTB timeState

	// index counts acquisitions, for the run's ordinal. runs counts the ones
	// whose samples reached the recording, dropped the ones the rack could not
	// keep up with, and missed the ones the loop was not there to take.
	index   uint32
	dropped int64
	missed  int64
	failed  int64

	// volts and env are reused across acquisitions: a megapoint acquisition a
	// trigger must not allocate one.
	volts []float32
	env   dso.Envelope
	buf   []byte
}

// Record layouts, in bytes.
//
// The vertical settings stream, <scope>.ch<N>.vert: t 0-7, seq 8-11, event 12,
// flags 13 (on.set bit 0, on.applied bit 1, connected bit 2, vdiv.known bit 3,
// ofst.known bit 4, on.known bit 5), vdiv.set 16-23, ofst.set 24-31,
// vdiv.applied 32-39, ofst.applied 40-47, error 48-111.
const (
	svSeq        = 8
	svEvent      = 12
	svFlags      = 13
	svVDivSet    = 16
	svOfstSet    = 24
	svVDivApp    = 32
	svOfstApp    = 40
	svErr        = 48
	svErrLen     = 64
	svBytes      = svErr + svErrLen
	svFieldConn  = 3
	svFieldVDivK = 4
	svFieldOfstK = 5
	svFieldOnK   = 6
)

// The timebase stream, <scope>.timebase: t 0-7, seq 8-11, event 12, flags 13
// (connected bit 0, tdiv.known bit 1, trdl.known bit 2, msiz.known bit 3),
// tdiv.set 16-23, trdl.set 24-31, msiz.set 32-39, tdiv.applied 40-47,
// trdl.applied 48-55, msiz.applied 56-63, sara 64-71, error 72-135.
const (
	stSeq        = 8
	stEvent      = 12
	stFlags      = 13
	stTDivSet    = 16
	stTrdlSet    = 24
	stMsizSet    = 32
	stTDivApp    = 40
	stTrdlApp    = 48
	stMsizApp    = 56
	stSara       = 64
	stErr        = 72
	stErrLen     = 64
	stBytes      = stErr + stErrLen
	stFieldConn  = 3
	stFieldTDivK = 4
	stFieldTrdlK = 5
	stFieldMsizK = 6
)

// The acquisition stream, <scope>.acq: one record an acquisition a channel.
const (
	saRun     = 8
	saChan    = 12
	saEvent   = 13
	saSign    = 14
	saFlags   = 15 // trigger_delay bit 0, full_rate bit 1, frame_time.known bit 2, exact bit 3
	saPoints  = 16
	saBuckets = 20
	saVDiv    = 24
	saOfst    = 32
	saTDiv    = 40
	saSara    = 48
	saTrdl    = 56
	saMsiz    = 64
	saStep    = 72
	saFirst   = 80
	saFrame   = 88
	saFTime   = 96
	saReadout = 104
	saErr     = 112
	saErrLen  = 64
	saBytes   = saErr + saErrLen

	// The field index the frame time guards on, counting the axis field that
	// record.Schema puts first: t, run, channel, event, sign, trigger_delay,
	// full_rate, frame_time.known.
	saFieldFTimeKnown = 7
)

// What an acquisition record says happened.
const (
	acqRecorded = 1 // the samples are in this recording
	acqDropped  = 2 // the rack's stream could not keep up, and did not get them
	acqMissed   = 3 // the daemon was not there to take it
	acqFailed   = 4 // the readout did not finish
)

func held(f logb.Field) logb.Field { f.Hold = true; return f }

func guard(f logb.Field, on uint16) logb.Field {
	f.Guarded, f.GuardField, f.GuardValue = true, on, 1
	return f
}

func f64(name string, at uint32, unit, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 64, Type: logb.TypeFloat, Unit: unit, Desc: desc}
}

func u32(name string, at uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 32, Type: logb.TypeUint, Desc: desc}
}

func u8(name string, at uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 8, Type: logb.TypeUint, Desc: desc}
}

func bit(name string, at, b uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8*at + b, BitWidth: 1, Type: logb.TypeBool, Desc: desc}
}

func str(name string, at, n uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 8 * n, Type: logb.TypeString, Desc: desc}
}

// vertSchema is one channel's vertical settings, <scope>.ch<N>.vert, with §4's
// accounts beside each other: what a client asked for and what the scope
// reports holding after it, which differ whenever the instrument clamps a value
// to one of the 1-2-5 steps it has.
func vertSchema(scope string, ch int) *logb.Schema {
	name := fmt.Sprintf("%s.ch%d.vert", scope, ch)
	return record.Schema(name, 8*svBytes, []logb.Field{
		u32("seq", svSeq, "the request this record answers; 0 for what the scope reported unasked"),
		u8("event", svEvent, "1 a write was admitted, 2 the scope's readback after it, "+
			"3 a periodic readback found a change, 4 the scope answered, 5 it stopped answering"),
		held(bit("connected", svFlags, 2, "the scope is answering")),
		held(bit("vdiv.known", svFlags, 3, "vdiv.set has been set since the daemon started")),
		held(bit("ofst.known", svFlags, 4, "ofst.set has been set since the daemon started")),
		held(bit("on.known", svFlags, 5, "on.set has been set since the daemon started")),
		guard(held(f64("vdiv.set", svVDivSet, "V", "the volts a division a client asked for")), svFieldVDivK),
		guard(held(f64("ofst.set", svOfstSet, "V", "the vertical offset a client asked for")), svFieldOfstK),
		guard(held(bit("on.set", svFlags, 0, "whether a client asked for the channel to be displayed")), svFieldOnK),
		guard(held(f64("vdiv.applied", svVDivApp, "V",
			"the volts a division the scope reports holding, clamped to the steps it has")), svFieldConn),
		guard(held(f64("ofst.applied", svOfstApp, "V", "the vertical offset the scope reports holding")), svFieldConn),
		guard(held(bit("on.applied", svFlags, 1, "whether the scope reports the channel displayed")), svFieldConn),
		str("error", svErr, svErrLen, "why the scope stopped answering, or why a write failed; empty for none"),
	}, map[string]string{
		"instrument": scope,
		"channel":    strconv.Itoa(ch),
		"source":     "the ktestd scope driver: requests as admitted, then the scope's own readback",
		"clock":      "userspace: the daemon's clock when it asked or was answered",
	})
}

// timebaseSchema is the instrument's horizontal settings, <scope>.timebase.
// The sample rate has one account only, because it is not set: the scope picks
// it from the timebase and the memory depth, and reports what it picked.
func timebaseSchema(scope string) *logb.Schema {
	name := scope + ".timebase"
	return record.Schema(name, 8*stBytes, []logb.Field{
		u32("seq", stSeq, "the request this record answers; 0 for what the scope reported unasked"),
		u8("event", stEvent, "1 a write was admitted, 2 the scope's readback after it, "+
			"3 a periodic readback found a change, 4 the scope answered, 5 it stopped answering"),
		held(bit("connected", stFlags, 0, "the scope is answering")),
		held(bit("tdiv.known", stFlags, 1, "tdiv.set has been set since the daemon started")),
		held(bit("trdl.known", stFlags, 2, "trdl.set has been set since the daemon started")),
		held(bit("msiz.known", stFlags, 3, "msiz.set has been set since the daemon started")),
		guard(held(f64("tdiv.set", stTDivSet, "s", "the seconds a division a client asked for")), stFieldTDivK),
		guard(held(f64("trdl.set", stTrdlSet, "s", "the trigger delay a client asked for")), stFieldTrdlK),
		guard(held(f64("msiz.set", stMsizSet, "", "the memory depth in points a client asked for")), stFieldMsizK),
		guard(held(f64("tdiv.applied", stTDivApp, "s",
			"the seconds a division the scope reports holding, clamped to the steps it has")), stFieldConn),
		guard(held(f64("trdl.applied", stTrdlApp, "s", "the trigger delay the scope reports holding")), stFieldConn),
		guard(held(f64("msiz.applied", stMsizApp, "",
			"the memory depth the scope reports holding; two displayed channels share one memory")), stFieldConn),
		guard(held(f64("sara", stSara, "Hz",
			"the sample rate the scope reports; it is chosen from the timebase and the depth, never set")), stFieldConn),
		str("error", stErr, stErrLen, "why the scope stopped answering, or why a write failed; empty for none"),
	}, map[string]string{
		"instrument": scope,
		"source":     "the ktestd scope driver: requests as admitted, then the scope's own readback",
		"clock":      "userspace: the daemon's clock when it asked or was answered",
	})
}

// acqSchema is the scope's account of its acquisitions, <scope>.acq: one record
// per channel per trigger, whether or not its samples reached the recording.
//
// This is where §10.5's rule is kept. A waveform may be dropped; a dropped one
// must appear in the recording as a drop. So every acquisition the daemon knew
// about has a record here, and the event field says which of them the reader
// will find samples for.
func acqSchema(scope string) *logb.Schema {
	name := scope + ".acq"
	return record.Schema(name, 8*saBytes, []logb.Field{
		u32("run", saRun, "the run_id the samples were written under; one run an acquisition (§5.4)"),
		u8("channel", saChan, "the scope channel, from 1"),
		u8("event", saEvent, "1 the samples are in this recording, 2 the rack's stream could not keep up "+
			"and did not get them, 3 the daemon was not there to take the acquisition, 4 the readout did not finish"),
		u8("sign", saSign, "how a code above 127 was made negative: 0 subtract 256 (guide PG01-E02D), "+
			"1 subtract 255 (PG01-E02C). The two differ by one code on every negative sample"),
		bit("trigger_delay", saFlags, 0, "the trigger delay was taken into the time axis, which the guides do not do"),
		bit("full_rate", saFlags, 1, "every sample went into this recording, not the envelope alone"),
		bit("frame_time.known", saFlags, 2, "the scope reported a frame time for this acquisition"),
		bit("exact", saFlags, 3, "the envelope is the samples themselves: there were no more of them than buckets"),
		u32("points", saPoints, "samples the scope handed over"),
		u32("buckets", saBuckets, "columns the envelope was reduced to"),
		f64("vdiv", saVDiv, "V", "the volts a division the conversion to volts used"),
		f64("ofst", saOfst, "V", "the vertical offset the conversion to volts used"),
		f64("tdiv", saTDiv, "s", "the seconds a division in force"),
		f64("sara", saSara, "Hz", "the sample rate the axis was computed from"),
		f64("trdl", saTrdl, "s", "the trigger delay the scope reported"),
		f64("msiz", saMsiz, "", "the memory depth the scope reported"),
		f64("step", saStep, "s", "the interval between samples, which is 1/sara"),
		f64("first", saFirst, "s", "where sample 0 lies relative to this record's time, normally negative"),
		u32("frame", saFrame, "the History frame this acquisition was read from; 1 outside Sequence mode"),
		guard(f64("frame_time", saFTime, "s",
			"when the scope says it triggered, on the scope's own clock, whose origin is not documented. "+
				"It is beside this record's own time and never in place of it (§11)"), saFieldFTimeKnown),
		f64("readout", saReadout, "s", "how long the readout took"),
		str("error", saErr, saErrLen, "why the readout did not finish; empty for none"),
	}, map[string]string{
		"instrument": scope,
		"source":     "the ktestd scope driver, one record per channel per trigger",
		"clock":      "userspace: the daemon's clock when the readout finished, which is not when the scope triggered",
	})
}

// samplesSchema is one channel's samples, <scope>.ch<N>: implicit uniform axis,
// one record a sample, zero axis bytes (§5.4).
//
// A sample is f32 volts, decided in §13. The alternative was the digitiser's
// 8-bit code with the conversion in the schema, which is compact and exactly
// what was measured — and which makes every volts/div or offset change a schema
// change and so, under Logb §6.1, a new stream identity. §5.2's exemption
// covers axis_step alone, so the vertical axis has no equivalent way out, and
// §4 rests on this name meaning one thing for the whole session. The code is
// recoverable: vdiv, ofst and the sign rule are in the RUN frame and in
// <scope>.acq for every acquisition.
func samplesSchema(scope string, ch int, step time.Duration) *logb.Schema {
	name := fmt.Sprintf("%s.ch%d", scope, ch)
	return record.UniformSchema(name, 32, step, []logb.Field{
		{Name: "v", BitOffset: 0, BitWidth: 32, Type: logb.TypeFloat, Unit: "V",
			Desc: "the sample in volts: the digitiser's code x (vdiv / 25) - ofst, with the acquisition's own vdiv and ofst"},
	}, map[string]string{
		"instrument": scope,
		"channel":    strconv.Itoa(ch),
		"source":     "the scope's samples, converted with the settings in force at the readout",
		"clock":      "userspace: the acquisition is placed at the daemon's stamp at readout, not at the trigger (§12, slice 3)",
	})
}

// Envelope record: flags 0 (present bit 0), n 4-7, min 8-11, max 12-15.
const (
	seFlags     = 0
	seN         = 4
	seMin       = 8
	seMax       = 12
	seBytes     = 16
	seFieldPres = 0
)

// envSchema is one channel's envelope, <scope>.ch<N>.env: the min/max band a
// screen can draw, one bucket a column (§10.4).
//
// present is not decoration. Where a bucket holds no sample the values are
// meaningless, and a chart must break the band rather than draw a zero there.
func envSchema(scope string, ch int, step time.Duration) *logb.Schema {
	name := fmt.Sprintf("%s.ch%d.env", scope, ch)
	return record.UniformSchema(name, 8*seBytes, step, []logb.Field{
		bit("present", seFlags, 0, "this bucket held at least one sample"),
		u32("n", seN, "samples in this bucket"),
		guard(logb.Field{Name: "min", BitOffset: 8 * seMin, BitWidth: 32, Type: logb.TypeFloat, Unit: "V",
			Desc: "the least sample in this bucket"}, seFieldPres),
		guard(logb.Field{Name: "max", BitOffset: 8 * seMax, BitWidth: 32, Type: logb.TypeFloat, Unit: "V",
			Desc: "the greatest sample in this bucket"}, seFieldPres),
	}, map[string]string{
		"instrument": scope,
		"channel":    strconv.Itoa(ch),
		"kind":       "envelope",
		"of":         fmt.Sprintf("%s.ch%d", scope, ch),
		"source":     "a min/max reduction of every sample of the acquisition, not the scope's own sparsing",
		"clock":      "userspace: the acquisition is placed at the daemon's stamp at readout, not at the trigger (§12, slice 3)",
	})
}

// scopeSet records an admitted write and hands it to the scope, which answers
// the request itself once the write is done or failed.
func (l *loop) scopeSet(in *inst, r *request, t time.Time) reply {
	sc := in.scp
	ch := chanOf(r.part)
	switch r.what {
	case "vdiv":
		sc.chans[ch-1].set.vdiv, sc.chans[ch-1].known[0] = r.value, true
	case "ofst":
		sc.chans[ch-1].set.ofst, sc.chans[ch-1].known[1] = r.value, true
	case "on":
		sc.chans[ch-1].set.on, sc.chans[ch-1].known[2] = r.value != 0, true
	case "tdiv":
		sc.setTB.tdiv, sc.knownTB[0] = r.value, true
	case "trdl":
		sc.setTB.trdl, sc.knownTB[1] = r.value, true
	case "msiz":
		sc.setTB.msiz, sc.knownTB[2] = r.value, true
	}
	if ch > 0 {
		l.scopeVert(in, ch, t, l.seq, evRequested, "")
	} else {
		l.scopeTimebase(in, t, l.seq, evRequested, "")
	}
	sc.drv.cmds <- scopeCmd{part: r.part, what: r.what, value: r.value, seq: l.seq, reply: r.reply}
	return reply{deferred: true}
}

// scopeEvent records what a scope reported.
func (l *loop) scopeEvent(e scopeEvent) {
	in := l.insts[e.s.name]
	sc := in.scp
	if e.missed > 0 {
		// Acquisitions the driver threw away because this loop was not taking
		// them. They are recorded before whatever did get through, because that
		// is the order they happened in.
		for i := uint32(0); i < e.missed; i++ {
			sc.missed++
			l.acqRecord(in, e.t, 0, 0, acqMissed, nil, false, "the daemon was not taking acquisitions")
		}
	}
	switch e.kind {
	case scConnected:
		in.connected = true
		if err := l.w.Meta("scope."+in.name+".idn", e.idn); err != nil {
			fatalf("write: %v", err)
		}
	case scDisconnected:
		in.connected = false
		msg := "not answering"
		if e.err != nil {
			msg = e.err.Error()
		}
		l.scopeTimebase(in, e.t, 0, evDisconnected, msg)
		for ch := range sc.chans {
			l.scopeVert(in, ch+1, e.t, 0, evDisconnected, msg)
		}
	case scVert:
		sc.chans[e.ch-1].applied = e.vert
		l.scopeVert(in, e.ch, e.t, e.seq, e.why, "")
	case scTimebase:
		sc.appliedTB = e.tb
		l.scopeTimebase(in, e.t, e.seq, e.why, "")
	case scAcqFailed:
		sc.failed++
		msg := "the readout did not finish"
		if e.err != nil {
			msg = e.err.Error()
		}
		l.acqRecord(in, e.t, e.run, 0, acqFailed, nil, false, msg)
	case scAcquired:
		l.acquisition(in, e)
	}
}

// acquisition writes one channel's share of an acquisition: its envelope
// always, its samples at full rate where the configuration asked for them, and
// a record in <scope>.acq saying which of those a reader will find.
//
// §10.5's policy is applied here, and only to the rack's stream. The recording
// keeps everything: a waveform may be dropped, and this is the one place where
// dropping is a decision rather than an overflow, so it is taken where the
// alternative — disconnecting the browser whole, which is what the Hub does on
// its own — would cost more than the acquisition is worth.
func (l *loop) acquisition(in *inst, e scopeEvent) {
	sc := in.scp
	a := e.acq
	toRack := l.rackRoom(a)
	if !toRack {
		sc.dropped++
	}

	sc.index++
	acq := &record.Acq{RunID: e.run, Index: sc.index, Params: acqParams(in.name, a)}
	if err := l.w.BeginAcq(e.t, acq, toRack); err != nil {
		fatalf("write: %v", err)
	}

	pc := sc.chans[e.ch-1]
	if pc.samples != nil {
		sc.volts = a.Volts(sc.volts)
		sc.buf = appendFloat32s(sc.buf[:0], sc.volts)
		if err := l.w.WriteSamples(e.t, pc.samples, e.run, a.Step(), a.First(),
			uint32(len(sc.volts)), sc.buf, toRack); err != nil {
			fatalf("write: %v", err)
		}
	}
	a.Envelope(&sc.env, Buckets)
	sc.buf = appendEnvelope(sc.buf[:0], &sc.env)
	step := time.Duration(math.Round(sc.env.Step * 1e9))
	first := time.Duration(math.Round(sc.env.First * 1e9))
	if err := l.w.WriteSamples(e.t, pc.env, e.run, step, first,
		uint32(sc.env.Len()), sc.buf, toRack); err != nil {
		fatalf("write: %v", err)
	}

	event := uint8(acqRecorded)
	if !toRack {
		event = acqDropped
	}
	l.acqRecord(in, e.t, e.run, e.ch, event, a, pc.samples != nil, "")
	if e.last {
		l.w.EndAcq(e.run, toRack)
	}
}

// rackRoom says whether the rack's stream can take this acquisition.
//
// The Hub disconnects a consumer that falls behind, which is right for the bus
// — a hole in a byte stream is corruption, not loss — and wrong for a waveform,
// which may be dropped and said to have been. So the decision is taken here,
// before the bytes are written: if what this acquisition would add does not fit
// in the room the slowest consumer has left, it is not written to the rack at
// all, and the recording says so.
func (l *loop) rackRoom(a *dso.Acquisition) bool {
	// The envelope is what the rack gets, and its size is known: one record a
	// bucket. A margin leaves room for the records every other stream is still
	// writing while this one is large.
	const margin = 1 << 20
	return l.rackHub.Room() >= Buckets*seBytes+margin
}

// acqParams is the RUN frame's parameter set: everything needed to read the
// samples back as what the instrument measured (§5.4).
func acqParams(scope string, a *dso.Acquisition) map[string]string {
	p := map[string]string{
		"instrument":         scope,
		"channel":            strconv.Itoa(a.Channel),
		"vdiv":               g(a.Chan.VDiv),
		"ofst":               g(a.Chan.Ofst),
		"tdiv":               g(a.Set.TDiv),
		"sara":               g(a.Set.Sara),
		"trdl":               g(a.Set.Trdl),
		"msiz":               g(a.Set.Msiz),
		"mode":               a.Set.Mode,
		"points":             strconv.Itoa(a.Len()),
		"step":               a.Step().String(),
		"first":              a.First().String(),
		"sign":               a.Sign.String(),
		"codes_per_division": strconv.Itoa(dso.Codes),
		"conversion":         "volts = code x (vdiv / 25) - ofst",
		"trigger_delay":      strconv.FormatBool(a.TrigDelay),
		"readout":            a.Elapsed.String(),
		"frame":              strconv.Itoa(a.Frame),
	}
	if a.HaveFrameTime {
		// The scope's own clock, whose origin no document states. It is here to
		// be compared with the daemon's stamp, never to stand in for it (§11).
		p["frame_time"] = dso.FormatFrameTime(a.FrameTime)
		p["frame_time.clock"] = "the scope's own, origin undocumented; not the recording's timeline"
	}
	return p
}

func g(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// appendFloat32s writes the samples as the schema declares them: f32, little
// endian, one to a record.
func appendFloat32s(dst []byte, v []float32) []byte {
	if cap(dst) < 4*len(v) {
		dst = make([]byte, 0, 4*len(v))
	}
	for _, x := range v {
		dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(x))
	}
	return dst
}

// appendEnvelope writes the envelope in the layout envSchema declares.
func appendEnvelope(dst []byte, e *dso.Envelope) []byte {
	n := e.Len() * seBytes
	if cap(dst) < n {
		dst = make([]byte, 0, n)
	}
	dst = dst[:n]
	clear(dst)
	for b := 0; b < e.Len(); b++ {
		rec := dst[b*seBytes:]
		if e.N[b] > 0 {
			rec[seFlags] = 1
		}
		binary.LittleEndian.PutUint32(rec[seN:], e.N[b])
		binary.LittleEndian.PutUint32(rec[seMin:], math.Float32bits(e.Min[b]))
		binary.LittleEndian.PutUint32(rec[seMax:], math.Float32bits(e.Max[b]))
	}
	return dst
}

// scopeVert writes one channel's settings record.
func (l *loop) scopeVert(in *inst, ch int, t time.Time, seq uint32, why uint8, msg string) {
	sc := in.scp
	pc := sc.chans[ch-1]
	err := l.w.Write(t, pc.vert, func(rec []byte) {
		le := binary.LittleEndian
		le.PutUint32(rec[svSeq:], seq)
		rec[svEvent] = why
		var fl byte
		for b, on := range []bool{pc.set.on, pc.applied.on, in.connected, pc.known[0], pc.known[1], pc.known[2]} {
			if on {
				fl |= 1 << b
			}
		}
		rec[svFlags] = fl
		le.PutUint64(rec[svVDivSet:], math.Float64bits(pc.set.vdiv))
		le.PutUint64(rec[svOfstSet:], math.Float64bits(pc.set.ofst))
		le.PutUint64(rec[svVDivApp:], math.Float64bits(pc.applied.vdiv))
		le.PutUint64(rec[svOfstApp:], math.Float64bits(pc.applied.ofst))
		copy(rec[svErr:svErr+svErrLen], truncate(msg, svErrLen))
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

// scopeTimebase writes the instrument's horizontal settings record.
func (l *loop) scopeTimebase(in *inst, t time.Time, seq uint32, why uint8, msg string) {
	sc := in.scp
	err := l.w.Write(t, sc.timebase, func(rec []byte) {
		le := binary.LittleEndian
		le.PutUint32(rec[stSeq:], seq)
		rec[stEvent] = why
		var fl byte
		for b, on := range []bool{in.connected, sc.knownTB[0], sc.knownTB[1], sc.knownTB[2]} {
			if on {
				fl |= 1 << b
			}
		}
		rec[stFlags] = fl
		le.PutUint64(rec[stTDivSet:], math.Float64bits(sc.setTB.tdiv))
		le.PutUint64(rec[stTrdlSet:], math.Float64bits(sc.setTB.trdl))
		le.PutUint64(rec[stMsizSet:], math.Float64bits(sc.setTB.msiz))
		le.PutUint64(rec[stTDivApp:], math.Float64bits(sc.appliedTB.tdiv))
		le.PutUint64(rec[stTrdlApp:], math.Float64bits(sc.appliedTB.trdl))
		le.PutUint64(rec[stMsizApp:], math.Float64bits(sc.appliedTB.msiz))
		le.PutUint64(rec[stSara:], math.Float64bits(sc.appliedTB.sara))
		copy(rec[stErr:stErr+stErrLen], truncate(msg, stErrLen))
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

// acqRecord writes one record of the scope's account of its acquisitions. a is
// nil for an acquisition that produced no samples — one missed or one whose
// readout failed — and the record then carries the event and nothing else,
// which is the honest shape of "this happened and there is nothing to show".
func (l *loop) acqRecord(in *inst, t time.Time, run uint32, ch int, event uint8,
	a *dso.Acquisition, full bool, msg string) {

	err := l.w.Write(t, in.scp.acq, func(rec []byte) {
		le := binary.LittleEndian
		le.PutUint32(rec[saRun:], run)
		rec[saChan] = byte(ch)
		rec[saEvent] = event
		copy(rec[saErr:saErr+saErrLen], truncate(msg, saErrLen))
		if a == nil {
			return
		}
		rec[saSign] = byte(a.Sign)
		var fl byte
		if a.TrigDelay {
			fl |= 1
		}
		if full {
			fl |= 2
		}
		if a.HaveFrameTime {
			fl |= 4
		}
		if in.scp.env.Exact {
			fl |= 8
		}
		rec[saFlags] = fl
		le.PutUint32(rec[saPoints:], uint32(a.Len()))
		le.PutUint32(rec[saBuckets:], uint32(in.scp.env.Len()))
		for at, v := range map[uint32]float64{
			saVDiv: a.Chan.VDiv, saOfst: a.Chan.Ofst, saTDiv: a.Set.TDiv, saSara: a.Set.Sara,
			saTrdl: a.Set.Trdl, saMsiz: a.Set.Msiz,
			saStep: a.Step().Seconds(), saFirst: a.First().Seconds(),
			saFTime: a.FrameTime.Seconds(), saReadout: a.Elapsed.Seconds(),
		} {
			le.PutUint64(rec[at:], math.Float64bits(v))
		}
		le.PutUint32(rec[saFrame:], uint32(a.Frame))
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

// scopeQuantities are the settings of a scope that can be written, by part.
// The list is what the API admits, what the limits file may bound, and what the
// recording states a limit for — all three from one place, so that a setting
// cannot exist in one of them and not the others.
var scopeQuantities = map[string][]string{
	"vert":     {"vdiv", "ofst", "on"},
	"timebase": {"tdiv", "trdl", "msiz"},
}

// scopeParts is every settable part of a scope with the given channel count.
func scopeParts(channels int) []string {
	parts := []string{"timebase"}
	for ch := 1; ch <= channels; ch++ {
		parts = append(parts, fmt.Sprintf("ch%d.vert", ch))
	}
	return parts
}

// quantitiesOf is what may be set on a part, whichever instrument it belongs to.
func quantitiesOf(part string) []string {
	if part == "timebase" {
		return scopeQuantities["timebase"]
	}
	if strings.HasSuffix(part, ".vert") {
		return scopeQuantities["vert"]
	}
	return []string{"v", "i", "out"} // a supply channel
}

// scopeChannel matches a scope channel, scope1.ch1, and not the streams around
// it: scope1.ch1.env is its envelope and scope1.ch1.vert its settings, and
// neither is a thing a scope widget draws.
var scopeChannel = regexp.MustCompile(`^[A-Za-z0-9_-]+\.ch[1-9][0-9]*$`)

// isSwitch says whether a quantity is on or off rather than a number. A supply
// channel's output and a scope channel's display are the two, and naming them
// beats inferring it: a setting that is not listed here takes a number, and is
// refused a boolean until someone decides otherwise.
func isSwitch(what string) bool { return what == "out" || what == "on" }
