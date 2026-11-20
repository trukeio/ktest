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
	"github.com/trukeio/ktest/secondary"
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
// What a mode and an action are — the entry action, the settling, the
// timeout, the stop, the outcome and their records — is package secondary's
// Core, shared with every DUT rack built on it. What is ktestd's is the
// control plane in front of it, the same as its bus's and instruments', and
// what an action is: a keyword of the contract's resource file, run by the
// procedure executor with the DUT library (lib.NewDUT). Its acts — sends,
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

// dutInst is the dut instrument's own state, on the loop: the core, and
// what runs the actions it starts.
type dutInst struct {
	core    *secondary.Core
	c       *monitor.Contract
	suite   *procedure.Suite // the actions' resource, nil when the contract has none
	adapter *dutAdapter
	d       *daemon

	step *teeStream
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
	di := &dutInst{c: c, d: d}
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
	di.d = d
	if di.adapter != nil {
		di.adapter.d, di.adapter.table = d, table
	}
}

// declare lays out the core's streams and the actions' steps, and states
// every monitor unknown and no mode from the start.
func (di *dutInst) declare(w *tee, t time.Time) error {
	var err error
	if di.step, err = w.NewStream(dutStepSchema); err != nil {
		return err
	}
	return di.core.Declare(teeWriter{w}, t)
}

// teeWriter is the daemon's recording as the core's Writer. Only the loop
// writes it, and the core is only called on the loop.
type teeWriter struct{ w *tee }

func (t teeWriter) NewStream(mk func() *logb.Schema) (any, error) { return t.w.NewStream(mk) }
func (t teeWriter) Write(s any, at time.Time, fill func(rec []byte)) error {
	return t.w.Write(at, s.(*teeStream), fill)
}

// dutActions is the core's Actions, given the dut instrument once there is
// one: the core is made first, since the instrument's resource is checked
// against the contract the core loads.
type dutActions struct{ di *dutInst }

func (a *dutActions) Run(ctx context.Context, action string) error { return a.di.Run(ctx, action) }

// Run is secondary.Actions: an action is a keyword of the resource, run by
// the procedure executor, its acts requests of the control plane in the
// name of whoever asked for it. Its timeout and its stop are the core's.
func (di *dutInst) Run(ctx context.Context, action string) error {
	info, _ := secondary.ActionInfoFrom(ctx)
	res := di.suite.Call(ctx, di.c.Actions[action], &dutHost{d: di.d, n: info.Run})
	if res.Outcome == procedure.OK {
		return nil
	}
	msg := res.Msg
	if res.Pos.File != "" && msg != "" {
		msg = res.Pos.String() + ": " + msg
	}
	return &peer.Failure{Outcome: peer.Outcome(res.Outcome), Msg: msg}
}

// dutRequest is a mode change or an action, admitted: on the loop, after the
// decision is recorded, carried out by the core.
func (l *loop) dutRequest(in *inst, r *request, t time.Time) reply {
	by := secondary.By{Name: r.who.name, Via: r.who.via}
	if r.kind == reqDutMode {
		in.dut.core.SetMode(l.seq, by, r.name, t)
	} else {
		in.dut.core.Invoke(l.seq, by, r.name, t)
	}
	if err := in.dut.core.Err(); err != nil {
		fatalf("write: %v", err)
	}
	return reply{seq: l.seq}
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
func (di *dutInst) stopAction(why string) { di.core.Stop(why) }

// dutHost is what an action sees of the daemon.
type dutHost struct {
	d *daemon
	n uint32 // the action run
}

func (h *dutHost) Resolve(name string) (expr.Type, error) { return h.d.procs.table.Resolve(name) }
func (h *dutHost) Value(name string) (expr.Value, int64, bool) {
	return h.d.procs.table.Value(name)
}
func (h *dutHost) Step(ev procedure.StepEvent) {
	n := h.n
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
	info, ok := secondary.ActionInfoFrom(ctx)
	if !ok {
		return procedure.Failf(procedure.Fail, "no action is under way")
	}
	req.internal = true
	req.who = caller{name: info.By.Name, role: roleOperate, via: record.ViaAction}
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

// dutContract is GET /dut/contract: which contract this DUT rack runs, and
// the rack's provenance, for a master to compare with its own copy before it
// relies on anything the rack says (handoff §4.6).
func (d *daemon) dutContract(w http.ResponseWriter, r *http.Request) {
	if !d.dutOn(w) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d.dutCore.Info(d.provenance))
}

// clock is GET /clock: the daemon's clock, in Unix nanoseconds, read as late
// as it can be, for a peer estimating the offset between the racks' clocks.
func clock(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "{\"t\":%d}\n", time.Now().UnixNano())
}
