//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rveen/logb"

	"github.com/rveen/ktest/channel"
	"github.com/rveen/ktest/expr"
	"github.com/rveen/ktest/procedure"
	"github.com/rveen/ktest/procedure/lib"
	"github.com/rveen/ktest/record"
)

// # Procedures
//
// -procedures names a directory of procedures, written in a strict subset of
// Robot syntax, which the daemon runs itself (handoff §4.8): a run survives
// the client that started it disconnecting.
//
//	curl ... -d '{"file":"ramp.robot","test":"Ramp"}' http://ktest/procedures/run
//	curl ... -X DELETE http://ktest/procedures/run
//
// A procedure is loaded and checked whole before it starts, and refused with a
// sentence naming the construct and its line. One runs at a time.
//
// A run acts through the control plane like any client, for the name that
// started it: it takes the lease of each instrument it writes, the first time
// it writes it, arms it, renews the lease while it runs and releases it at
// the end. Each of those is a control decision recorded with via 3 and the
// starter's name, and held to the same limits. Nothing is armed again behind
// the stop's back: a stop disarms the run's instruments too, and the run's
// next write is refused.
//
// Three streams record it:
//
//	procedure.run     each run's start, end and stop, with its hash, library and mode
//	procedure.step    every keyword call, at its start and at its end with its outcome
//	procedure.result  what Record Step records

// errProcedure is a procedure the daemon refused to load; errProcBusy is a
// start while a run is under way, or a stop while none is.
var (
	errProcedure = errors.New("procedure")
	errProcBusy  = errors.New("procedure")
)

// procs is the daemon's procedure runner. Its state is behind mu, because a
// run's goroutine, the HTTP handlers and the loop all touch it; what it writes
// it writes through the loop, as everything else does.
type procs struct {
	d     *daemon
	dir   string
	table *channel.Table
	lib   *procedure.Library

	hard   context.Context // cancelled when the daemon stops
	cancel context.CancelFunc

	mu  sync.Mutex
	cur *procRun
	n   uint32 // runs started
	// last is the run that ended most recently, which procedure.run's held
	// fields go on stating until another starts.
	last *procRun

	run, step, result *teeStream

	// contract is the sha256 of the DUT contract the daemon runs, which every
	// run records; "" for none.
	contract string

	// lock is -locked: every run is held to it when it starts. nil is
	// engineering mode, where no run is accredited.
	lock *locked
}

// procRun is the run under way.
type procRun struct {
	n    uint32
	who  caller
	test string
	file string
	hash string
	stop context.CancelFunc
	// accredited: the run was held to the lock and nothing differed.
	accredited bool
	leases     map[string]string // instrument -> lease token, for what the run has leased
	armed      map[string]bool
	mu         sync.Mutex

	// cause is why a daemon rule ended the run, nil when none did.
	cause *runCause
}

type runCause struct {
	outcome procedure.Outcome
	msg     string
}

// failRun ends the run under way with a daemon rule as its cause: its
// [Teardown] runs, and its end says what failed it. Called on the loop.
func (p *procs) failRun(o procedure.Outcome, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return
	}
	p.cur.mu.Lock()
	if p.cur.cause == nil {
		p.cur.cause = &runCause{o, msg}
	}
	p.cur.mu.Unlock()
	p.cur.stop()
}

func newProcs(d *daemon, dir string, table *channel.Table) *procs {
	p := &procs{d: d, dir: dir, table: table}
	p.hard, p.cancel = context.WithCancel(context.Background())
	p.lib = lib.New(&rackAdapter{p: p})
	return p
}

// declare lays out the procedure streams, so that a reader meets them from
// the start.
func (p *procs) declare(w *tee) error {
	var err error
	if p.run, err = w.NewStream(procRunSchema); err != nil {
		return err
	}
	if p.step, err = w.NewStream(procStepSchema); err != nil {
		return err
	}
	p.result, err = w.NewStream(procResultSchema)
	return err
}

