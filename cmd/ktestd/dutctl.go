//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trukeio/logb"
	"github.com/trukeio/logb/dbc"
	"golang.org/x/sys/unix"

	"github.com/trukeio/ktest/can"
	"github.com/trukeio/ktest/channel"
	"github.com/trukeio/ktest/expr"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/procedure"
	"github.com/trukeio/ktest/procedure/lib"
	"github.com/trukeio/ktest/record"
)

// # Modes and actions
//
// A DUT rack operates the DUT itself (decision 9). The master changes its mode
// and asks for its actions, through an instrument of its own, dut, leased and
// armed like any other (handoff §4.5):
//
//	POST   /dut/lease, PUT /dut/lease, DELETE /dut/lease
//	POST   /dut/arm, POST /dut/disarm
//	PUT    /dut/mode            {"value": "active"}: run the mode's entry action, then apply it
//	POST   /dut/actions/{name}  run an action
//
// An action is a keyword of the contract's resource file, run by the procedure
// executor with the DUT library (lib.NewDUT), one at a time. Its acts — sends,
// cyclic tasks, channel writes — are requests of the control plane like any
// client's, recorded in the stream of the instrument they act on with via 4
// and the name that asked for the action. They are admitted while dut is
// armed, not under that instrument's lease; limits and the cyclic floor apply.
//
// The DUT goes on running when its master goes away. A lapsed or released dut
// lease leaves dut armed and its cyclic tasks running; it only means nobody
// may change the mode or start an action until someone leases it again. A
// lapsed bus lease stops the tasks its holder started and leaves the DUT
// rack's. A disarm of dut, or the stop, ends the action under way and every
// task the DUT rack holds.
//
// Recorded:
//
//	mode              requested and applied, held; each request, application and failure
//	dut.action.<name> each run of an action: its start, and its end with the outcome
//	dut.step          every keyword call inside an action, as procedure.step has a run's

var errDutBusy = errors.New("the DUT is busy")

// dutInst is the dut instrument's own state, on the loop.
type dutInst struct {
	c       *monitor.Contract
	suite   *procedure.Suite // the actions' resource, nil when the contract has none
	adapter *dutAdapter

	mode    *teeStream
	actions map[string]*teeStream
	step    *teeStream

	requested, applied string
	run                *dutRun // the action under way, nil when none
	n                  uint32  // actions started since the daemon did
}

// dutRun is one action under way: an action asked for, or a mode's entry.
type dutRun struct {
	n      uint32
	action string
	mode   *string // the mode it enters, nil for a plain action
	seq    uint32
	who    caller
	cancel context.CancelFunc

	// why it was stopped, when it was: set on the loop before cancel, read
	// after the action's goroutine has seen the cancel.
	stopped atomic.Pointer[string]
}

// The layouts. mode: t 0–7, seq 8–11, event 12, requested 16–47, applied
// 48–79, by 80–111, message 112–271. dut.action.<name>: t 0–7, seq 8–11,
// run 12–15, event 16, outcome 17, via 18, by 24–55, mode 56–87,
// message 88–247.
const (
	mdSeq       = 8
	mdEvent     = 12
	mdRequested = 16
	mdApplied   = 48
	mdBy        = 80
	mdMessage   = 112
	mdBytes     = mdMessage + 160

	acSeq     = 8
	acRun     = 12
	acEvent   = 16
	acOutcome = 17
	acVia     = 18
	acBy      = 24
	acMode    = 56
	acMessage = 88
	acBytes   = acMessage + 160
)

// The events of mode.
const (
	modeRequested = 1
	modeApplied   = 2
	modeFailed    = 3
)

func modeSchema() *logb.Schema {
	return record.Schema("mode", 8*mdBytes, []logb.Field{
		uintF("seq", mdSeq, 32, "", "the request; 0 for the start"),
		uintF("event", mdEvent, 8, "", "1 a mode was asked for, 2 it was applied, 3 its entry action failed; 0 the start"),
		holdF(strF("requested", mdRequested, 32, "the mode last asked for; empty for none")),
		holdF(strF("applied", mdApplied, 32, "the mode the DUT is in, from the end of its entry action on; empty for none")),
		strF("by", mdBy, record.HolderMax, "the name that asked"),
		strF("message", mdMessage, 160, "why an entry action failed"),
	}, map[string]string{
		"source": "the ktestd DUT rack",
		"clock":  "userspace: when it was asked, or when the entry action ended; the monitors settle from the latter",
	})
}

