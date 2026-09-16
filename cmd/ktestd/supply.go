//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rveen/logb"

	"github.com/rveen/ktest/psu"
	"github.com/rveen/ktest/record"
)

// A supply is one bench supply as the daemon drives it: a goroutine that owns
// the connection, so that a slow or silent instrument can never hold up the
// loop that records the bus. The loop decides what may be done (§7) and records
// what was done; this goroutine does it and says what came back.
//
// It keeps the three accounts of §4 apart. A setpoint is set, then read back
// from the supply — which may have clamped it — and the supply's error queue is
// drained, since SCPI reports a refusal nowhere else. Every channel is measured
// at the poll rate, and every tenth poll the setpoints are read back again, so
// that a knob turned on the front panel is in the recording too.
type supply struct {
	name     string
	addr     string
	channels int
	poll     time.Duration

	// cmds carries admitted writes, and only the loop sends on it, after
	// checking there is room, so that it never blocks on an instrument. off is
	// the stop, kept apart so that it never waits behind writes: it holds at
	// most one, and a second stop before the first is carried out is the same
	// stop.
	cmds   chan supplyCmd
	off    chan uint32
	events chan<- supplyEvent
}

// A supplySpec is one -psu flag: name=host:port[,channels].
type supplySpec struct {
	name     string
	addr     string
	channels int
}

type supplyFlags []supplySpec

func (f *supplyFlags) String() string {
	var s []string
	for _, x := range *f {
		s = append(s, fmt.Sprintf("%s=%s,%d", x.name, x.addr, x.channels))
	}
	return strings.Join(s, " ")
}

func (f *supplyFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	if !ok || name == "" || rest == "" {
		return errors.New(`want name=host:port[,channels], e.g. psu1=192.168.1.20:5025,3`)
	}
	// The name begins every channel's name (§4), so it may not contain the dot
	// that separates the parts of one, and it must fit the recording.
	if len(name) > 16 || strings.IndexFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return fmt.Errorf("supply name %q: up to 16 letters, digits, '-' or '_'", name)
	}
	addr, n, _ := strings.Cut(rest, ",")
	spec := supplySpec{name: name, addr: addr, channels: 1}
	if n != "" {
		c, err := strconv.Atoi(n)
		if err != nil || c < 1 || c > 16 {
			return fmt.Errorf("supply %s: %q channels; a supply has 1 to 16", name, n)
		}
		spec.channels = c
	}
	for _, x := range *f {
		if x.name == name {
			return fmt.Errorf("supply %s is named twice", name)
		}
	}
	*f = append(*f, spec)
	return nil
}

type supplyOp int

const (
	opSet supplyOp = iota // one setpoint or output
	opOff                 // every output off: the stop
)

type supplyCmd struct {
	op    supplyOp
	ch    int    // from 1, as the supply numbers them
	what  string // "v", "i" or "out"
	value float64
	seq   uint32
	reply chan reply // nil for the stop, which nobody waits on
}

// chanState is what a supply reports holding for one channel.
type chanState struct {
	v, i float64
	out  bool
}

type supplyEventKind int

const (
	seConnected supplyEventKind = iota
	seDisconnected
	seState // a channel's setpoints and output, as read back
	seMeas  // a channel's measurements
)

// What caused a channel's state record.
const (
	evRequested    = 1 // a write was admitted: the set fields change
	evApplied      = 2 // the supply's readback after that write
	evReadback     = 3 // a periodic readback that found the supply changed
	evConnected    = 4 // the readback when the supply answered
	evDisconnected = 5 // the supply stopped answering
	evOff          = 6 // the stop switched the output off
)

type supplyEvent struct {
	s     *supply
	kind  supplyEventKind
	t     time.Time
	ch    int
	seq   uint32
	why   uint8
	state chanState
	v, i  float64 // measured
	errs  []string
	idn   string
	err   error
}

var (
	errNotConnected = errors.New("the supply is not connected")
	errBusy         = errors.New("the supply has too many writes waiting; try again")
	errInstrument   = errors.New("the supply stopped answering")
	errAboveLimit   = errors.New("interlock")
)

