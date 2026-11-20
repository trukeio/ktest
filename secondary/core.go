package secondary

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
)

// # The core of a DUT rack
//
// Core is what makes a program a DUT rack, apart from how it is reached:
// the contract, loaded against the host's channels; the monitors, evaluated
// live with the reorder window, the watermark and gaps (doc/peer-protocol.md
// §5); and the DUT's modes and actions (§6.5, §6.6): a mode's entry action,
// its settling, an action's timeout, its stop and its outcome; and every
// record of them, laid out as §7.4 says.
//
// What it leaves to its host is the control plane — who may ask, the lease,
// arming, numbering requests and recording the decisions — and the
// recording itself. Server is a host with a small control plane of its own;
// ktestd -contract is a host whose control plane is the one its bus and
// instruments share.
//
// Core is not safe for concurrent use. Its host calls it from one goroutine
// at a time, and gives it Do, which runs a function there: an action ends on
// a goroutine of its own, and hands its end back through Do.

// Writer is where Core's records go: the host's recording. A stream is
// whatever handle the host gives it.
type Writer interface {
	NewStream(mk func() *logb.Schema) (any, error)
	Write(stream any, t time.Time, fill func(rec []byte)) error
}

// By is who asked for a mode or an action, as the records say it.
type By struct {
	Name string
	Via  record.Via
}

// ActionInfo is what an action is told of itself: the run, numbered from 1
// across actions since the core started; the request that started it; who
// asked; and, for a mode's entry action, the mode.
type ActionInfo struct {
	Run    uint32
	Seq    uint32
	By     By
	Action string
	Mode   string
}

type actionInfoKey struct{}

// ActionInfoFrom is the ActionInfo of the action whose context ctx is.
func ActionInfoFrom(ctx context.Context) (ActionInfo, bool) {
	a, ok := ctx.Value(actionInfoKey{}).(ActionInfo)
	return a, ok
}

