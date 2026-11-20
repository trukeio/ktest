// Package secondary is a DUT rack's side of the Ktest peer protocol
// (doc/peer-protocol.md): a program that operates and observes a device
// under test, and is driven by a master — ktestd or another test
// application — as an instrument.
//
// The host program declares the streams it records of the DUT, writes their
// records as they arrive, and runs the DUT's actions when asked. The Server
// does the rest of what the protocol asks of a secondary:
//
//   - loads the contract against the host's streams, and records it;
//   - evaluates the monitors live, purely, with the watermark and gaps (§5);
//   - records everything, to the host's recording and, record by record, to
//     /stream/sub (§7), in the layouts of §7.4;
//   - serves the REST interface (§6): the lease, arming, modes, actions, the
//     contract and the clock, each request recorded as a control decision.
//
// The host serves Handler over TLS with an http.Server of its own, and calls
// Run until it stops. It keeps the DUT running whatever the master does
// (§10.1): a lapsed lease or a lost link stops nothing here; only a disarm,
// which the Server passes on as OnDisarm, does.
//
// ktestd's -contract is the other secondary in this repository. It shares
// this package's layouts and subscription filter, and the monitor package,
// and keeps its own event loop, which it shares with its bus and
// instruments.
package secondary

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/channel"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
	"github.com/trukeio/ktest/stream"
)