// run owns the connection until stop is closed.
func (s *supply) run(stop <-chan struct{}) {
	var conn *psu.Supply
	var last []chanState
	var retry <-chan time.Time = time.After(0)
	reported := false // a disconnection is recorded once, not at every retry
	polls := 0
	tick := time.NewTicker(s.poll)
	defer tick.Stop()

	emit := func(e supplyEvent) {
		e.s = s
		if e.t.IsZero() {
			e.t = time.Now()
		}
		select {
		case s.events <- e:
		case <-stop:
		}
	}
	drop := func(err error) {
		if conn != nil {
			conn.Close()
			conn = nil
		}
		if !reported {
			emit(supplyEvent{kind: seDisconnected, err: err})
			reported = true
		}
		retry = time.After(2 * time.Second)
	}
	readAll := func(why uint8, onlyChanged bool) error {
		for ch := 1; ch <= s.channels; ch++ {
			st, err := readState(conn, ch)
			if err != nil {
				return err
			}
			if !onlyChanged || st != last[ch-1] {
				emit(supplyEvent{kind: seState, ch: ch, why: why, state: st})
			}
			last[ch-1] = st
		}
		return nil
	}
	do := func(cmd supplyCmd) {
		var err error
		if conn == nil {
			err = errNotConnected
		} else if err = s.exec(conn, cmd, last, emit); err != nil {
			drop(err)
			err = fmt.Errorf("%w: %v", errInstrument, err)
		}
		if cmd.reply != nil {
			cmd.reply <- reply{seq: cmd.seq, err: err}
		}
	}

	for {
		// The stop before anything else that is waiting.
		select {
		case seq := <-s.off:
			do(supplyCmd{op: opOff, seq: seq})
			continue
		default:
		}
		select {
		case <-stop:
			if conn != nil {
				conn.Close()
			}
			return

		case <-retry:
			retry = nil
			c, err := psu.Dial(s.addr, psu.Keysight, 2*time.Second)
			if err == nil {
				var idn string
				if idn, err = c.Identify(); err == nil {
					conn, reported = c, false
					last = make([]chanState, s.channels)
					emit(supplyEvent{kind: seConnected, idn: idn})
					if err = readAll(evConnected, false); err != nil {
						drop(err)
					}
					continue
				}
				c.Close()
			}
			drop(err)

		case seq := <-s.off:
			do(supplyCmd{op: opOff, seq: seq})

		case cmd := <-s.cmds:
			do(cmd)

		case <-tick.C:
			if conn == nil {
				continue
			}
			polls++
			var err error
			for ch := 1; ch <= s.channels && err == nil; ch++ {
				var v, i float64
				if v, err = conn.MeasureVoltage(ch); err == nil {
					if i, err = conn.MeasureCurrent(ch); err == nil {
						emit(supplyEvent{kind: seMeas, ch: ch, v: v, i: i})
					}
				}
			}
			if err == nil && polls%10 == 0 {
				err = readAll(evReadback, true)
			}
			if err != nil {
				drop(err)
			}
		}
	}
}

// exec carries out one command and reports what the supply holds after it.
// Its error is the connection's; what the supply refused is in its error
// queue, and goes into the recording rather than into the answer (§6).
func (s *supply) exec(conn *psu.Supply, cmd supplyCmd, last []chanState, emit func(supplyEvent)) error {
	chans := []int{cmd.ch}
	if cmd.op == opOff {
		chans = chans[:0]
		for ch := 1; ch <= s.channels; ch++ {
			chans = append(chans, ch)
		}
	}
	for _, ch := range chans {
		var err error
		switch {
		case cmd.op == opOff:
			err = conn.SetOutput(ch, false)
		case cmd.what == "v":
			err = conn.SetVoltage(ch, cmd.value)
		case cmd.what == "i":
			err = conn.SetCurrent(ch, cmd.value)
		case cmd.what == "out":
			err = conn.SetOutput(ch, cmd.value != 0)
		}
		if err != nil {
			return err
		}
		st, err := readState(conn, ch)
		if err != nil {
			return err
		}
		errs, err := conn.Errors()
		if err != nil {
			return err
		}
		last[ch-1] = st
		why := uint8(evApplied)
		if cmd.op == opOff {
			why = evOff
		}
		emit(supplyEvent{kind: seState, ch: ch, why: why, seq: cmd.seq, state: st, errs: errs})
	}
	return nil
}