// CoreConfig is what a DUT rack's core is made of.
type CoreConfig struct {
	// Contract is the path of dut-interface.json; Channels resolves its
	// inputs to the host's streams; Check, if set, holds its actions to
	// their resource.
	Contract string
	Channels monitor.Channels
	Check    monitor.ActionCheck
	// Actions runs the contract's actions. Required if it declares any.
	Actions Actions
	// Source is what the mode and action streams' schemas say wrote them,
	// and MonitorSource the monitor streams'; "" is "the DUT rack", and
	// Source.
	Source, MonitorSource string
	// Window is how long a record is held for earlier ones before the
	// monitors see it, as given: 0 is no wait. WatermarkEvery is how often
	// the watermark is recorded, and Queue how many records may wait for
	// the evaluator before they are a gap.
	Window, WatermarkEvery time.Duration
	Queue                  int
	// ActionTimeout is an action's timeout when the contract gives none; 0
	// is 30 s.
	ActionTimeout time.Duration
	// Do runs f on the host's goroutine, after what is already queued
	// there. Required.
	Do func(f func())
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Core is a DUT rack's core.
type Core struct {
	cfg    CoreConfig
	c      *monitor.Contract
	online *monitor.Online
	tap    *monitor.Tap
	monIn  <-chan monitor.Item
	out    chan monitor.Output

	w       Writer
	mons    map[string]any
	wm, gap any
	mode    any
	actions map[string]any
	lastWM  int64
	err     error

	requested, applied string
	run                *actionRun
	runs               uint32
}

type actionRun struct {
	info    ActionInfo
	cancel  context.CancelFunc
	stopped string // why it was stopped, set before cancel
	timeout time.Duration
}

// NewCore loads the contract and makes the evaluator. Nothing is recorded
// until Declare, and nothing evaluated until Run.
func NewCore(cfg CoreConfig) (*Core, error) {
	switch {
	case cfg.Do == nil:
		return nil, errors.New("secondary: Do is required: an action's end is handed back to the host through it")
	case cfg.Window < 0 || cfg.WatermarkEvery <= 0 || cfg.Queue <= 0:
		return nil, errors.New("secondary: the window may be 0; the watermark period and the queue must be positive")
	}
	if cfg.Source == "" {
		cfg.Source = "the DUT rack"
	}
	if cfg.MonitorSource == "" {
		cfg.MonitorSource = cfg.Source
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c, err := monitor.Load(cfg.Contract, cfg.Channels, cfg.Check)
	if err != nil {
		return nil, err
	}
	if len(c.Actions) > 0 && cfg.Actions == nil {
		return nil, fmt.Errorf("secondary: contract %s declares actions, and Actions is nil", c.Name)
	}
	online, err := monitor.NewOnline(c, cfg.Window, cfg.WatermarkEvery)
	if err != nil {
		return nil, err
	}
	k := &Core{cfg: cfg, c: c, online: online, out: make(chan monitor.Output, 64)}
	k.tap, k.monIn = monitor.NewTap(online, cfg.Queue)
	return k, nil
}

// Contract is the loaded contract.
func (k *Core) Contract() *monitor.Contract { return k.c }

// Meta is what the host's recording states of the contract and the
// monitors, before its first record: the protocol version, the contract
// and its resource with their sha256, and the evaluator's settings.
func (k *Core) Meta() map[string]string {
	m := map[string]string{
		"peer.protocol":          strconv.Itoa(peer.Protocol),
		"dut.contract":           k.c.Name,
		"dut.contract.sha256":    k.c.Hash,
		"dut.contract.source":    k.cfg.Contract,
		"dut.monitors.window":    k.cfg.Window.String(),
		"dut.monitors.watermark": k.cfg.WatermarkEvery.String(),
		"dut.monitors.queue":     strconv.Itoa(k.cfg.Queue),
	}
	if k.c.Resource != "" {
		m["dut.resource"] = k.c.Resource
		m["dut.resource.sha256"] = k.c.ResourceHash
	}
	return m
}

// Attach is the files the host's recording embeds: the contract, and its
// resource.
func (k *Core) Attach() (map[string][]byte, error) {
	raw, err := os.ReadFile(k.cfg.Contract)
	if err != nil {
		return nil, err
	}
	a := map[string][]byte{"dut-interface.json": raw}
	if k.c.Resource != "" {
		rb, err := os.ReadFile(k.c.ResourcePath())
		if err != nil {
			return nil, err
		}
		a[filepath.Base(k.c.Resource)] = rb
	}
	return a, nil
}

// Info is GET /dut/contract's answer (§6.2), with the rack's provenance.
func (k *Core) Info(provenance map[string]string) map[string]any {
	return map[string]any{
		"protocol":        peer.Protocol,
		"contract":        k.c.Name,
		"sha256":          k.c.Hash,
		"resource_sha256": k.c.ResourceHash,
		"provenance":      provenance,
	}
}

// Declare lays out the core's streams in w, and states every monitor
// unknown and no mode from t, so that a reader meets each from the start.
func (k *Core) Declare(w Writer, t time.Time) error {
	k.w = w
	k.mons, k.actions = map[string]any{}, map[string]any{}
	var err error
	for _, name := range k.c.Order {
		if k.mons[name], err = w.NewStream(func() *logb.Schema { return MonSchema(k.c, name, k.cfg.MonitorSource) }); err != nil {
			return err
		}
	}
	if k.wm, err = w.NewStream(func() *logb.Schema { return WatermarkSchema(k.cfg.MonitorSource) }); err != nil {
		return err
	}
	if k.gap, err = w.NewStream(func() *logb.Schema { return GapSchema(k.cfg.MonitorSource) }); err != nil {
		return err
	}
	if k.mode, err = w.NewStream(func() *logb.Schema { return ModeSchema(k.cfg.Source) }); err != nil {
		return err
	}
	for _, a := range slices.Sorted(maps.Keys(k.c.Actions)) {
		what := k.c.Actions[a]
		if k.actions[a], err = w.NewStream(func() *logb.Schema { return ActionSchema(a, what, k.cfg.Source) }); err != nil {
			return err
		}
	}
	for _, name := range k.c.Order {
		k.writeTransition(monitor.Transition{T: t.UnixNano(), Monitor: name, From: monitor.Unknown,
			To: monitor.Unknown, Cause: "start"})
	}
	k.writeMode(t, ModeRecord{Event: ModeStart})
	return k.err
}

// Err is the first error writing a record.
func (k *Core) Err() error { return k.err }

func (k *Core) write(s any, t time.Time, fill func(rec []byte)) {
	if k.err != nil {
		return
	}
	k.err = k.w.Write(s, t, fill)
}

func (k *Core) writeTransition(x monitor.Transition) {
	k.write(k.mons[x.Monitor], time.Unix(0, x.T), func(rec []byte) { PutTransition(rec, x) })
}

func (k *Core) writeMode(t time.Time, m ModeRecord) {
	m.Requested, m.Applied = k.requested, k.applied
	k.write(k.mode, t, func(rec []byte) { PutMode(rec, m) })
}

func (k *Core) writeAction(t time.Time, run *actionRun, ev byte, o peer.Outcome, msg string) {
	in := run.info
	a := ActionRecord{Seq: in.Seq, Run: in.Run, Event: ev, Via: byte(in.By.Via), By: in.By.Name, Mode: in.Mode, Message: msg}
	if o != "" {
		a.Outcome = byte(slices.Index(peer.Outcomes, o) + 1)
	}
	k.write(k.actions[in.Action], t, func(rec []byte) { PutAction(rec, a) })
}

// # The monitors

// Run is the evaluator, on the goroutine the host gives it, until Close.
// Its outputs come out of Outputs, for the host to hand back to Output.
func (k *Core) Run() {
	k.online.Run(k.monIn, k.out)
	close(k.out)
}

// Outputs is what the evaluator produced; closed once it has stopped.
func (k *Core) Outputs() <-chan monitor.Output { return k.out }

// Input hands the evaluator a record of a stream the host wrote, at its
// time t; streams the contract does not read are passed over. rec is
// copied.
func (k *Core) Input(stream string, t time.Time, rec []byte) { k.tap.Record(stream, t, rec) }

// SetClock tells the evaluator the host's clock: the latest time at which
// the host knew it had taken in everything its sources sent. A host behind
// a backlog leaves it where it was, so that the watermark does not run past
// records still waiting.
func (k *Core) SetClock(t time.Time) { k.tap.SetClock(t) }

// Tick lets the evaluator advance to the clock SetClock last gave: deadlines
// fall due, and the watermark moves.
func (k *Core) Tick() { k.tap.Tick(k.tap.Clock()) }

// Output records what the evaluator produced: its transitions, the gap
// that caused some of them, then the watermark over all of it.
func (k *Core) Output(o monitor.Output) {
	for _, x := range o.Transitions {
		k.writeTransition(x)
	}
	if g := o.Gap; g != nil {
		k.write(k.gap, time.Unix(0, g.T), func(rec []byte) { PutGap(rec, g) })
	}
	if wm := o.Watermark; wm != 0 && wm > k.lastWM {
		k.lastWM = wm
		k.write(k.wm, time.Unix(0, wm), func(rec []byte) { PutWatermark(rec, wm) })
	}
}

// Close ends the evaluator's input; Outputs closes once it has drained.
func (k *Core) Close() { k.tap.Close() }

// # Modes and actions

// Busy says whether an action, or a mode's entry action, is under way, and
// which; a host refuses another mode or action meanwhile (409).
func (k *Core) Busy() (string, bool) {
	if k.run == nil {
		return "", false
	}
	return k.run.info.Action, true
}

// SetMode carries out a mode change the host has admitted, under its
// request number: recorded as requested, then the mode's entry action, if
// it has one, then applied (or failed). The monitors that watch it settle
// from when it is applied.
func (k *Core) SetMode(seq uint32, by By, mode string, t time.Time) {
	k.requested = mode
	k.writeMode(t, ModeRecord{Seq: seq, Event: ModeRequested, By: by.Name})
	action := ""
	if m := k.c.Modes[mode]; m != nil {
		action = m.Action
	}
	if action == "" {
		k.applyMode(mode, seq, by, t)
		return
	}
	k.start(ActionInfo{Seq: seq, By: by, Action: action, Mode: mode}, t)
}

// Invoke runs an action the host has admitted, under its request number.
// Its start is recorded now, its end when it ends.
func (k *Core) Invoke(seq uint32, by By, action string, t time.Time) {
	k.start(ActionInfo{Seq: seq, By: by, Action: action}, t)
}

// Stop ends the action under way, saying why; its end is recorded when it
// has finished. A disarm, a rack's stop and the rack stopping call it.
func (k *Core) Stop(why string) {
	if k.run != nil {
		k.run.stopped = why
		k.run.cancel()
	}
}

// Applied is the mode the DUT is in.
func (k *Core) Applied() string { return k.applied }

// applyMode puts the DUT in a mode from t: recorded, and handed to the
// monitors, which settle from t for the mode's settle time.
func (k *Core) applyMode(mode string, seq uint32, by By, t time.Time) {
	// The wall clock alone: the monitors take Unix nanoseconds, and a
	// monotonic reading would put the mode a few nanoseconds off the
	// recording's own account of t.
	t = t.Round(0)
	k.applied = mode
	k.writeMode(t, ModeRecord{Seq: seq, Event: ModeApplied, By: by.Name})
	k.tap.Mode(k.tap.Clock(), t, mode)
}

func (k *Core) start(info ActionInfo, t time.Time) {
	k.runs++
	info.Run = k.runs
	timeout := k.cfg.ActionTimeout
	if d, ok := k.c.Timeouts[info.Action]; ok && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), actionInfoKey{}, info), timeout)
	run := &actionRun{info: info, cancel: cancel, timeout: timeout}
	k.run = run
	k.writeAction(t, run, ActionStarted, "", "")
	go func() {
		err := k.cfg.Actions.Run(ctx, info.Action)
		deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		k.cfg.Do(func() { k.end(run, err, deadline) })
	}()
}

// end records an action's end, and applies the mode it entered, or says it
// failed. On the host's goroutine.
func (k *Core) end(run *actionRun, err error, deadline bool) {
	t := k.cfg.Now()
	if k.run == run {
		k.run = nil
	}
	o, msg := peer.OK, ""
	var f *peer.Failure
	switch {
	case run.stopped != "":
		o, msg = peer.Stopped, run.stopped
	case deadline:
		o, msg = peer.Timeout, fmt.Sprintf("action %s did not end within its timeout, %v", run.info.Action, run.timeout)
	case errors.As(err, &f):
		o, msg = f.Outcome, f.Msg
	case err != nil:
		o, msg = peer.Fail, err.Error()
	}
	k.writeAction(t, run, ActionEnded, o, msg)
	in := run.info
	if in.Mode == "" {
		return
	}
	if o == peer.OK {
		k.applyMode(in.Mode, in.Seq, in.By, t)
		return
	}
	k.writeMode(t, ModeRecord{Seq: in.Seq, Event: ModeFailed, By: in.By.Name,
		Message: fmt.Sprintf("entry action %s: %s: %s", in.Action, o, msg)})
}
