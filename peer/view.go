package peer

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/logb"
)

// The monitor states (doc/peer-protocol.md §5.1), by their protocol codes.
const (
	StateOK       = "ok"
	StateDev      = "dev"
	StateUnknown  = "unknown"
	StateInactive = "inactive"
	StateSettling = "settling"
)

var stateCodes = []string{StateOK, StateDev, StateUnknown, StateInactive, StateSettling}

// The events of the mode stream (§7.4).
const (
	ModeStart     = 0
	ModeRequested = 1
	ModeApplied   = 2
	ModeFailed    = 3
)

// View is what a master knows of a DUT rack, from what it streamed: each
// monitor's states on the DUT rack's clock, its watermark, the mode as
// applied, the actions' ends, the link and the clocks' offset.
//
// It is fed with Apply, or with Observe, SetLink and SetClock, from one
// goroutine — the one that also records what arrives — and read from any:
// a reader waiting on it is woken by every change.
type View struct {
	mu      sync.Mutex
	changed chan struct{}

	connected bool
	offset    time.Duration // the DUT rack's clock less this one
	unc       time.Duration // its uncertainty
	haveOff   bool
	wm        int64     // the DUT rack's watermark, its clock; 0 for none
	wmRecv    time.Time // when it arrived, this rack's clock

	hist map[string][]stamp // each monitor's states, oldest first, the DUT rack's clock

	mode      string // as applied
	modeT     int64  // when, the DUT rack's clock
	modeSeq   uint32 // the request the last mode record answered
	modeEvent byte
	modeMsg   string
	actions   map[uint32]actionEnd
}

type stamp struct {
	t     int64
	state string
}

type actionEnd struct {
	outcome Outcome
	msg     string
}

// HistMax bounds each monitor's history: a day of a monitor changing every
// second is more than a procedure looks back over, and less than a megabyte.
const HistMax = 100_000

// NewView makes an empty view: not connected, every monitor unknown.
func NewView() *View {
	return &View{changed: make(chan struct{}), hist: map[string][]stamp{}, actions: map[uint32]actionEnd{}}
}

// update changes the view under its lock and wakes every waiter.
func (v *View) update(f func()) {
	v.mu.Lock()
	f()
	close(v.changed)
	v.changed = make(chan struct{})
	v.mu.Unlock()
}

// Apply takes an event a Link handed over.
func (v *View) Apply(e Event) {
	switch {
	case e.Link != nil:
		switch e.Link.Kind {
		case LinkUp:
			v.SetLink(true)
		case LinkDown, LinkRefused:
			v.SetLink(false)
		}
	case e.Clock != nil:
		v.SetClock(*e.Clock)
	case e.Item != nil:
		v.Observe(e.Item)
	}
}

// Observe takes what a streamed record says.
func (v *View) Observe(it *Item) {
	name := it.Schema.Name
	switch {
	case name == "mon.watermark":
		v.update(func() {
			if it.PeerT > v.wm {
				v.wm, v.wmRecv = it.PeerT, it.Recv
			}
		})
	case name == "mon.gap":
	case strings.HasPrefix(name, "mon."):
		st := stateOf(it)
		m := strings.TrimPrefix(name, "mon.")
		v.update(func() {
			h := v.hist[m]
			if n := len(h); n > 0 && h[n-1].state == st && h[n-1].t <= it.PeerT {
				return // a restatement of what is already known
			}
			h = append(h, stamp{it.PeerT, st})
			if len(h) > HistMax {
				h = h[len(h)-HistMax:]
			}
			v.hist[m] = h
		})
	case name == "mode":
		ev := byte(Num(it, "event"))
		applied := Str(it, "applied")
		seq := uint32(Num(it, "seq"))
		msg := Str(it, "message")
		v.update(func() {
			if ev == ModeApplied || it.Restated {
				v.mode, v.modeT = applied, it.PeerT
			}
			v.modeSeq, v.modeEvent, v.modeMsg = seq, ev, msg
		})
	case strings.HasPrefix(name, "dut.action."):
		if Num(it, "event") != 2 {
			return
		}
		seq := uint32(Num(it, "seq"))
		o := OutcomeOf(int(Num(it, "outcome")))
		msg := Str(it, "message")
		v.update(func() { v.actions[seq] = actionEnd{o, msg} })
	}
}

// stateOf is a monitor record's state: the text its schema gives the code,
// or the protocol's name for it if the schema gives none.
func stateOf(it *Item) string {
	f := Field(it.Schema, "state")
	if f < 0 {
		return StateUnknown
	}
	b := &logb.Batch{Schema: it.Schema, Count: 1, Data: it.Rec}
	switch x := mustValue(b, f).(type) {
	case string:
		return x
	case uint64:
		if int(x) < len(stateCodes) {
			return stateCodes[x]
		}
	case int64:
		if x >= 0 && int(x) < len(stateCodes) {
			return stateCodes[x]
		}
	case float64:
		if x >= 0 && int(x) < len(stateCodes) && x == float64(int(x)) {
			return stateCodes[int(x)]
		}
	}
	return StateUnknown
}