func readState(conn *psu.Supply, ch int) (chanState, error) {
	var st chanState
	var err error
	if st.v, err = conn.Voltage(ch); err != nil {
		return st, err
	}
	if st.i, err = conn.Current(ch); err != nil {
		return st, err
	}
	st.out, err = conn.Output(ch)
	return st, err
}

// chanLimit bounds what a client may set on one part of one instrument,
// whatever the instrument itself would accept. A limit left out is none: the
// instrument's own rating is then the only bound, and the recording says
// "none".
//
// Each field is named for the quantity it bounds, so that the key the recording
// states — limits.<instrument>.<part>.<quantity>_max — is the JSON key with a
// prefix, and a reader that wants to know what bounded a write does not have to
// know which instrument wrote it.
type chanLimit struct {
	VMax *float64 `json:"v_max"`
	IMax *float64 `json:"i_max"`

	// A scope's. The memory depth is the one setting a client can use to stall
	// the rack: a full-depth readout is tens of megabytes and takes as long as
	// it takes, and every acquisition behind it waits.
	MsizMax *float64 `json:"msiz_max"`
}

// max is the limit on one quantity, nil for none.
func (c chanLimit) max(what string) *float64 {
	switch what {
	case "v":
		return c.VMax
	case "i":
		return c.IMax
	case "msiz":
		return c.MsizMax
	}
	return nil
}

// A channel's name, as the recording and the API have it (§4): the instrument,
// the part of it the setting belongs to, the quantity, and which account —
// psu1.ch2.v.set, scope1.timebase.tdiv.set. Only set is written.
//
// The part is whatever lies between the instrument and the quantity, because
// the instruments do not all divide the same way. A supply's part is a channel,
// ch2. A scope has settings that belong to a channel — scope1.ch1.vert.vdiv —
// and settings that belong to the instrument — scope1.timebase.tdiv — and the
// part is what says which. It is also the key the limits file uses, so a limit
// is stated against the thing it bounds rather than against a quantity's name.
type channelName struct {
	inst string
	part string // "ch2", "ch1.vert", "timebase"
	what string // the quantity: "v", "i", "out", "vdiv", "tdiv", ...
}

func parseChannel(name string) (channelName, error) {
	p := strings.Split(name, ".")
	bad := fmt.Errorf("channel %q: a channel that can be set is named like psu1.ch1.v.set or "+
		"scope1.timebase.tdiv.set: the instrument, the part of it, the quantity, and set", name)
	if len(p) < 4 || p[len(p)-1] != "set" {
		return channelName{}, bad
	}
	c := channelName{inst: p[0], part: strings.Join(p[1:len(p)-2], "."), what: p[len(p)-2]}
	if c.inst == "" || c.part == "" || c.what == "" {
		return channelName{}, bad
	}
	return c, nil
}

// base is the instrument and the part: what a limit is stated against, and the
// stream the set and applied accounts live in.
func (c channelName) base() string { return c.inst + "." + c.part }

func (c channelName) String() string { return c.base() + "." + c.what + ".set" }

// chanOf is the channel number a part names, or 0 when the part is not one:
// "ch2" is 2, "ch1.vert" is 1, "timebase" is 0.
func chanOf(part string) int {
	head, _, _ := strings.Cut(part, ".")
	n, ok := strings.CutPrefix(head, "ch")
	if !ok {
		return 0
	}
	ch, err := strconv.Atoi(n)
	if err != nil || ch < 1 || strconv.Itoa(ch) != n {
		return 0
	}
	return ch
}