// job runs f on the loop, unless the loop has stopped taking work.
func (p *procs) job(f func(l *loop)) {
	select {
	case p.d.jobs <- f:
	case <-p.d.closing:
	}
}

type runBody struct {
	File string `json:"file"`
	Test string `json:"test"`
}

// start is POST /procedures/run.
func (p *procs) start(w http.ResponseWriter, r *http.Request) {
	if p.dir == "" {
		httpError(w, http.StatusNotFound, 0, "this daemon runs no procedures; start it with -procedures <directory>")
		return
	}
	var body runBody
	if !decode(w, r, &body) {
		return
	}
	who := callerOf(r)
	// A procedure is a file in the procedures directory, named relative to
	// it: a client chooses which procedure, never which file on the machine.
	var loadErr error
	var suite *procedure.Suite
	switch {
	case who.role != roleOperate:
		loadErr = fmt.Errorf("%w: %q has the view role, which may read the streams and stop, and nothing more",
			errForbidden, who.name)
	case body.File == "" || body.Test == "":
		loadErr = fmt.Errorf("%w: a run names its file and its test: {\"file\": \"ramp.robot\", \"test\": \"Ramp\"}", errProcedure)
	case !filepath.IsLocal(body.File):
		loadErr = fmt.Errorf("%w: %q: a procedure is named relative to the procedures directory, inside it", errProcedure, body.File)
	default:
		suite, loadErr = procedure.Load(filepath.Join(p.dir, body.File),
			procedure.Env{Lib: p.lib, Channels: p.table})
		if loadErr == nil && !containsTest(suite, body.Test) {
			loadErr = fmt.Errorf("%w: %s has no test %q; its tests are %s", errProcedure, body.File, body.Test,
				strings.Join(suite.Tests(), ", "))
		} else if loadErr != nil {
			loadErr = fmt.Errorf("%w: %v", errProcedure, loadErr)
		}
	}
	// A locked daemon holds the run to its sums now, on the bytes that were
	// loaded: what differs refuses the run, or marks it.
	var diffs []string
	if loadErr == nil && p.lock != nil {
		diffs = p.lock.check(body.File, suite)
		if len(diffs) > 0 && !p.lock.mark {
			loadErr = fmt.Errorf("%w: %s is refused: it %s", errLocked, body.File, diffSentence(p.lock.path, diffs))
		}
	}
	p.d.submit(w, r, &request{kind: reqJob, job: func(l *loop, t time.Time, seq uint32) reply {
		if loadErr != nil {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.record(l, t, procEvent{seq: seq, event: runRefused, who: who,
				msg: fmt.Sprintf("%s from %s: %v", body.Test, body.File, loadErr)})
			return reply{seq: seq, err: loadErr}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cur != nil {
			err := fmt.Errorf("%w: run %d, %s from %s, is under way; stop it first", errProcBusy, p.cur.n, p.cur.test, p.cur.file)
			p.record(l, t, procEvent{seq: seq, event: runRefused, who: who,
				msg: fmt.Sprintf("%s from %s: %v", body.Test, body.File, err)})
			return reply{seq: seq, err: err}
		}
		p.n++
		ctx, stop := context.WithCancel(p.hard)
		run := &procRun{n: p.n, who: who, test: body.Test, file: body.File, hash: suite.Hash, stop: stop,
			accredited: p.lock != nil && len(diffs) == 0,
			leases:     map[string]string{}, armed: map[string]bool{}}
		p.cur = run
		ev := procEvent{seq: seq, run: run.n, event: runStart, who: who}
		if len(diffs) > 0 {
			ev.msg = "not accredited: it " + diffSentence(p.lock.path, diffs)
		}
		p.record(l, t, ev)
		go p.execute(ctx, run, suite)
		return reply{seq: seq}
	}})
}

func containsTest(s *procedure.Suite, test string) bool {
	for _, t := range s.Tests() {
		if strings.EqualFold(strings.Join(strings.Fields(t), " "), strings.Join(strings.Fields(test), " ")) {
			return true
		}
	}
	return false
}

// stop is DELETE /procedures/run. Anyone the daemon admits may stop a run,
// as anyone may disarm: stopping is always safe, and the run's [Teardown]
// still runs.
func (p *procs) stopRun(w http.ResponseWriter, r *http.Request) {
	who := callerOf(r)
	p.d.submit(w, r, &request{kind: reqJob, job: func(l *loop, t time.Time, seq uint32) reply {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.cur == nil {
			err := fmt.Errorf("%w: no run is under way", errProcBusy)
			return reply{seq: seq, err: err}
		}
		p.record(l, t, procEvent{seq: seq, run: p.cur.n, event: runStopAsked, who: who})
		p.cur.stop()
		return reply{seq: seq}
	}})
}

// execute runs a procedure to its end on a goroutine of its own, so that a
// slow step holds up nothing else.
func (p *procs) execute(ctx context.Context, run *procRun, suite *procedure.Suite) {
	host := &procHost{p: p, run: run}
	renew := make(chan struct{})
	go p.renew(run, renew)
	res := suite.Run(ctx, p.hard, run.test, host)
	close(renew)
	run.mu.Lock()
	if c := run.cause; c != nil {
		res.Outcome, res.Msg = c.outcome, c.msg
	}
	run.mu.Unlock()
	// The leases the run took are released when it ends, which disarms the
	// instruments without switching anything off: the teardown has already
	// done what the procedure says to do at the end.
	run.mu.Lock()
	leases := run.leases
	run.leases = map[string]string{}
	run.mu.Unlock()
	for _, inst := range sortedKeys(leases) {
		p.ask(context.WithoutCancel(p.hard), run, &request{kind: reqLeaseRelease, inst: inst, token: leases[inst]})
	}
	p.job(func(l *loop) {
		p.mu.Lock()
		defer p.mu.Unlock()
		where := ""
		if res.Pos.File != "" {
			where = res.Pos.String()
		}
		if p.cur != run {
			return // the daemon's stopping recorded the end already
		}
		p.cur, p.last = nil, run
		p.record(l, time.Now(), procEvent{run: run.n, event: runEnd, outcome: res.Outcome, msg: res.Msg, where: where})
	})
}

// renew keeps the run's leases while it runs. A run that stopped renewing
// would lose its instruments the way a crashed client does, which is the
// right thing for a run whose goroutine has gone.
func (p *procs) renew(run *procRun, done <-chan struct{}) {
	tick := time.NewTicker(defaultTTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			run.mu.Lock()
			leases := make(map[string]string, len(run.leases))
			for k, v := range run.leases {
				leases[k] = v
			}
			run.mu.Unlock()
			for _, inst := range sortedKeys(leases) {
				p.ask(p.hard, run, &request{kind: reqLeaseRenew, inst: inst, token: leases[inst]})
			}
		}
	}
}