func actionSchema(name, keyword string) *logb.Schema {
	return record.Schema("dut.action."+name, 8*acBytes, []logb.Field{
		uintF("seq", acSeq, 32, "", "the request that started it"),
		uintF("run", acRun, 32, "", "the action run, numbered from 1 since the daemon started, across actions"),
		uintF("event", acEvent, 8, "", "1 it started, 2 it ended"),
		uintF("outcome", acOutcome, 8, "", "at its end: "+outcomeDesc),
		uintF("via", acVia, 8, "", "the listener the request came through: 1 the local socket, 2 TLS"),
		strF("by", acBy, record.HolderMax, "the name that asked"),
		strF("mode", acMode, 32, "the mode it is the entry action of; empty for an action asked for"),
		strF("message", acMessage, 160, "at its end: why it failed"),
	}, map[string]string{
		"source":  "the ktestd DUT rack",
		"keyword": keyword,
		"clock":   "userspace: when it started and ended",
	})
}

func dutStepSchema() *logb.Schema {
	s := procStepSchema()
	s.Name = "dut.step"
	s.UUID = record.Schema("dut.step", s.RecordBits, nil, nil).UUID
	for i := range s.Fields {
		if s.Fields[i].Name == "run" {
			s.Fields[i].Desc = "the action run the call belongs to"
		}
	}
	s.Meta = map[string]string{"source": "the ktestd DUT rack's actions", "clock": "userspace: when the call started or ended"}
	return s
}

// newDutInst loads the contract's actions. The resource is checked whole with
// the DUT library, against this daemon's database, channels and monitors, and
// every action must be a keyword callable with no arguments: a contract that
// would fail on the day an action is first asked for refuses the start.
func newDutInst(d *daemon, c *monitor.Contract, table *channel.Table) (*dutInst, error) {
	di := &dutInst{c: c}
	if c.Resource == "" {
		return di, nil
	}
	di.adapter = &dutAdapter{d: d, c: c, table: table}
	lb := lib.NewDUT(di.adapter)
	s, err := procedure.LoadResource(c.ResourcePath(), procedure.Env{Lib: lb, Channels: table})
	if err != nil {
		return nil, fmt.Errorf("resource: %v", err)
	}
	for _, a := range slices.Sorted(maps.Keys(c.Actions)) {
		if err := s.Bare(c.Actions[a]); err != nil {
			return nil, fmt.Errorf("action %s: %v", a, err)
		}
	}
	di.suite = s
	return di, nil
}

// bind gives the actions the daemon they run in, and its live channels, once
// there is one: the resource was checked before, against a stand-in with this
// daemon's configuration, so that a bad one refused the start before anything
// was written.
func (di *dutInst) bind(d *daemon, table *channel.Table) {
	if di.adapter != nil {
		di.adapter.d, di.adapter.table = d, table
	}
}

// declare lays out the DUT rack's streams, and states no mode from the start.
func (di *dutInst) declare(w *tee, t time.Time) error {
	var err error
	if di.mode, err = w.NewStream(modeSchema); err != nil {
		return err
	}
	if di.step, err = w.NewStream(dutStepSchema); err != nil {
		return err
	}
	di.actions = map[string]*teeStream{}
	for _, a := range slices.Sorted(maps.Keys(di.c.Actions)) {
		kw := di.c.Actions[a]
		if di.actions[a], err = w.NewStream(func() *logb.Schema { return actionSchema(a, kw) }); err != nil {
			return err
		}
	}
	return di.writeMode(w, t, 0, 0, caller{}, "")
}

func (di *dutInst) writeMode(w *tee, t time.Time, seq uint32, ev byte, who caller, msg string) error {
	return w.Write(t, di.mode, func(rec []byte) {
		putUint32(rec[mdSeq:], seq)
		rec[mdEvent] = ev
		putStr(rec, mdRequested, 32, di.requested)
		putStr(rec, mdApplied, 32, di.applied)
		putStr(rec, mdBy, record.HolderMax, who.name)
		putStr(rec, mdMessage, 160, msg)
	})
}

func (di *dutInst) writeAction(w *tee, t time.Time, run *dutRun, ev byte, o procedure.Outcome, msg string) error {
	return w.Write(t, di.actions[run.action], func(rec []byte) {
		putUint32(rec[acSeq:], run.seq)
		putUint32(rec[acRun:], run.n)
		rec[acEvent] = ev
		rec[acOutcome] = outcomeCode(o)
		rec[acVia] = byte(run.who.via)
		putStr(rec, acBy, record.HolderMax, run.who.name)
		if run.mode != nil {
			putStr(rec, acMode, 32, *run.mode)
		}
		putStr(rec, acMessage, 160, msg)
	})
}