// Config is what a DUT rack is made of.
type Config struct {
	// Contract is the path of dut-interface.json.
	Contract string
	// Streams are the streams the host will write, each made with
	// record.Schema: its time axis first, in nanoseconds. The contract's
	// inputs must name them.
	Streams []*logb.Schema
	// Check, if set, holds the contract's actions to their resource; see
	// monitor.ActionCheck.
	Check monitor.ActionCheck
	// Actions runs the contract's actions. Required if it declares any.
	Actions Actions
	// Auth says who a bearer token belongs to; false refuses it. Required.
	// Tokens makes one from a fixed set.
	Auth func(token string) (Caller, bool)
	// Recording is where the recording goes: Logb, compressed. Required: a
	// DUT rack records everything it streams (§10.2).
	Recording io.Writer
	// Name names the recording; "" is "dut-rack".
	Name string
	// Meta is the rack's provenance, written into the recording and given
	// in GET /dut/contract.
	Meta map[string]string
	// Source is what the streams' schemas say wrote them; "" is "the DUT
	// rack".
	Source string

	// Window is how long a record is held for earlier ones to arrive before
	// the monitors see it; 0 is 20 ms. It must exceed the longest a host
	// takes between a record's time and its Write: a record written later
	// than that is late, a gap, and every monitor is unknown until its
	// inputs are known again. The watermark trails the clock by it, so a
	// master's watermark_age must exceed it by a margin.
	Window time.Duration
	// WatermarkEvery is how often the watermark is recorded; 0 is 10 ms.
	WatermarkEvery time.Duration
	// Tick is how often the evaluator is told the time; 0 is 1 ms.
	Tick time.Duration
	// Queue is how many records may wait for the evaluator before they are
	// a gap; 0 is 4096.
	Queue int
	// ActionTimeout is an action's timeout when the contract gives none; 0
	// is 30 s.
	ActionTimeout time.Duration
	// Segment is how often the recording restates its schemas and held
	// values; 0 is 1 s.
	Segment time.Duration

	// OnDisarm, if set, is called when the DUT is disarmed: the host stops
	// whatever it runs for the DUT that a disarm should stop, such as its
	// cyclic traffic. It is called with no lock held.
	OnDisarm func()
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Caller is an authenticated name and what it may do: operate, or only view
// the streams and disarm (§2).
type Caller struct {
	Name    string
	Operate bool
}

// Tokens is an Auth over a fixed set of tokens.
func Tokens(m map[string]Caller) func(string) (Caller, bool) {
	type entry struct {
		tok []byte
		c   Caller
	}
	var list []entry
	for t, c := range m {
		list = append(list, entry{[]byte(t), c})
	}
	return func(tok string) (Caller, bool) {
		var found Caller
		ok := false
		for _, e := range list {
			if subtle.ConstantTimeCompare(e.tok, []byte(tok)) == 1 {
				found, ok = e.c, true
			}
		}
		return found, ok
	}
}

// Actions runs the DUT's actions.
type Actions interface {
	// Run runs action, as the contract names it, and returns when it ends:
	// nil for ok, a *peer.Failure for another outcome, or any other error
	// for fail. ctx ends at the action's timeout, or when it is stopped.
	Run(ctx context.Context, action string) error
}

// ActionFunc is a function as Actions.
type ActionFunc func(ctx context.Context, action string) error

func (f ActionFunc) Run(ctx context.Context, action string) error { return f(ctx, action) }

// Server is a DUT rack.
type Server struct {
	cfg      Config
	c        *monitor.Contract
	provence map[string]string

	online *monitor.Online
	tap    *monitor.Tap
	monIn  <-chan monitor.Item
	out    chan monitor.Output

	hub *stream.Hub

	// The recording is written on a goroutine of its own, fed in order by
	// fileOps: compressing it may take tens of milliseconds, and nothing the
	// monitors need may wait for that. The subscription stream is
	// uncompressed, a record to a frame, and is written inline.
	file     *record.Writer
	fileOps  chan fileOp
	fileDone chan struct{}
	fileMu   sync.Mutex
	fileErr  error

	mu      sync.Mutex
	sub     *record.Writer
	streams map[string]*Stream
	mons    map[string]*Stream
	wm, gap *Stream
	mode    *Stream
	actions map[string]*Stream
	lastWM  int64
	err     error // the first error writing the recording
	closed  bool

	seq                uint32
	lease              *lease
	armed              bool
	requested, applied string
	run                *actionRun
	runs               uint32
}

type lease struct {
	token   string
	holder  string
	ttl     time.Duration
	expires time.Time
}

type actionRun struct {
	n       uint32
	action  string
	mode    *string
	seq     uint32
	by      string
	cancel  context.CancelFunc
	stopped string // why it was stopped, set under the lock before cancel
}

// fileOp is one thing for the recording's goroutine to write.
type fileOp struct {
	st    *record.Stream
	t     time.Time
	rec   []byte // the record as the subscription stream was given it
	ctl   *record.Control
	flush bool
}

// Stream is one stream of the recording, in both of its writers.
type Stream struct {
	name      string
	file, sub *record.Stream
}

// Name is the stream's name.
func (s *Stream) Name() string { return s.name }

// The DUT rack's own instrument, in its control stream.
const instrument = "dut"

// New loads the contract against the host's streams and starts the
// recording. Nothing is evaluated or served until Run.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Auth == nil:
		return nil, errors.New("secondary: Auth is required: every request is made under a named token (§2)")
	case cfg.Recording == nil:
		return nil, errors.New("secondary: Recording is required: a DUT rack records what it streams (§10.2)")
	}
	if cfg.Name == "" {
		cfg.Name = "dut-rack"
	}
	if cfg.Source == "" {
		cfg.Source = "the DUT rack"
	}
	if cfg.Window == 0 {
		cfg.Window = 20 * time.Millisecond
	}
	if cfg.WatermarkEvery <= 0 {
		cfg.WatermarkEvery = 10 * time.Millisecond
	}
	if cfg.Tick <= 0 {
		cfg.Tick = time.Millisecond
	}
	if cfg.Queue <= 0 {
		cfg.Queue = 4096
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	table := channel.NewTable()
	for _, sc := range cfg.Streams {
		if sc.AxisField != 0 || len(sc.Fields) == 0 || sc.Fields[0].Name != "t" {
			return nil, fmt.Errorf("secondary: stream %s does not lead with the time axis; make it with record.Schema", sc.Name)
		}
		table.Stream(sc)
	}
	c, err := monitor.Load(cfg.Contract, table, cfg.Check)
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
	s := &Server{cfg: cfg, c: c, online: online, out: make(chan monitor.Output, 64), hub: stream.New(),
		streams: map[string]*Stream{}, mons: map[string]*Stream{}, actions: map[string]*Stream{}}
	s.tap, s.monIn = monitor.NewTap(online, cfg.Queue)

	meta := map[string]string{}
	maps.Copy(meta, cfg.Meta)
	meta["peer.protocol"] = strconv.Itoa(peer.Protocol)
	meta["dut.contract"] = c.Name
	meta["dut.contract.sha256"] = c.Hash
	meta["dut.monitors.window"] = cfg.Window.String()
	meta["dut.monitors.watermark"] = cfg.WatermarkEvery.String()
	meta["dut.monitors.queue"] = strconv.Itoa(cfg.Queue)
	attach := map[string][]byte{}
	raw, err := os.ReadFile(cfg.Contract)
	if err != nil {
		return nil, err
	}
	attach["dut-interface.json"] = raw
	if c.Resource != "" {
		rb, err := os.ReadFile(c.ResourcePath())
		if err != nil {
			return nil, err
		}
		meta["dut.resource"] = c.Resource
		meta["dut.resource.sha256"] = c.ResourceHash
		attach[filepath.Base(c.Resource)] = rb
	}
	s.provence = maps.Clone(cfg.Meta)
	if s.provence == nil {
		s.provence = map[string]string{}
	}

	fcfg := record.Config{Bus: cfg.Name, NoBus: true, Codec: logb.CodecZstd, Segment: cfg.Segment,
		Meta: meta, Attach: attach, Now: cfg.Now}
	if s.file, err = record.New(cfg.Recording, fcfg); err != nil {
		return nil, err
	}
	scfg := record.Config{Bus: cfg.Name, NoBus: true, Codec: logb.CodecNone, Filter: logb.FilterNone,
		PerFrame: 1, Live: true, Segment: cfg.Segment, Meta: meta, Attach: attach, Now: cfg.Now}
	if s.sub, err = record.New(s.hub, scfg); err != nil {
		return nil, err
	}
	s.provence["recording.epoch"] = strconv.FormatInt(s.file.Epoch().UnixNano(), 10)

	// Everything is declared before the first record, so that a reader meets
	// every stream from the start: the host's, the monitors', the mode and
	// the actions, and the DUT's control stream.
	for _, sc := range cfg.Streams {
		sc := sc
		st, err := s.newStream(func() *logb.Schema { return cloneSchema(sc) })
		if err != nil {
			return nil, err
		}
		s.streams[sc.Name] = st
	}
	for _, name := range c.Order {
		if s.mons[name], err = s.newStream(func() *logb.Schema { return MonSchema(c, name, cfg.Source) }); err != nil {
			return nil, err
		}
	}
	if s.wm, err = s.newStream(func() *logb.Schema { return WatermarkSchema(cfg.Source) }); err != nil {
		return nil, err
	}
	if s.gap, err = s.newStream(func() *logb.Schema { return GapSchema(cfg.Source) }); err != nil {
		return nil, err
	}
	if s.mode, err = s.newStream(func() *logb.Schema { return ModeSchema(cfg.Source) }); err != nil {
		return nil, err
	}
	for _, a := range slices.Sorted(maps.Keys(c.Actions)) {
		what := c.Actions[a]
		if s.actions[a], err = s.newStream(func() *logb.Schema { return ActionSchema(a, what, cfg.Source) }); err != nil {
			return nil, err
		}
	}
	for _, w := range []*record.Writer{s.file, s.sub} {
		if err := w.DeclareControl(instrument, "dut"); err != nil {
			return nil, err
		}
	}
	s.fileOps, s.fileDone = make(chan fileOp, 1<<14), make(chan struct{})
	go s.writeFile()
	t := cfg.Now()
	for _, name := range c.Order {
		s.writeTransition(monitor.Transition{T: t.UnixNano(), Monitor: name, From: monitor.Unknown,
			To: monitor.Unknown, Cause: "start"})
	}
	s.writeMode(t, ModeRecord{Event: ModeStart})
	if s.err != nil {
		return nil, s.err
	}
	return s, nil
}