// shutdown ends a run when the daemon stops: on the loop, so that the end is
// recorded before the recording closes.
func (p *procs) shutdown(l *loop, t time.Time) {
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil {
		return
	}
	run := p.cur
	p.cur, p.last = nil, run
	p.record(l, t, procEvent{run: run.n, event: runEnd, outcome: procedure.Stopped, msg: "the daemon is stopping"})
}

// ask makes a request of the control plane for the run, as a client would,
// and waits for its answer.
func (p *procs) ask(ctx context.Context, run *procRun, req *request) reply {
	req.who = caller{name: run.who.name, role: run.who.role, via: record.ViaProcedure}
	req.reply = make(chan reply, 1)
	select {
	case p.d.reqs <- req:
	case <-p.d.closing:
		return reply{err: errors.New("the daemon is stopping")}
	case <-ctx.Done():
		return reply{err: ctx.Err()}
	}
	select {
	case rep := <-req.reply:
		return rep
	case <-p.d.gone:
		return reply{err: errors.New("the daemon stopped before the instrument answered")}
	case <-ctx.Done():
		// The request was taken and will be answered; the run no longer
		// waits for it.
		return reply{err: ctx.Err()}
	}
}

// procHost is what a run's procedure sees of the daemon.
type procHost struct {
	p   *procs
	run *procRun
}