// dutRequest is a mode change or an action, admitted: on the loop, after the
// decision is recorded.
func (l *loop) dutRequest(in *inst, r *request, t time.Time) reply {
	di := in.dut
	if r.kind == reqDutMode {
		di.requested = r.name
		if err := di.writeMode(l.w, t, l.seq, modeRequested, r.who, ""); err != nil {
			fatalf("write: %v", err)
		}
		action := ""
		if m := di.c.Modes[r.name]; m != nil {
			action = m.Action
		}
		if action == "" {
			l.applyMode(di, r.name, l.seq, r.who, t)
			return reply{seq: l.seq}
		}
		mode := r.name
		l.startAction(di, &dutRun{action: action, mode: &mode, seq: l.seq, who: r.who}, t)
		return reply{seq: l.seq}
	}
	l.startAction(di, &dutRun{action: r.name, seq: l.seq, who: r.who}, t)
	return reply{seq: l.seq}
}

// applyMode puts the DUT in a mode from t: recorded, and handed to the
// monitors, which settle from t for the mode's settle time.
func (l *loop) applyMode(di *dutInst, mode string, seq uint32, who caller, t time.Time) {
	// The wall clock alone: the recorder measures a time with a monotonic
	// reading from its epoch by that reading, and the monitors take Unix
	// nanoseconds, and the two differ by the clocks' drift since the start
	// (measured: 9 ns after 2 s). Without it both say the same instant.
	t = t.Round(0)
	di.applied = mode
	if err := di.writeMode(l.w, t, seq, modeApplied, who, ""); err != nil {
		fatalf("write: %v", err)
	}
	if l.mon != nil {
		l.mon.tap.Mode(l.mon.tap.Clock(), t, mode)
	}
}

func (l *loop) startAction(di *dutInst, run *dutRun, t time.Time) {
	di.n++
	run.n = di.n
	timeout := 30 * time.Second
	if d, ok := di.c.Timeouts[run.action]; ok && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(l.d.dutHard, timeout)
	run.cancel = cancel
	di.run = run
	l.d.dutCur.Store(run)
	if err := di.writeAction(l.w, t, run, 1, "", ""); err != nil {
		fatalf("write: %v", err)
	}
	go l.d.execute(ctx, di, run, timeout)
}

// execute runs an action to its end on a goroutine of its own, and hands the
// end back to the loop.
func (d *daemon) execute(ctx context.Context, di *dutInst, run *dutRun, timeout time.Duration) {
	res := di.suite.Call(ctx, di.c.Actions[run.action], &dutHost{d: d, run: run})
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Outcome, res.Msg, res.Pos = procedure.Timeout,
			fmt.Sprintf("action %s did not end within its timeout, %v", run.action, timeout), procedure.Pos{}
	case run.stopped.Load() != nil:
		res.Outcome, res.Msg, res.Pos = procedure.Stopped, *run.stopped.Load(), procedure.Pos{}
	}
	run.cancel()
	d.dutJob(func(l *loop) {
		t := time.Now()
		if di.run == run {
			di.run = nil
			d.dutCur.Store(nil)
		}
		msg := res.Msg
		if res.Pos.File != "" && msg != "" {
			msg = res.Pos.String() + ": " + msg
		}
		if err := di.writeAction(l.w, t, run, 2, res.Outcome, msg); err != nil {
			fatalf("write: %v", err)
		}
		if run.mode == nil {
			return
		}
		if res.Outcome == procedure.OK {
			l.applyMode(di, *run.mode, run.seq, run.who, t)
			return
		}
		if err := di.writeMode(l.w, t, run.seq, modeFailed, run.who,
			fmt.Sprintf("entry action %s: %s: %s", run.action, res.Outcome, msg)); err != nil {
			fatalf("write: %v", err)
		}
	})
}

// dutJob runs f on the loop, unless the loop has stopped taking work.
func (d *daemon) dutJob(f func(l *loop)) {
	select {
	case d.jobs <- f:
	case <-d.closing:
	}
}

// stopAction ends the action under way, as a disarm of dut, the stop or the
// daemon stopping does, saying which. Its end is recorded when its goroutine
// has finished.
func (di *dutInst) stopAction(why string) {
	if di.run != nil {
		di.run.stopped.Store(&why)
		di.run.cancel()
	}
}

// dutHost is what an action sees of the daemon.
type dutHost struct {
	d   *daemon
	run *dutRun
}

func (h *dutHost) Resolve(name string) (expr.Type, error) { return h.d.procs.table.Resolve(name) }
func (h *dutHost) Value(name string) (expr.Value, int64, bool) {
	return h.d.procs.table.Value(name)
}
func (h *dutHost) Step(ev procedure.StepEvent) {
	n := h.run.n
	h.d.dutJob(func(l *loop) {
		if di := l.insts[dutName].dut; di != nil {
			recordStep(l, di.step, n, ev)
		}
	})
}