func cloneSchema(sc *logb.Schema) *logb.Schema {
	c := *sc
	c.Fields = slices.Clone(sc.Fields)
	c.Meta = maps.Clone(sc.Meta)
	return &c
}

func (s *Server) newStream(mk func() *logb.Schema) (*Stream, error) {
	sc := mk()
	f, err := s.file.NewStream(sc)
	if err != nil {
		return nil, err
	}
	sb, err := s.sub.NewStream(mk())
	if err != nil {
		return nil, err
	}
	return &Stream{name: sc.Name, file: f, sub: sb}, nil
}

// Freeze stops the server for d as a stopped process stops: its clock, its
// evaluator and its streams stand still, and writes and requests that need it
// wait. The watermark a master reads stops with them. It is for fault
// injection — the conformance kit's reference rack shows a master a DUT rack
// whose watermark has stopped (doc/peer-protocol.md §9.4) — and a rack has no
// other use for it.
func (s *Server) Freeze(d time.Duration) {
	s.mu.Lock()
	time.Sleep(d)
	s.mu.Unlock()
}

// Contract is the loaded contract.
func (s *Server) Contract() *monitor.Contract { return s.c }

// Stream is one of the host's streams, by name; nil if Config.Streams did
// not declare it.
func (s *Server) Stream(name string) *Stream { return s.streams[name] }