func (h *procHost) Resolve(name string) (expr.Type, error) { return h.p.table.Resolve(name) }
func (h *procHost) Value(name string) (expr.Value, int64, bool) {
	return h.p.table.Value(name)
}

func (h *procHost) Step(ev procedure.StepEvent) {
	n := h.run.n
	h.p.job(func(l *loop) { h.p.recordStep(l, n, ev) })
}

// rackAdapter is lib.Rack over the daemon. The library is made once, with
// this, and each call finds the run under way.
type rackAdapter struct{ p *procs }

func (a *rackAdapter) Resolve(name string) (expr.Type, error) { return a.p.table.Resolve(name) }

func (a *rackAdapter) Stressor(target string) error {
	inst, part, ok := strings.Cut(target, ".")
	n, isSupply := a.p.d.supplies[inst]
	if !ok || !isSupply {
		return fmt.Errorf("%s: a stress target is a supply channel of this daemon, such as psu1.ch1", target)
	}
	ch, err := strconv.Atoi(strings.TrimPrefix(part, "ch"))
	if !strings.HasPrefix(part, "ch") || err != nil || ch < 1 || ch > n {
		return fmt.Errorf("%s: supply %s has channels ch1 to ch%d", target, inst, n)
	}
	return nil
}

func (a *rackAdapter) current() *procRun {
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	return a.p.cur
}

// Set writes one setting for the run: the instrument leased and armed first,
// if the run has not yet done so.
func (a *rackAdapter) Set(ctx context.Context, channel string, value float64) error {
	run := a.current()
	if run == nil {
		return procedure.Failf(procedure.Fail, "no run is under way")
	}
	cn, err := parseChannel(channel)
	if err != nil {
		return procedure.Failf(procedure.Fail, "%v", err)
	}
	refused := func(rep reply) error {
		if errors.Is(rep.err, context.Canceled) || errors.Is(rep.err, context.DeadlineExceeded) {
			return rep.err
		}
		return procedure.Failf(procedure.Refused, "%s: %v", channel, rep.err)
	}

	run.mu.Lock()
	token, leased := run.leases[cn.inst]
	armed := run.armed[cn.inst]
	run.mu.Unlock()
	if !leased {
		holder := run.who.name
		if holder == "" {
			holder = "procedure"
		}
		rep := a.p.ask(ctx, run, &request{kind: reqLeaseTake, inst: cn.inst, holder: holder, ttl: defaultTTL})
		if rep.err != nil {
			return refused(rep)
		}
		token = rep.token
		run.mu.Lock()
		run.leases[cn.inst] = token
		run.mu.Unlock()
	}
	if !armed {
		if rep := a.p.ask(ctx, run, &request{kind: reqArm, inst: cn.inst, token: token}); rep.err != nil {
			return refused(rep)
		}
		run.mu.Lock()
		run.armed[cn.inst] = true
		run.mu.Unlock()
	}
	rep := a.p.ask(ctx, run, &request{kind: reqChannelSet, inst: cn.inst, token: token, part: cn.part,
		ch: chanOf(cn.part), what: cn.what, value: value})
	if rep.err != nil {
		return refused(rep)
	}
	return nil
}

func (a *rackAdapter) Result(sr lib.StepResult) {
	run := a.current()
	if run == nil {
		return
	}
	a.p.job(func(l *loop) { a.p.recordResult(l, run.n, sr) })
}

// The procedure streams' record layouts, in bytes.
//
// procedure.run: t 0–7, seq 8–11, run 12–15, event 16, outcome 17, via 18,
// state 19 (running bit 0, accredited bit 1), test 20–83, file 84–147,
// hash 148–211, library 212–243, contract 244–307, mode 308–323, by 324–355,
// where 356–419, message 420–579, lock 580–643.
const (
	prSeq      = 8
	prRun      = 12
	prEvent    = 16
	prOutcome  = 17
	prVia      = 18
	prState    = 19
	prTest     = 20
	prFile     = prTest + 64
	prHash     = prFile + 64
	prLibrary  = prHash + 64
	prContract = prLibrary + 32
	prMode     = prContract + 64
	prBy       = prMode + 16
	prWhere    = prBy + record.HolderMax
	prMessage  = prWhere + 64
	prLock     = prMessage + 160
	prBytes    = prLock + 64
)