// dutName is the DUT rack's instrument.
const dutName = "dut"

// dutAdapter is lib.DUT over the daemon: what an action's keywords do.
type dutAdapter struct {
	d     *daemon
	c     *monitor.Contract
	table *channel.Table
}

func (a *dutAdapter) message(name string) (*dbc.Message, error) {
	if a.d.db == nil {
		return nil, fmt.Errorf("message %s: this rack has no database; start it with -dbc", name)
	}
	m := a.d.db.Message(name)
	if m == nil {
		return nil, fmt.Errorf("the database has no message %q", name)
	}
	return m, nil
}

// signals reads the values an action gave in each signal's own unit: a
// number with a unit is converted when the signal's unit is one the
// expression language knows and has its dimension; a bare number, or one
// written in the signal's own unit (3000rpm for rpm), is taken as written; a
// name is looked up in the signal's value table by the encoder.
func signals(m *dbc.Message, sig map[string]procedure.Value) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range sig {
		var s *dbc.Signal
		for _, x := range m.Signals {
			if x.Name == k {
				s = x
			}
		}
		if s == nil {
			return nil, fmt.Errorf("message %s has no signal %q", m.Name, k)
		}
		switch v.Kind {
		case procedure.VBool:
			out[k] = 0.0
			if v.Bool {
				out[k] = 1.0
			}
			continue
		case procedure.VNumber:
		case procedure.VString:
			text := strings.TrimSpace(v.Str)
			if f, err := strconv.ParseFloat(strings.TrimSuffix(text, s.Unit), 64); err == nil && (s.Unit != "" || text == strings.TrimSuffix(text, s.Unit)) {
				out[k] = f
				continue
			}
			f, u, err := expr.ParseQuantity(text)
			if err != nil {
				out[k] = text // a name from the signal's value table, or the encoder says why not
				continue
			}
			v = procedure.Number(f, u)
		default:
			return nil, fmt.Errorf("signal %s: %s is not a value", k, v.String())
		}
		if v.Unit.Name == "" {
			out[k] = v.Num
			continue
		}
		su := expr.ParseUnit(s.Unit)
		if s.Unit == "" || !su.SameDimension(v.Unit) || su.Scale == 0 {
			return nil, fmt.Errorf("signal %s is in %q, and %s is not", k, s.Unit, v.String())
		}
		out[k] = v.Num / su.Scale
	}
	return out, nil
}

func (a *dutAdapter) Message(name string, sig map[string]procedure.Value) error {
	m, err := a.message(name)
	if err != nil || sig == nil {
		return err
	}
	vals, err := signals(m, sig)
	if err != nil {
		return err
	}
	_, err = (&sendBody{Message: name, Signals: vals}).fromSignals(a.d.db)
	return err
}

func (a *dutAdapter) frame(name string, sig map[string]procedure.Value) (*can.Frame, error) {
	m, err := a.message(name)
	if err != nil {
		return nil, err
	}
	vals, err := signals(m, sig)
	if err != nil {
		return nil, err
	}
	return (&sendBody{Message: name, Signals: vals}).fromSignals(a.d.db)
}

func (a *dutAdapter) Send(ctx context.Context, message string, sig map[string]procedure.Value) error {
	f, err := a.frame(message, sig)
	if err != nil {
		return procedure.Failf(procedure.Fail, "%v", err)
	}
	return a.ask(ctx, &request{kind: reqSend, inst: a.d.bus, f: *f}, message)
}

func (a *dutAdapter) Cyclic(ctx context.Context, message string, sig map[string]procedure.Value, period time.Duration) error {
	f, err := a.frame(message, sig)
	if err != nil {
		return procedure.Failf(procedure.Fail, "%v", err)
	}
	return a.ask(ctx, &request{kind: reqCyclicSet, inst: a.d.bus, f: *f, period: period}, message)
}

func (a *dutAdapter) StopCyclic(ctx context.Context, message string) error {
	m, err := a.message(message)
	if err != nil {
		return procedure.Failf(procedure.Fail, "%v", err)
	}
	id := m.ID
	if m.Extended {
		id |= unix.CAN_EFF_FLAG
	}
	return a.ask(ctx, &request{kind: reqCyclicDelete, inst: a.d.bus, f: can.Frame{ID: id}}, message)
}