// psuChan is one supply channel as the loop records it: what was asked for,
// what the supply holds, and the streams they go in.
type psuChan struct {
	st, meas *teeStream
	set      chanState
	known    [3]bool // whether v, i and out have been set since the daemon started
	applied  chanState
}

// Channel state record layout, in bytes: t 0–7, seq 8–11, event 12, flags 13
// (out.set bit 0, out.applied bit 1, connected bit 2, v.known bit 3, i.known
// bit 4, out.known bit 5), v.set 16–23, i.set 24–31, v.applied 32–39,
// i.applied 40–47, error 48–111.
const (
	psSeq      = 8
	psEvent    = 12
	psFlags    = 13
	psVSet     = 16
	psISet     = 24
	psVApplied = 32
	psIApplied = 40
	psErr      = 48
	psErrLen   = 64
	psBytes    = psErr + psErrLen
)

// Field indices the guards refer to; Schema puts the axis at 0.
const (
	psFieldConnected = 3
	psFieldVKnown    = 4
	psFieldIKnown    = 5
	psFieldOutKnown  = 6
)

// chanSchema is a supply channel's state stream, <supply>.ch<N>. Every field
// but the request's number, the event and the error is held, so a reader that
// joins at any segment learns what was set and what the supply holds.
//
// A setpoint nobody has set since the daemon started is absent, not zero: the
// daemon does not know what was asked of the supply before it. What the supply
// holds is absent while it is not answering.
func chanSchema(supply string, ch int) *logb.Schema {
	name := fmt.Sprintf("%s.ch%d", supply, ch)
	b := func(n string, bit uint32, desc string) logb.Field {
		return logb.Field{Name: n, BitOffset: 8*psFlags + bit, BitWidth: 1, Type: logb.TypeBool, Desc: desc, Hold: true}
	}
	f := func(n string, at uint32, unit, desc string) logb.Field {
		return logb.Field{Name: n, BitOffset: 8 * at, BitWidth: 64, Type: logb.TypeFloat, Unit: unit, Desc: desc, Hold: true}
	}
	guard := func(fd logb.Field, on uint16) logb.Field {
		fd.Guarded, fd.GuardField, fd.GuardValue = true, on, 1
		return fd
	}
	fields := []logb.Field{
		{Name: "seq", BitOffset: 8 * psSeq, BitWidth: 32, Type: logb.TypeUint,
			Desc: "the request this record answers; 0 for what the supply reported unasked"},
		{Name: "event", BitOffset: 8 * psEvent, BitWidth: 8, Type: logb.TypeUint,
			Desc: "1 a write was admitted, 2 the supply's readback after it, 3 a periodic readback found a change, " +
				"4 the supply answered, 5 it stopped answering, 6 the stop switched the output off"},
		b("connected", 2, "the supply is answering"),
		b("v.known", 3, "v.set has been set since the daemon started"),
		b("i.known", 4, "i.set has been set since the daemon started"),
		b("out.known", 5, "out.set has been set since the daemon started"),
		guard(f("v.set", psVSet, "V", "the voltage setpoint a client asked for"), psFieldVKnown),
		guard(f("i.set", psISet, "A", "the current limit a client asked for"), psFieldIKnown),
		guard(b("out.set", 0, "the output state a client asked for"), psFieldOutKnown),
		guard(f("v.applied", psVApplied, "V", "the voltage setpoint the supply reports holding, which it may have clamped"),
			psFieldConnected),
		guard(f("i.applied", psIApplied, "A", "the current limit the supply reports holding"), psFieldConnected),
		guard(b("out.applied", 1, "the output state the supply reports"), psFieldConnected),
		{Name: "error", BitOffset: 8 * psErr, BitWidth: 8 * psErrLen, Type: logb.TypeString,
			Desc: "what the supply's error queue held after a write, or why it stopped answering; empty for none"},
	}
	return record.Schema(name, 8*psBytes, fields, map[string]string{
		"instrument": supply,
		"channel":    strconv.Itoa(ch),
		"source":     "the ktestd supply driver: requests as admitted, then the supply's own SCPI readback",
		"clock":      "userspace: the daemon's clock when it asked or was answered",
	})
}