// Write records one record of a host stream at t: fill writes its fields
// after the time axis, the first 8 bytes. It may be called from any
// goroutine. Records of one stream should come in time order; records of
// different streams may come a little out of it, by up to Config.Window.
func (s *Server) Write(st *Stream, t time.Time, fill func(rec []byte)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("secondary: the server has stopped")
	}
	s.tap.SetClock(s.cfg.Now())
	s.write(st, t, fill)
	return s.err
}

// write records to both writers, and hands the record to the evaluator; the
// first error is kept. Called with the lock.
func (s *Server) write(st *Stream, t time.Time, fill func(rec []byte)) {
	if s.err != nil || s.closed {
		return
	}
	// The wall clock alone. A time with a monotonic reading is measured
	// from the recording's epoch by that reading, and the evaluator takes
	// Unix nanoseconds: the two would differ by the clocks' drift since the
	// start, and the recording would not replay to the nanosecond.
	t = t.Round(0)
	var cp []byte
	if err := s.sub.Write(t, st.sub, func(rec []byte) { fill(rec); cp = slices.Clone(rec) }); err != nil {
		s.err = err
		return
	}
	s.tap.Record(st.name, t, cp)
	s.toFile(fileOp{st: st.file, t: t, rec: cp})
}

// toFile queues an op for the recording. A queue full is a recording that
// cannot keep up, and waits: the recording is complete or it is nothing.
// Called with the lock.
func (s *Server) toFile(op fileOp) {
	if err := s.fileError(); err != nil && s.err == nil {
		s.err = err
		return
	}
	s.fileOps <- op
}

func (s *Server) fileError() error {
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	return s.fileErr
}

// writeFile is the recording's goroutine: it writes each op as the
// subscription stream had it, the axis apart, which is the recording's own.
func (s *Server) writeFile() {
	defer close(s.fileDone)
	for op := range s.fileOps {
		var err error
		switch {
		case op.flush:
			err = s.file.Flush()
		case op.ctl != nil:
			err = s.file.Control(op.t, op.ctl)
		default:
			err = s.file.Write(op.t, op.st, func(rec []byte) { copy(rec[8:], op.rec[8:]) })
		}
		if err != nil {
			s.fileMu.Lock()
			if s.fileErr == nil {
				s.fileErr = err
			}
			s.fileMu.Unlock()
		}
	}
	err := s.file.Close()
	s.fileMu.Lock()
	if s.fileErr == nil {
		s.fileErr = err
	}
	s.fileMu.Unlock()
}

func (s *Server) writeTransition(x monitor.Transition) {
	s.write(s.mons[x.Monitor], time.Unix(0, x.T), func(rec []byte) { PutTransition(rec, x) })
}

func (s *Server) writeMode(t time.Time, m ModeRecord) {
	m.Requested, m.Applied = s.requested, s.applied
	s.write(s.mode, t, func(rec []byte) { PutMode(rec, m) })
}