// SetLink says whether the DUT rack is connected. A link lost forgets the
// watermark: nothing is vouched for until the next one.
func (v *View) SetLink(up bool) {
	v.update(func() {
		v.connected = up
		if !up {
			v.wm, v.wmRecv = 0, time.Time{}
		}
	})
}

// SetClock takes a new estimate of the clocks' offset.
func (v *View) SetClock(e Estimate) {
	v.update(func() { v.offset, v.unc, v.haveOff = e.Offset, e.Uncertainty, true })
}

// Connected says whether the DUT rack is connected.
func (v *View) Connected() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.connected
}

// Watermark is the DUT rack's latest watermark, its clock, Unix ns, and when
// it arrived by this one; 0 if there is none since the link came up.
func (v *View) Watermark() (int64, time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.wm, v.wmRecv
}

// Offset is the clocks' offset, the DUT rack's less this one, and its
// uncertainty; ok is false until there is an estimate.
func (v *View) Offset() (offset, uncertainty time.Duration, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.offset, v.unc, v.haveOff
}

// Mode is the mode the DUT rack last applied, and when, its clock.
func (v *View) Mode() (string, int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.mode, v.modeT
}

// StateAt is a monitor's state at t, the DUT rack's clock: unknown before
// anything is known of it.
func (v *View) StateAt(monitor string, t int64) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stateAt(monitor, t)
}

// Latest is a monitor's latest state, whether or not the watermark has
// passed it: what a protection rule reacts to (§9.4).
func (v *View) Latest(monitor string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	h := v.hist[monitor]
	if len(h) == 0 {
		return StateUnknown
	}
	return h[len(h)-1].state
}

// States are the monitors' states at the watermark: final, as far as the
// DUT rack has vouched for them.
func (v *View) States(monitors []string) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := map[string]string{}
	for _, m := range monitors {
		out[m] = v.stateAt(m, v.wm)
	}
	return out
}

// Deviated says which monitors were dev or unknown at any instant of
// [p0, p1], the DUT rack's clock, the state held at p0 included, with the
// state found. It does not wait for the watermark: DUT.Deviations does.
func (v *View) Deviated(monitors []string, p0, p1 int64) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.deviated(monitors, p0, p1)
}

func (v *View) deviated(monitors []string, p0, p1 int64) map[string]string {
	dev := map[string]string{}
	bad := func(s string) bool { return s == StateDev || s == StateUnknown }
	for _, m := range monitors {
		if s := v.stateAt(m, p0); bad(s) {
			dev[m] = s
			continue
		}
		for _, x := range v.hist[m] {
			if x.t > p0 && x.t <= p1 && bad(x.state) {
				dev[m] = x.state
				break
			}
		}
	}
	return dev
}

// wait calls done under the view's lock until it says so, the deadline
// passes, ctx ends, or the link is down, which is link_lost.
func (v *View) wait(ctx context.Context, deadline time.Time, what string, done func() bool) error {
	within := time.Until(deadline)
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		v.mu.Lock()
		if !v.connected {
			v.mu.Unlock()
			return failf(LinkLost, "the DUT rack is not connected, waiting for %s", what)
		}
		if done() {
			v.mu.Unlock()
			return nil
		}
		ch := v.changed
		v.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			return failf(Timeout, "%s did not happen within %v", what, within.Round(time.Millisecond))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ToPeer is a time of this rack's clock on the DUT rack's, Unix ns, with the
// offset's uncertainty, which is 0 until there is an estimate.
func (v *View) ToPeer(t time.Time) (int64, time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.toPeer(t)
}

func (v *View) toPeer(t time.Time) (int64, time.Duration) {
	if !v.haveOff {
		return t.UnixNano(), 0
	}
	return t.UnixNano() + int64(v.offset), v.unc
}

// stateAt is a monitor's last stamp at or before t. Called with the lock.
func (v *View) stateAt(m string, t int64) string {
	h := v.hist[m]
	i, _ := slices.BinarySearchFunc(h, t, func(s stamp, t int64) int {
		if s.t <= t {
			return -1
		}
		return 1
	})
	if i == 0 {
		return StateUnknown
	}
	return h[i-1].state
}

// Field is the index of a schema's field of that name, -1 if it has none.
func Field(s *logb.Schema, name string) int {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			return i
		}
	}
	return -1
}

// Num is a record's numeric field by name, as its schema reads it; 0 if it
// has no such field, or the field is not a number.
func Num(it *Item, name string) float64 {
	f := Field(it.Schema, name)
	if f < 0 {
		return 0
	}
	b := &logb.Batch{Schema: it.Schema, Count: 1, Data: it.Rec}
	switch x := mustValue(b, f).(type) {
	case uint64:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

// Str is a record's text field by name; "" if it has none.
func Str(it *Item, name string) string {
	f := Field(it.Schema, name)
	if f < 0 {
		return ""
	}
	b := &logb.Batch{Schema: it.Schema, Count: 1, Data: it.Rec}
	s, _ := mustValue(b, f).(string)
	return s
}

func mustValue(b *logb.Batch, f int) any {
	v, err := b.Value(0, f)
	if err != nil {
		return nil
	}
	return v
}