// measSchema is a channel's measurements, <supply>.ch<N>.meas: the supply's
// reading at its terminals, polled.
func measSchema(supply string, ch int) *logb.Schema {
	name := fmt.Sprintf("%s.ch%d.meas", supply, ch)
	return record.Schema(name, 8*24, []logb.Field{
		{Name: "v", BitOffset: 64, BitWidth: 64, Type: logb.TypeFloat, Unit: "V", Desc: "MEAS:VOLT?"},
		{Name: "i", BitOffset: 128, BitWidth: 64, Type: logb.TypeFloat, Unit: "A", Desc: "MEAS:CURR?"},
	}, map[string]string{
		"instrument": supply,
		"channel":    strconv.Itoa(ch),
		"source":     "the supply's own measurement, polled",
		"clock":      "userspace: the daemon's clock when the answer arrived",
	})
}

// channelSet records an admitted write and hands it to the supply, which
// answers the request itself once the write is done or failed.
func (l *loop) channelSet(in *inst, r *request, t time.Time) reply {
	pc := in.chans[r.ch-1]
	switch r.what {
	case "v":
		pc.set.v, pc.known[0] = r.value, true
	case "i":
		pc.set.i, pc.known[1] = r.value, true
	case "out":
		pc.set.out, pc.known[2] = r.value != 0, true
	}
	l.psuState(in, r.ch, t, l.seq, evRequested, "")
	in.sup.cmds <- supplyCmd{op: opSet, ch: r.ch, what: r.what, value: r.value, seq: l.seq, reply: r.reply}
	return reply{deferred: true}
}

// supplyEvent records what a supply reported.
func (l *loop) supplyEvent(e supplyEvent) {
	in := l.insts[e.s.name]
	switch e.kind {
	case seConnected:
		in.connected = true
		if err := l.w.Meta("psu."+in.name+".idn", e.idn); err != nil {
			fatalf("write: %v", err)
		}
	case seDisconnected:
		in.connected = false
		msg := "not answering"
		if e.err != nil {
			msg = e.err.Error()
		}
		for ch := range in.chans {
			l.psuState(in, ch+1, e.t, 0, evDisconnected, msg)
		}
	case seState:
		in.chans[e.ch-1].applied = e.state
		l.psuState(in, e.ch, e.t, e.seq, e.why, strings.Join(e.errs, "; "))
	case seMeas:
		pc := in.chans[e.ch-1]
		err := l.w.Write(e.t, pc.meas, func(rec []byte) {
			binary.LittleEndian.PutUint64(rec[8:], math.Float64bits(e.v))
			binary.LittleEndian.PutUint64(rec[16:], math.Float64bits(e.i))
		})
		if err != nil {
			fatalf("write: %v", err)
		}
	}
}

// psuState writes a channel's state record.
func (l *loop) psuState(in *inst, ch int, t time.Time, seq uint32, why uint8, msg string) {
	pc := in.chans[ch-1]
	err := l.w.Write(t, pc.st, func(rec []byte) {
		le := binary.LittleEndian
		le.PutUint32(rec[psSeq:], seq)
		rec[psEvent] = why
		var fl byte
		for bit, on := range []bool{pc.set.out, pc.applied.out, in.connected, pc.known[0], pc.known[1], pc.known[2]} {
			if on {
				fl |= 1 << bit
			}
		}
		rec[psFlags] = fl
		le.PutUint64(rec[psVSet:], math.Float64bits(pc.set.v))
		le.PutUint64(rec[psISet:], math.Float64bits(pc.set.i))
		le.PutUint64(rec[psVApplied:], math.Float64bits(pc.applied.v))
		le.PutUint64(rec[psIApplied:], math.Float64bits(pc.applied.i))
		copy(rec[psErr:psErr+psErrLen], truncate(msg, psErrLen))
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

// truncate cuts s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