func (s *Server) writeAction(t time.Time, run *actionRun, ev byte, o peer.Outcome, msg string) {
	a := ActionRecord{Seq: run.seq, Run: run.n, Event: ev, Via: byte(record.ViaTLS), By: run.by, Message: msg}
	if o != "" {
		a.Outcome = byte(slices.Index(peer.Outcomes, o) + 1)
	}
	if run.mode != nil {
		a.Mode = *run.mode
	}
	s.write(s.actions[run.action], t, func(rec []byte) { PutAction(rec, a) })
}

// control records a decision on the DUT, with the state it leaves. Called
// with the lock.
func (s *Server) control(t time.Time, req record.ControlRequest, v record.ControlVerdict, seq uint32, by, channel string) {
	if s.err != nil || s.closed {
		return
	}
	c := &record.Control{Seq: seq, Request: req, Verdict: v, By: by, Via: record.ViaTLS, Instrument: instrument,
		Kind: "dut", Channel: channel, Armed: s.armed}
	if by == "" {
		c.Via = record.ViaDaemon
	}
	if s.lease != nil {
		c.Holder, c.TTL = s.lease.holder, s.lease.ttl
	}
	t = t.Round(0)
	if err := s.sub.Control(t, c); err != nil {
		s.err = err
		return
	}
	s.toFile(fileOp{t: t, ctl: c})
}

// Run evaluates the monitors and keeps the recording until ctx ends, then
// stops the action under way and closes the recording. It returns the
// first error writing the recording, which also ends it.
func (s *Server) Run(ctx context.Context) error {
	go func() {
		s.online.Run(s.monIn, s.out)
		close(s.out)
	}()
	tick := time.NewTicker(s.cfg.Tick)
	defer tick.Stop()
	flush := time.NewTicker(200 * time.Millisecond)
	defer flush.Stop()
	for {
		select {
		case <-ctx.Done():
			return s.shutdown()
		case <-tick.C:
			s.mu.Lock()
			now := s.cfg.Now()
			s.tap.SetClock(now)
			s.tap.Tick(now)
			s.expire(now)
			err := s.err
			s.mu.Unlock()
			if err != nil {
				s.shutdown()
				return err
			}
		case o := <-s.out:
			s.mu.Lock()
			s.output(o)
			s.mu.Unlock()
		case <-flush.C:
			s.mu.Lock()
			if s.err == nil && !s.closed {
				s.toFile(fileOp{flush: true})
			}
			s.mu.Unlock()
		}
	}
}

// output records what the evaluator produced: its transitions, the gap
// that caused some of them, then the watermark over all of it.
func (s *Server) output(o monitor.Output) {
	for _, x := range o.Transitions {
		s.writeTransition(x)
	}
	if g := o.Gap; g != nil {
		s.write(s.gap, time.Unix(0, g.T), func(rec []byte) { PutGap(rec, g) })
	}
	if wm := o.Watermark; wm != 0 && wm > s.lastWM {
		s.lastWM = wm
		s.write(s.wm, time.Unix(0, wm), func(rec []byte) { PutWatermark(rec, wm) })
	}
}

// expire ends a lease whose term ran out. It stops nothing (§6.3).
func (s *Server) expire(now time.Time) {
	if s.lease != nil && now.After(s.lease.expires) {
		s.lease = nil
		s.seq++
		s.control(now, record.ControlLeaseExpired, record.VerdictAccepted, s.seq, "", "")
	}
}