// procedure.step: t 0–7, run 8–11, id 12–15, parent 16–19, event 20,
// outcome 21, flags 22 (args truncated bit 0, message truncated bit 1),
// t_start 24–31, keyword 32–95, where 96–159, args 160–351, message 352–511.
const (
	stepRun      = 8
	stepID       = 12
	stepParent   = 16
	stepEvent    = 20
	stepOutcome  = 21
	stepFlags    = 22
	stepStart    = 24
	stepKeyword  = 32
	stepWhere    = stepKeyword + 64
	stepArgs     = stepWhere + 64
	stepMessage  = stepArgs + 192
	stepStepByte = stepMessage + 160
)

// procedure.result: t 0–7, run 8–11, flags 12 (level is a number bit 0,
// threshold given bit 1), level 16–23, threshold 24–31, point 32–95,
// level_text 96–127, level_unit 128–143, threshold_unit 144–159,
// result 160–223.
const (
	rsRun       = 8
	rsFlags     = 12
	rsLevel     = 16
	rsThreshold = 24
	rsPoint     = 32
	rsLevelText = rsPoint + 64
	rsLevelUnit = rsLevelText + 32
	rsThUnit    = rsLevelUnit + 16
	rsResult    = rsThUnit + 16
	rsBytes     = rsResult + 64
)

// The events of procedure.run.
const (
	runStart     = 1
	runEnd       = 2
	runStopAsked = 3
	runRefused   = 4
)

const outcomeDesc = "1 ok, 2 nok, 3 timeout, 4 refused, 5 link_lost, 6 fail, 7 stopped; 0 for none"

func outcomeCode(o procedure.Outcome) uint8 {
	for i, x := range procedure.Outcomes {
		if x == o {
			return uint8(i + 1)
		}
	}
	return 0
}

func uintF(name string, at, bits uint32, unit, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: bits, Type: logb.TypeUint, Unit: unit, Desc: desc}
}

func strF(name string, at, n uint32, desc string) logb.Field {
	return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 8 * n, Type: logb.TypeString, Desc: desc}
}

func holdF(f logb.Field) logb.Field { f.Hold = true; return f }

func procRunSchema() *logb.Schema {
	return record.Schema("procedure.run", 8*prBytes, []logb.Field{
		uintF("seq", prSeq, 32, "", "the request that started, stopped or was refused; 0 for an end"),
		uintF("run", prRun, 32, "", "the run, numbered from 1 since the daemon started; 0 for a refusal"),
		uintF("event", prEvent, 8, "", "1 a run started, 2 it ended, 3 a stop was asked for, 4 a start was refused"),
		uintF("outcome", prOutcome, 8, "", "how the run ended, at its end: "+outcomeDesc),
		uintF("via", prVia, 8, "", "the listener the request came through: 1 the local socket, 2 TLS; 0 for an end"),
		holdF(logb.Field{Name: "running", BitOffset: 8 * prState, BitWidth: 1, Type: logb.TypeBool,
			Desc: "a run is under way"}),
		holdF(logb.Field{Name: "accredited", BitOffset: 8*prState + 1, BitWidth: 1, Type: logb.TypeBool,
			Desc: "the run was held to the locked sums and nothing differed; never in engineering mode"}),
		holdF(strF("test", prTest, 64, "the test case run")),
		holdF(strF("file", prFile, 64, "the procedure file, relative to the procedures directory")),
		holdF(strF("hash", prHash, 64, "sha256 of every file the procedure was loaded from, hex")),
		holdF(strF("library", prLibrary, 32, "the keyword library's version")),
		holdF(strF("contract", prContract, 64, "sha256 of the DUT contract; empty for a run with none")),
		holdF(strF("mode", prMode, 16, "locked or engineering (handoff §4.9)")),
		holdF(strF("lock", prLock, 64, "sha256 of the -locked file the run was held to; empty in engineering mode")),
		strF("by", prBy, record.HolderMax, "the name that asked; empty on the local socket and for an end"),
		strF("where", prWhere, 64, "where the run failed, file:line"),
		strF("message", prMessage, 160, "why a run failed or was refused"),
	}, map[string]string{
		"source": "the ktestd procedure runner",
		"clock":  "userspace: the daemon's clock when it decided, or when the run ended",
	})
}