func (a *dutAdapter) Settable(name string) error {
	cn, err := parseChannel(name)
	if err != nil {
		return err
	}
	if !a.d.hasChannel(cn) {
		return fmt.Errorf("no channel %s here", cn)
	}
	return nil
}

func (a *dutAdapter) Set(ctx context.Context, name string, value float64) error {
	cn, err := parseChannel(name)
	if err != nil {
		return procedure.Failf(procedure.Fail, "%v", err)
	}
	return a.ask(ctx, &request{kind: reqChannelSet, inst: cn.inst, part: cn.part, ch: chanOf(cn.part),
		what: cn.what, value: value}, name)
}

func (a *dutAdapter) Monitor(name string) error {
	if a.c.Monitors[name] == nil {
		return fmt.Errorf("the contract has no monitor %q; its monitors are %s", name,
			strings.Join(a.c.Order, ", "))
	}
	return nil
}

func (a *dutAdapter) MonitorState(name string) (string, bool) {
	v, _, ok := a.table.Value("mon." + name + ".state")
	return v.Str, ok && v.Known
}

// ask makes a request of the control plane for the action under way, and
// waits for its answer.
func (a *dutAdapter) ask(ctx context.Context, req *request, what string) error {
	run := a.d.dutCur.Load()
	if run == nil {
		return procedure.Failf(procedure.Fail, "no action is under way")
	}
	req.internal = true
	req.who = caller{name: run.who.name, role: roleOperate, via: record.ViaAction}
	req.reply = make(chan reply, 1)
	select {
	case a.d.reqs <- req:
	case <-a.d.closing:
		return procedure.Failf(procedure.Refused, "the daemon is stopping")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case rep := <-req.reply:
		if rep.err != nil {
			return procedure.Failf(procedure.Refused, "%s: %v", what, rep.err)
		}
		return nil
	case <-a.d.gone:
		return procedure.Failf(procedure.Refused, "the daemon stopped before the instrument answered")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The HTTP side.

type modeBody struct {
	Value *string `json:"value"`
}

// dutMode is PUT /dut/mode.
func (d *daemon) dutMode(w http.ResponseWriter, r *http.Request) {
	if !d.dutOn(w) {
		return
	}
	var body modeBody
	if !decode(w, r, &body) {
		return
	}
	switch {
	case body.Value == nil:
		httpError(w, http.StatusBadRequest, 0, `"value" is required: the mode, or "" for none`)
		return
	case *body.Value != "" && d.contract.Modes[*body.Value] == nil:
		httpError(w, http.StatusNotFound, 0, "the contract has no mode %q; its modes are %s", *body.Value,
			strings.Join(slices.Sorted(maps.Keys(d.contract.Modes)), ", "))
		return
	}
	d.submit(w, r, &request{kind: reqDutMode, inst: dutName, name: *body.Value})
}

// dutAction is POST /dut/actions/{action}.
func (d *daemon) dutAction(w http.ResponseWriter, r *http.Request) {
	if !d.dutOn(w) {
		return
	}
	name := r.PathValue("action")
	if _, ok := d.contract.Actions[name]; !ok {
		httpError(w, http.StatusNotFound, 0, "the contract has no action %q; its actions are %s", name,
			strings.Join(slices.Sorted(maps.Keys(d.contract.Actions)), ", "))
		return
	}
	d.submit(w, r, &request{kind: reqDutAction, inst: dutName, name: name})
}

func (d *daemon) dutOn(w http.ResponseWriter) bool {
	if d.contract == nil {
		httpError(w, http.StatusNotFound, 0, "this daemon is no DUT rack; start it with -contract dut-interface.json")
		return false
	}
	return true
}

// dutState guards what the HTTP side and an action's goroutine read of the
// DUT rack outside the loop.
type dutState struct {
	mu  sync.Mutex
	cur *dutRun
}

func (s *dutState) Load() *dutRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *dutState) Store(r *dutRun) {
	s.mu.Lock()
	s.cur = r
	s.mu.Unlock()
}

// dutContract is GET /dut/contract: which contract this DUT rack runs, and
// the rack's provenance, for a master to compare with its own copy before it
// relies on anything the rack says (handoff §4.6).
func (d *daemon) dutContract(w http.ResponseWriter, r *http.Request) {
	if !d.dutOn(w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"protocol":        peer.Protocol,
		"contract":        d.contract.Name,
		"sha256":          d.contract.Hash,
		"resource_sha256": d.contract.ResourceHash,
		"provenance":      d.provenance,
	})
}

// clock is GET /clock: the daemon's clock, in Unix nanoseconds, read as late
// as it can be, for a peer estimating the offset between the racks' clocks.
func clock(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "{\"t\":%d}\n", time.Now().UnixNano())
}