func (s *Server) shutdown() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return s.err
	}
	if s.run != nil {
		s.run.stopped = "the DUT rack stopped"
		s.run.cancel()
	}
	s.control(s.cfg.Now(), record.ControlShutdown, record.VerdictAccepted, 0, "", "")
	s.tap.Close()
	s.mu.Unlock()
	// What the evaluator still had, recorded; then nothing more.
	for o := range s.out {
		s.mu.Lock()
		s.output(o)
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.closed = true
	err := s.err
	close(s.fileOps)
	s.mu.Unlock()
	<-s.fileDone
	if e := s.fileError(); err == nil {
		err = e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.sub.Close(); err == nil {
		err = e
	}
	s.hub.Close()
	return err
}

// # The REST interface (§6)

// Handler serves §6 and §7 under the Config's Auth. The host serves it over
// TLS.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /dut/contract", s.getContract)
	m.HandleFunc("GET /clock", s.getClock)
	m.HandleFunc("POST /dut/lease", s.leaseTake)
	m.HandleFunc("PUT /dut/lease", s.leaseRenew)
	m.HandleFunc("DELETE /dut/lease", s.leaseRelease)
	m.HandleFunc("POST /dut/arm", s.arm)
	m.HandleFunc("POST /dut/disarm", s.disarm)
	m.HandleFunc("PUT /dut/mode", s.setMode)
	m.HandleFunc("POST /dut/actions/{action}", s.invoke)
	m.HandleFunc("GET /stream/sub", s.subscribe)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		tok = strings.TrimSpace(tok)
		if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dut"`)
			reply(w, http.StatusUnauthorized, map[string]any{"error": "this needs a token: Authorization: Bearer <token>"})
			return
		}
		c, ok := s.cfg.Auth(tok)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="dut", error="invalid_token"`)
			reply(w, http.StatusUnauthorized, map[string]any{"error": "the token is not one this rack admits"})
			return
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

type callerKey struct{}

func callerOf(r *http.Request) Caller { c, _ := r.Context().Value(callerKey{}).(Caller); return c }

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func refuse(w http.ResponseWriter, code int, seq uint32, format string, a ...any) {
	body := map[string]any{"error": fmt.Sprintf(format, a...)}
	if seq != 0 {
		body["seq"] = seq
	}
	reply(w, code, body)
}

// decode reads a request body strictly; an empty body is no keys.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		refuse(w, http.StatusBadRequest, 0, "the body: %v", err)
		return false
	}
	return true
}

func (s *Server) getContract(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, map[string]any{
		"protocol":        peer.Protocol,
		"contract":        s.c.Name,
		"sha256":          s.c.Hash,
		"resource_sha256": s.c.ResourceHash,
		"provenance":      s.provence,
	})
}

func (s *Server) getClock(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "{\"t\":%d}\n", s.cfg.Now().UnixNano())
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request) {
	filter, err := SubFilter(r.URL.Query().Get("ch"))
	if err != nil {
		refuse(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	s.hub.ServeStreams(w, r, filter)
}

// admit decides a request under the lock: the caller's role, then the
// lease, then the arming, as needed. It records a refusal, and returns the
// request's number and false; on true the caller records the acceptance
// with what it did.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, req record.ControlRequest, channel string,
	needLease, needArmed bool) (uint32, bool) {
	c := callerOf(r)
	t := s.cfg.Now()
	s.expire(t)
	s.seq++
	no := func(code int, v record.ControlVerdict, format string, a ...any) (uint32, bool) {
		s.control(t, req, v, s.seq, c.Name, channel)
		refuse(w, code, s.seq, format, a...)
		return s.seq, false
	}
	switch {
	case s.closed:
		refuse(w, http.StatusServiceUnavailable, 0, "the DUT rack is stopping")
		return 0, false
	case !c.Operate:
		return no(http.StatusForbidden, record.VerdictForbidden,
			"%q has the view role, which may read the streams and disarm, and nothing more", c.Name)
	case needLease && (s.lease == nil ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Ktest-Lease")), []byte(s.lease.token)) != 1):
		return no(http.StatusForbidden, record.VerdictNoLease,
			"this needs the DUT's lease: take one, and send its token as the Ktest-Lease header")
	case needArmed && !s.armed:
		return no(http.StatusConflict, record.VerdictNotArmed, "the DUT is not armed: POST /dut/arm first")
	}
	return s.seq, true
}

func (s *Server) leaseTake(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Holder string `json:"holder"`
		TTL    string `json:"ttl"`
	}
	if !decode(w, r, &body) {
		return
	}
	c := callerOf(r)
	if body.Holder != "" && body.Holder != c.Name {
		refuse(w, http.StatusBadRequest, 0, "the holder is the name you authenticated as, %q", c.Name)
		return
	}
	ttl, err := parseTTL(body.TTL)
	if err != nil {
		refuse(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.admit(w, r, record.ControlLeaseTake, "", false, false)
	if !ok {
		return
	}
	t := s.cfg.Now()
	if s.lease != nil {
		s.control(t, record.ControlLeaseTake, record.VerdictLeaseHeld, seq, c.Name, "")
		refuse(w, http.StatusConflict, seq, "%s holds the DUT's lease for another %v", s.lease.holder,
			s.lease.expires.Sub(t).Round(time.Second))
		return
	}
	s.lease = &lease{token: newToken(), holder: c.Name, ttl: ttl, expires: t.Add(ttl)}
	s.control(t, record.ControlLeaseTake, record.VerdictAccepted, seq, c.Name, "")
	reply(w, http.StatusCreated, map[string]any{"seq": seq, "token": s.lease.token, "ttl": ttl.String()})
}

func (s *Server) leaseRenew(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TTL string `json:"ttl"`
	}
	if !decode(w, r, &body) {
		return
	}
	var ttl time.Duration
	if body.TTL != "" {
		var err error
		if ttl, err = parseTTL(body.TTL); err != nil {
			refuse(w, http.StatusBadRequest, 0, "%v", err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.admit(w, r, record.ControlLeaseRenew, "", true, false)
	if !ok {
		return
	}
	if ttl > 0 {
		s.lease.ttl = ttl
	}
	t := s.cfg.Now()
	s.lease.expires = t.Add(s.lease.ttl)
	s.control(t, record.ControlLeaseRenew, record.VerdictAccepted, seq, callerOf(r).Name, "")
	reply(w, http.StatusAccepted, map[string]any{"seq": seq, "ttl": s.lease.ttl.String()})
}

func (s *Server) leaseRelease(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.admit(w, r, record.ControlLeaseRelease, "", true, false)
	if !ok {
		return
	}
	s.lease = nil
	s.control(s.cfg.Now(), record.ControlLeaseRelease, record.VerdictAccepted, seq, callerOf(r).Name, "")
	reply(w, http.StatusAccepted, map[string]any{"seq": seq})
}

func (s *Server) arm(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.admit(w, r, record.ControlArm, "", true, false)
	if !ok {
		return
	}
	s.armed = true
	s.control(s.cfg.Now(), record.ControlArm, record.VerdictAccepted, seq, callerOf(r).Name, "")
	reply(w, http.StatusAccepted, map[string]any{"seq": seq})
}

// disarm needs no lease and no operate role: stopping is always allowed
// (§6.4). It ends the action under way, and tells the host.
func (s *Server) disarm(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		refuse(w, http.StatusServiceUnavailable, 0, "the DUT rack is stopping")
		return
	}
	s.seq++
	seq := s.seq
	s.armed = false
	if s.run != nil {
		s.run.stopped = "the DUT was disarmed by " + callerOf(r).Name
		s.run.cancel()
	}
	s.control(s.cfg.Now(), record.ControlDisarm, record.VerdictAccepted, seq, callerOf(r).Name, "")
	s.mu.Unlock()
	if s.cfg.OnDisarm != nil {
		s.cfg.OnDisarm()
	}
	reply(w, http.StatusAccepted, map[string]any{"seq": seq})
}

func (s *Server) setMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value *string `json:"value"`
	}
	if !decode(w, r, &body) {
		return
	}
	switch {
	case body.Value == nil:
		refuse(w, http.StatusBadRequest, 0, `"value" is required: the mode, or "" for none`)
		return
	case *body.Value != "" && s.c.Modes[*body.Value] == nil:
		refuse(w, http.StatusNotFound, 0, "the contract has no mode %q; its modes are %s", *body.Value,
			strings.Join(slices.Sorted(maps.Keys(s.c.Modes)), ", "))
		return
	}
	mode := *body.Value
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.admit(w, r, record.ControlMode, mode, true, true)
	if !ok {
		return
	}
	by, t := callerOf(r).Name, s.cfg.Now()
	if s.run != nil {
		s.control(t, record.ControlMode, record.VerdictBusy, seq, by, mode)
		refuse(w, http.StatusConflict, seq, "the DUT is busy: action %s is under way", s.run.action)
		return
	}
	s.control(t, record.ControlMode, record.VerdictAccepted, seq, by, mode)
	s.requested = mode
	s.writeMode(t, ModeRecord{Seq: seq, Event: ModeRequested, By: by})
	action := ""
	if m := s.c.Modes[mode]; m != nil {
		action = m.Action
	}
	if action == "" {
		s.applyMode(mode, seq, by, t)
	} else {
		s.start(&actionRun{action: action, mode: &mode, seq: seq, by: by}, t)
	}
	reply(w, http.StatusAccepted, map[string]any{"seq": seq})
}

func (s *Server) invoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("action")
	if _, ok := s.c.Actions[name]; !ok {
		refuse(w, http.StatusNotFound, 0, "the contract has no action %q; its actions are %s", name,
			strings.Join(slices.Sorted(maps.Keys(s.c.Actions)), ", "))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, ok := s.admit(w, r, record.ControlAction, name, true, true)
	if !ok {
		return
	}
	by, t := callerOf(r).Name, s.cfg.Now()
	if s.run != nil {
		s.control(t, record.ControlAction, record.VerdictBusy, seq, by, name)
		refuse(w, http.StatusConflict, seq, "the DUT is busy: action %s is under way", s.run.action)
		return
	}
	s.control(t, record.ControlAction, record.VerdictAccepted, seq, by, name)
	s.start(&actionRun{action: name, seq: seq, by: by}, t)
	reply(w, http.StatusAccepted, map[string]any{"seq": seq})
}

// applyMode puts the DUT in a mode from t: recorded, and handed to the
// monitors, which settle from t for the mode's settle time. Called with the
// lock.
func (s *Server) applyMode(mode string, seq uint32, by string, t time.Time) {
	// The wall clock alone: the monitors take Unix nanoseconds, and a
	// monotonic reading would put the mode a few nanoseconds off the
	// recording's own account of t.
	t = t.Round(0)
	s.applied = mode
	s.writeMode(t, ModeRecord{Seq: seq, Event: ModeApplied, By: by})
	s.tap.Mode(s.tap.Clock(), t, mode)
}

// start runs an action on a goroutine of its own. Called with the lock.
func (s *Server) start(run *actionRun, t time.Time) {
	s.runs++
	run.n = s.runs
	timeout := s.cfg.ActionTimeout
	if d, ok := s.c.Timeouts[run.action]; ok && d > 0 {
		timeout = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	run.cancel = cancel
	s.run = run
	s.writeAction(t, run, ActionStarted, "", "")
	go func() {
		err := s.cfg.Actions.Run(ctx, run.action)
		deadline := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		s.end(run, err, deadline, timeout)
	}()
}

// end records an action's end, and applies the mode it entered, or says it
// failed.
func (s *Server) end(run *actionRun, err error, deadline bool, timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.cfg.Now()
	if s.run == run {
		s.run = nil
	}
	o, msg := peer.OK, ""
	var f *peer.Failure
	switch {
	case run.stopped != "":
		o, msg = peer.Stopped, run.stopped
	case deadline:
		o, msg = peer.Timeout, fmt.Sprintf("action %s did not end within its timeout, %v", run.action, timeout)
	case errors.As(err, &f):
		o, msg = f.Outcome, f.Msg
	case err != nil:
		o, msg = peer.Fail, err.Error()
	}
	s.writeAction(t, run, ActionEnded, o, msg)
	if run.mode == nil {
		return
	}
	if o == peer.OK {
		s.applyMode(*run.mode, run.seq, run.by, t)
		return
	}
	s.writeMode(t, ModeRecord{Seq: run.seq, Event: ModeFailed, By: run.by,
		Message: fmt.Sprintf("entry action %s: %s: %s", run.action, o, msg)})
}

func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second || d > time.Hour {
		return 0, fmt.Errorf("ttl %q: a duration from 1s to 1h", s)
	}
	return d, nil
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