func procStepSchema() *logb.Schema {
	return record.Schema("procedure.step", 8*stepStepByte, []logb.Field{
		uintF("run", stepRun, 32, "", "the run the step belongs to"),
		uintF("id", stepID, 32, "", "the call, numbered from 1 within the run in the order calls start"),
		uintF("parent", stepParent, 32, "", "the call this one was made from; 0 for a test's own step"),
		uintF("event", stepEvent, 8, "", "1 the call started, 2 it ended"),
		uintF("outcome", stepOutcome, 8, "", "at its end: "+outcomeDesc),
		{Name: "args_truncated", BitOffset: 8 * stepFlags, BitWidth: 1, Type: logb.TypeBool,
			Desc: "args did not fit, and is cut short"},
		{Name: "message_truncated", BitOffset: 8*stepFlags + 1, BitWidth: 1, Type: logb.TypeBool,
			Desc: "message did not fit, and is cut short"},
		{Name: "t_start", BitOffset: 8 * stepStart, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "when the call started, Unix time; the record's own time is its start or its end"},
		strF("keyword", stepKeyword, 64, "the keyword, as written"),
		strF("where", stepWhere, 64, "file:line of the call"),
		strF("args", stepArgs, 192, "the arguments as resolved, separated by two spaces"),
		strF("message", stepMessage, 160, "at its end: why it failed, or what it returned"),
	}, map[string]string{
		"source": "the ktestd procedure runner",
		"clock":  "userspace: the daemon's clock when the call started or ended",
	})
}

func procResultSchema() *logb.Schema {
	return record.Schema("procedure.result", 8*rsBytes, []logb.Field{
		uintF("run", rsRun, 32, "", "the run that recorded it"),
		{Name: "level.known", BitOffset: 8 * rsFlags, BitWidth: 1, Type: logb.TypeBool,
			Desc: "the level is a number; level_text has it otherwise"},
		{Name: "threshold.known", BitOffset: 8*rsFlags + 1, BitWidth: 1, Type: logb.TypeBool,
			Desc: "a threshold was given"},
		{Name: "level", BitOffset: 8 * rsLevel, BitWidth: 64, Type: logb.TypeFloat,
			Desc: "the stress level, in SI; level_unit is the unit it was written in", Guarded: true, GuardField: 2, GuardValue: 1},
		{Name: "threshold", BitOffset: 8 * rsThreshold, BitWidth: 64, Type: logb.TypeFloat,
			Desc: "the threshold found, in SI", Guarded: true, GuardField: 3, GuardValue: 1},
		strF("point", rsPoint, 64, "the point of the test"),
		strF("level_text", rsLevelText, 32, "the level as written"),
		strF("level_unit", rsLevelUnit, 16, "the unit the level was written in"),
		strF("threshold_unit", rsThUnit, 16, "the unit the threshold was written in"),
		strF("result", rsResult, 64, "the result"),
	}, map[string]string{
		"source": "the Record Step keyword of a procedure",
		"clock":  "userspace: the daemon's clock when it was recorded",
	})
}

// putStr writes s into a fixed string field of n bytes, cut at a character
// boundary, and says whether it had to be cut.
func putStr(rec []byte, at, n int, s string) bool {
	cut := false
	for len(s) > n {
		cut = true
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	copy(rec[at:at+n], s)
	return cut
}

type procEvent struct {
	seq     uint32
	run     uint32 // 0 for a refusal
	event   uint8
	who     caller // who asked; nobody for an end
	outcome procedure.Outcome
	msg     string
	where   string
}

// record writes one procedure.run record. Called on the loop with p.mu held,
// after the event has changed the state: the held fields say what is true
// from this record on — whether a run is under way, and which run is, or was
// last — whatever the event was. A refused start leaves them as they were,
// and says what it refused in its message.
func (p *procs) record(l *loop, t time.Time, e procEvent) {
	state := p.cur
	if state == nil {
		state = p.last
	}
	err := l.w.Write(t, p.run, func(rec []byte) {
		putUint32(rec[prSeq:], e.seq)
		putUint32(rec[prRun:], e.run)
		rec[prEvent] = e.event
		rec[prOutcome] = outcomeCode(e.outcome)
		if e.event != runEnd {
			rec[prVia] = byte(e.who.via)
			putStr(rec, prBy, record.HolderMax, e.who.name)
		}
		if p.cur != nil {
			rec[prState] = 1
		}
		if state != nil {
			if state.accredited {
				rec[prState] |= 2
			}
			putStr(rec, prTest, 64, state.test)
			putStr(rec, prFile, 64, state.file)
			putStr(rec, prHash, 64, state.hash)
		}
		putStr(rec, prLibrary, 32, lib.Version)
		putStr(rec, prContract, 64, p.contract)
		if p.lock != nil {
			putStr(rec, prMode, 16, "locked")
			putStr(rec, prLock, 64, p.lock.hash)
		} else {
			putStr(rec, prMode, 16, "engineering")
		}
		putStr(rec, prWhere, 64, e.where)
		putStr(rec, prMessage, 160, e.msg)
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

func (p *procs) recordStep(l *loop, run uint32, ev procedure.StepEvent) {
	recordStep(l, p.step, run, ev)
}

// recordStep writes one keyword call's start or end into a stream laid out as
// procedure.step is.
func recordStep(l *loop, st *teeStream, run uint32, ev procedure.StepEvent) {
	t := ev.Start
	if ev.End {
		t = ev.Stop
	}
	err := l.w.Write(t, st, func(rec []byte) {
		putUint32(rec[stepRun:], run)
		putUint32(rec[stepID:], ev.ID)
		putUint32(rec[stepParent:], ev.Parent)
		rec[stepEvent] = 1
		if ev.End {
			rec[stepEvent] = 2
			rec[stepOutcome] = outcomeCode(ev.Outcome)
		}
		putUint64(rec[stepStart:], uint64(ev.Start.UnixNano()))
		putStr(rec, stepKeyword, 64, ev.Keyword)
		putStr(rec, stepWhere, 64, ev.Pos.String())
		if putStr(rec, stepArgs, 192, strings.Join(ev.Args, "  ")) {
			rec[stepFlags] |= 1
		}
		if putStr(rec, stepMessage, 160, ev.Msg) {
			rec[stepFlags] |= 2
		}
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

func (p *procs) recordResult(l *loop, run uint32, sr lib.StepResult) {
	err := l.w.Write(time.Now(), p.result, func(rec []byte) {
		putUint32(rec[rsRun:], run)
		putStr(rec, rsPoint, 64, sr.Point)
		putStr(rec, rsResult, 64, sr.Result)
		putStr(rec, rsLevelText, 32, sr.Level.String())
		if sr.Level.Kind == procedure.VNumber {
			rec[rsFlags] |= 1
			putUint64(rec[rsLevel:], math.Float64bits(sr.Level.Num))
			putStr(rec, rsLevelUnit, 16, sr.Level.Unit.Name)
		}
		if th := sr.Threshold; th != nil && th.Kind == procedure.VNumber {
			rec[rsFlags] |= 2
			putUint64(rec[rsThreshold:], math.Float64bits(th.Num))
			putStr(rec, rsThUnit, 16, th.Unit.Name)
		}
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}

func putUint32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putUint64(b []byte, v uint64) {
	putUint32(b, uint32(v))
	putUint32(b[4:], uint32(v>>32))
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
