package conformance

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/internal/faultinject"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
	"github.com/trukeio/ktest/secondary"
)

// MasterConfig is the reference DUT rack a master is checked against.
type MasterConfig struct {
	// Addr is where the reference rack listens: host:port.
	Addr string
	// Dir is where it writes what the master needs: the contract and its
	// resource, its certificate, and the master's token; and its recording.
	Dir string
	// Phase is how long each fault lasts, and the quiet between them; 0 is
	// 3 s. It must be longer than the master's lease renewal and clock
	// sampling periods for those to be seen.
	Phase time.Duration
	// Connect is how long the master has to connect, take the lease, arm,
	// and subscribe; 0 is 2 minutes.
	Connect time.Duration
	// Reconnect is how long the master has to come back after a fault; 0
	// is 15 s.
	Reconnect time.Duration
	// Say, if set, is told what the run is doing.
	Say func(string)
	// Ready, if set, is told the rack's URL once it listens: with Addr's
	// port 0, the port it was given.
	Ready func(url string)
}

// The reference rack's contract: a heartbeat, a supply the rack sets, an
// abort monitor on it, one mode and two actions.
const referenceContract = `{
  "contract": "conformance-reference",
  "schema": 1,
  "inputs":   {"Beat": {"channel": "Beat", "max_age": "1s"}, "Vx": {"channel": "Supply.v", "max_age": "1s"}},
  "monitors": {
    "alive": {"ok": "age(Beat) < 50ms"},
    "abort": {"ok": "Vx < 16V"}
  },
  "modes":    {"active": {"action": "enter", "settle": "50ms"}},
  "resource": "actions.txt",
  "actions":  {"enter": "enter", "reset": "reset"}
}
`

const referenceActions = "The reference rack's actions: enter and reset, both of which do nothing and succeed.\n"

var (
	beatSchema = record.Schema("Beat", 8*16, []logb.Field{
		{Name: "n", BitOffset: 64, BitWidth: 32, Type: logb.TypeUint},
	}, nil)
	refSupplySchema = record.Schema("Supply", 8*16, []logb.Field{
		{Name: "v", BitOffset: 64, BitWidth: 64, Type: logb.TypeFloat, Unit: "V"},
	}, nil)
)

// Master runs the reference rack until the master under test has been
// through every phase, or ctx ends, and reports what it saw.
func Master(ctx context.Context, cfg MasterConfig) (*Report, error) {
	if cfg.Phase <= 0 {
		cfg.Phase = 3 * time.Second
	}
	if cfg.Connect <= 0 {
		cfg.Connect = 2 * time.Minute
	}
	if cfg.Reconnect <= 0 {
		cfg.Reconnect = 15 * time.Second
	}
	if cfg.Say == nil {
		cfg.Say = func(string) {}
	}
	m := &masterRun{cfg: cfg, r: &Report{Subject: "master, against the reference DUT rack at https://" + cfg.Addr}}
	if err := m.setUp(); err != nil {
		return nil, err
	}
	defer m.tearDown()
	m.run(ctx)
	return m.r, nil
}

type masterRun struct {
	cfg  MasterConfig
	r    *Report
	addr string // where it listens

	srv     *secondary.Server
	srvStop context.CancelFunc
	srvDone chan error
	rec     *os.File
	hs      *http.Server
	genStop chan struct{}
	genDone sync.WaitGroup
	volts   atomic.Uint64

	mu       sync.Mutex
	log      []request
	subs     map[*http.Request]context.CancelFunc
	down     bool           // every request answered 503, every subscription ended
	override map[string]any // GET /dut/contract's answer changed so
	changed  chan struct{}  // closed and remade on every request
}

// request is one request the master made, as the reference rack answered it.
type request struct {
	at     time.Time
	method string
	path   string
	query  string
	status int
	body   map[string]any // a lease request's
}

func (m *masterRun) setUp() error {
	if err := os.MkdirAll(m.cfg.Dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{"dut-interface.json": referenceContract, "actions.txt": referenceActions}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(m.cfg.Dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	tb := make([]byte, 32)
	rand.Read(tb)
	token := hex.EncodeToString(tb)
	if err := os.WriteFile(filepath.Join(m.cfg.Dir, "master.token"), []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	cert, err := secondary.SelfSigned(filepath.Join(m.cfg.Dir, "cert.pem"), 7*24*time.Hour, "localhost", "127.0.0.1", "::1")
	if err != nil {
		return err
	}
	if m.rec, err = os.Create(filepath.Join(m.cfg.Dir, "reference.logb")); err != nil {
		return err
	}
	m.setVolts(12)
	m.srv, err = secondary.New(secondary.Config{
		Contract:  filepath.Join(m.cfg.Dir, "dut-interface.json"),
		Streams:   []*logb.Schema{beatSchema, refSupplySchema},
		Actions:   secondary.ActionFunc(func(context.Context, string) error { return nil }),
		Auth:      secondary.Tokens(map[string]secondary.Caller{token: {Name: "master", Operate: true}}),
		Recording: m.rec,
		Name:      "conformance-reference",
		Source:    "the conformance kit's reference rack",
		Meta:      map[string]string{"daemon.version": "ktest conformance reference rack"},
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.srvStop, m.srvDone = cancel, make(chan error, 1)
	go func() { m.srvDone <- m.srv.Run(ctx) }()
	m.genStop = make(chan struct{})
	m.genDone.Add(1)
	go m.generate()

	m.subs, m.changed = map[*http.Request]context.CancelFunc{}, make(chan struct{})
	ln, err := tls.Listen("tcp", m.cfg.Addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	m.addr = ln.Addr().String()
	m.r.Subject = "master, against the reference DUT rack at https://" + m.addr
	m.hs = &http.Server{Handler: m.observe(m.srv.Handler())}
	go m.hs.Serve(ln)
	return nil
}

func (m *masterRun) tearDown() {
	m.hs.Close()
	close(m.genStop)
	m.genDone.Wait()
	m.srvStop()
	<-m.srvDone
	m.rec.Close()
}

func (m *masterRun) setVolts(v float64) { m.volts.Store(math.Float64bits(v)) }

// generate is the reference DUT: a heartbeat and a supply reading every
// 5 ms.
func (m *masterRun) generate() {
	defer m.genDone.Done()
	beat, supply := m.srv.Stream("Beat"), m.srv.Stream("Supply")
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var n uint32
	for {
		select {
		case <-m.genStop:
			return
		case t := <-tick.C:
			n++
			v := m.volts.Load()
			m.srv.Write(beat, t, func(rec []byte) { binary.LittleEndian.PutUint32(rec[8:], n) })
			m.srv.Write(supply, t, func(rec []byte) { binary.LittleEndian.PutUint64(rec[8:], v) })
		}
	}
}

// observe logs every request the master makes, and carries out the faults.
func (m *masterRun) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := request{at: time.Now(), method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if r.URL.Path == "/dut/lease" && r.Method == "POST" {
			// Read for the ttl asked for, and handed on as it was.
			var body map[string]any
			b := make([]byte, 0, 512)
			buf := make([]byte, 512)
			for {
				n, err := r.Body.Read(buf)
				b = append(b, buf[:n]...)
				if err != nil {
					break
				}
			}
			json.Unmarshal(b, &body)
			req.body = body
			r.Body = readCloser{strings.NewReader(string(b))}
		}
		m.mu.Lock()
		down, override := m.down, m.override
		m.mu.Unlock()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		switch {
		case down:
			sw.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(sw, `{"error":"the reference rack is down, for the conformance run"}`)
		case override != nil && r.URL.Path == "/dut/contract":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(sw).Encode(override)
		case r.URL.Path == "/stream/sub":
			ctx, cancel := context.WithCancel(r.Context())
			m.mu.Lock()
			m.subs[r] = cancel
			m.mu.Unlock()
			m.record(req) // as it opens; its status is the 200 of a stream
			next.ServeHTTP(sw, r.WithContext(ctx))
			m.mu.Lock()
			delete(m.subs, r)
			m.mu.Unlock()
			cancel()
			return
		default:
			next.ServeHTTP(sw, r)
		}
		req.status = sw.status
		m.record(req)
	})
}

func (m *masterRun) record(req request) {
	if req.status == 0 {
		req.status = http.StatusOK
	}
	m.mu.Lock()
	m.log = append(m.log, req)
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// setDown takes the rack off the network, ending every subscription, or
// puts it back.
func (m *masterRun) setDown(down bool) {
	m.mu.Lock()
	m.down = down
	if down {
		for _, cancel := range m.subs {
			cancel()
		}
	}
	m.mu.Unlock()
}

func (m *masterRun) event(format string, a ...any) {
	m.r.Timeline = append(m.r.Timeline, Event{time.Now(), fmt.Sprintf(format, a...)})
	m.cfg.Say(fmt.Sprintf(format, a...))
}

// since are the requests made at or after t.
func (m *masterRun) since(t time.Time) []request {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []request
	for _, x := range m.log {
		if !x.at.Before(t) {
			out = append(out, x)
		}
	}
	return out
}

// waitFor waits until the requests since t satisfy done, or the timeout.
func (m *masterRun) waitFor(ctx context.Context, t time.Time, timeout time.Duration, done func([]request) bool) bool {
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		ch := m.changed
		m.mu.Unlock()
		if done(m.since(t)) {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func first(log []request, pred func(request) bool) int {
	for i, x := range log {
		if pred(x) {
			return i
		}
	}
	return -1
}

func isContract(x request) bool { return x.method == "GET" && x.path == "/dut/contract" }
func isSub(x request) bool      { return x.path == "/stream/sub" }
func isLeaseTake(x request) bool {
	return x.method == "POST" && x.path == "/dut/lease" && x.status == http.StatusCreated
}
func isArm(x request) bool { return x.method == "POST" && x.path == "/dut/arm" && x.status < 300 }

// proceeds is a request a master makes only once it has accepted the rack:
// a subscription, a lease, an arming, a mode, an action.
func proceeds(x request) bool {
	return isSub(x) || (x.method == "POST" && (x.path == "/dut/lease" || x.path == "/dut/arm" ||
		strings.HasPrefix(x.path, "/dut/actions/"))) || (x.method == "PUT" && x.path == "/dut/mode")
}

func (m *masterRun) run(ctx context.Context) {
	start := time.Now()
	if m.cfg.Ready != nil {
		m.cfg.Ready("https://" + m.addr)
	}
	m.event("the reference rack is up: point the master at it with "+
		"-peer ref=https://%s,token=%s,cacert=%s,contract=%s", m.addr,
		filepath.Join(m.cfg.Dir, "master.token"), filepath.Join(m.cfg.Dir, "cert.pem"),
		filepath.Join(m.cfg.Dir, "dut-interface.json"))

	// Connect: §9.1.
	connected := m.waitFor(ctx, start, m.cfg.Connect, func(log []request) bool {
		return first(log, isSub) >= 0 && first(log, isLeaseTake) >= 0 && first(log, isArm) >= 0
	})
	if !connected {
		log := m.since(start)
		m.r.fail("§9.1", "the master connects: subscribes, takes the lease and arms", "within %v: %d requests, subscription %v, lease %v, armed %v",
			m.cfg.Connect, len(log), first(log, isSub) >= 0, first(log, isLeaseTake) >= 0, first(log, isArm) >= 0)
		return
	}
	m.r.pass("§9.1", "the master connects: subscribes, takes the lease and arms", "")
	m.event("the master is connected")
	log := m.since(start)
	c, s, l := first(log, isContract), first(log, isSub), first(log, isLeaseTake)
	if c < 0 || c > s || c > l {
		m.r.fail("§9.1", "it asks for the contract before it subscribes or takes the lease", "contract at %d, subscription at %d, lease at %d", c, s, l)
	} else {
		m.r.pass("§9.1", "it asks for the contract before it subscribes or takes the lease", "")
	}
	m.checkSubscription(log)

	// Steady: §8, §6.3.
	steady := time.Now()
	m.sleep(ctx, max(m.cfg.Phase, 3*time.Second))
	m.checkClock(steady, time.Now())

	// Faults: §9.4. What a master does about them is its own; the
	// timeline says when they were.
	m.begin(FaultAbort)
	m.setVolts(17)
	m.event("FAULT abort: the supply reads 17 V, so mon.abort goes dev")
	m.sleep(ctx, m.cfg.Phase)
	m.setVolts(12)
	m.end()
	m.event("cleared: the supply reads 12 V, so mon.abort goes ok")
	m.sleep(ctx, m.cfg.Phase)

	m.event("FAULT frozen: the rack stands still, and its watermark stops")
	m.begin(FaultFrozen)
	faultinject.Freeze(m.srv, m.cfg.Phase)
	m.end()
	m.event("cleared: the rack runs again")
	m.sleep(ctx, m.cfg.Phase)

	// The link lost, and back: §9.3.
	m.begin(FaultLink)
	m.setDown(true)
	m.event("FAULT link lost: every subscription ended, every request answered 503")
	m.sleep(ctx, m.cfg.Phase)
	up := time.Now()
	m.setDown(false)
	m.end()
	m.event("cleared: the rack answers again")
	m.checkReconnect(ctx, up, "after the link is lost")

	// Another contract, and another protocol: §9.1 again, on reconnection.
	for _, f := range []struct {
		name     string
		kind     string
		override func(map[string]any)
	}{
		{"another contract", FaultContract, func(o map[string]any) { o["sha256"] = strings.Repeat("0", 64) }},
		{"another protocol version", FaultProtocol, func(o map[string]any) { o["protocol"] = peer.Protocol + 1 }},
	} {
		o := m.contractAnswer()
		f.override(o)
		m.mu.Lock()
		m.override = o
		m.mu.Unlock()
		m.event("FAULT %s: the rack drops its link, and comes back 200 ms later stating it", f.name)
		m.begin(f.kind)
		m.setDown(true) // the subscriptions end, as a restarted rack's would
		m.sleep(ctx, 200*time.Millisecond)
		from := time.Now()
		m.setDown(false)
		asked := m.waitFor(ctx, from, m.cfg.Reconnect, func(log []request) bool { return first(log, isContract) >= 0 })
		m.sleep(ctx, m.cfg.Phase)
		log := m.since(from)
		title := "the master does not proceed with a rack stating " + f.name
		switch i := first(log, proceeds); {
		case !asked:
			m.r.skip("§9.1", title, "it did not ask for the contract within %v of the rack's return", m.cfg.Reconnect)
		case i >= 0:
			m.r.fail("§9.1", title, "it made %s %s", log[i].method, log[i].path)
		default:
			m.r.pass("§9.1", title, "it asked for the contract %d times, and made no other request that proceeds",
				countOf(log, isContract))
		}
		m.mu.Lock()
		m.override = nil
		m.mu.Unlock()
		m.end()
		back := time.Now()
		m.event("cleared: the rack states its own contract again")
		m.checkReconnect(ctx, back, "after "+f.name)
	}
	m.checkLease(start)
	m.event("done")
	if b, err := json.MarshalIndent(FaultFile{Rack: "https://" + m.addr, Faults: m.r.Faults}, "", "  "); err == nil {
		os.WriteFile(filepath.Join(m.cfg.Dir, "faults.json"), append(b, '\n'), 0o644)
	}
}

// begin and end mark a fault on the reference rack's clock.
func (m *masterRun) begin(kind string) {
	m.r.Faults = append(m.r.Faults, Fault{Kind: kind, Start: time.Now()})
}

func (m *masterRun) end() { m.r.Faults[len(m.r.Faults)-1].End = time.Now() }

func countOf(log []request, pred func(request) bool) int {
	n := 0
	for _, x := range log {
		if pred(x) {
			n++
		}
	}
	return n
}

// contractAnswer is GET /dut/contract as the rack answers it.
func (m *masterRun) contractAnswer() map[string]any {
	c := m.srv.Contract()
	return map[string]any{"protocol": peer.Protocol, "contract": c.Name, "sha256": c.Hash,
		"resource_sha256": c.ResourceHash, "provenance": map[string]string{"daemon.version": "ktest conformance reference rack"}}
}

func (m *masterRun) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// §9.2–9.4: the master sees what it must: the abort monitor, through the
// subscription; the watermark comes with any.
func (m *masterRun) checkSubscription(log []request) {
	i := first(log, isSub)
	var globs []string
	for _, q := range strings.Split(log[i].query, "&") {
		if v, ok := strings.CutPrefix(q, "ch="); ok {
			if u, err := url.QueryUnescape(v); err == nil {
				globs = strings.Split(u, ",")
			}
		}
	}
	for _, g := range globs {
		if ok, _ := path.Match(strings.TrimSpace(g), "mon.abort"); ok {
			m.r.pass("§9.4", "the master subscribes to the abort monitor", "ch=%s", strings.Join(globs, ","))
			return
		}
	}
	m.r.fail("§9.4", "the master subscribes to the abort monitor", "ch=%s", strings.Join(globs, ","))
}

// §8: the clock is sampled at least once a second.
func (m *masterRun) checkClock(from, to time.Time) {
	var ts []time.Time
	for _, x := range m.since(from) {
		if x.path == "/clock" && x.at.Before(to) {
			ts = append(ts, x.at)
		}
	}
	gap := to.Sub(from)
	if len(ts) > 0 {
		gap = max(ts[0].Sub(from), to.Sub(ts[len(ts)-1]))
		for i := 1; i < len(ts); i++ {
			gap = max(gap, ts[i].Sub(ts[i-1]))
		}
	}
	// A second, and the scheduling of a busy machine.
	if gap > 1500*time.Millisecond {
		m.r.fail("§8", "the clock is sampled at least once a second", "%d samples in %v, the longest gap %v",
			len(ts), to.Sub(from).Round(time.Millisecond), gap.Round(time.Millisecond))
	} else {
		m.r.pass("§8", "the clock is sampled at least once a second", "%d samples in %v", len(ts), to.Sub(from).Round(time.Millisecond))
	}
}

// §9.3: on reconnection, the contract is asked for before anything
// proceeds, and the master does reconnect.
func (m *masterRun) checkReconnect(ctx context.Context, from time.Time, when string) {
	ok := m.waitFor(ctx, from, m.cfg.Reconnect, func(log []request) bool { return first(log, isSub) >= 0 })
	if !ok {
		m.r.fail("§9.3", "the master reconnects "+when, "no subscription within %v", m.cfg.Reconnect)
		return
	}
	log := m.since(from)
	c, p := first(log, isContract), first(log, proceeds)
	if c < 0 || c > p {
		m.r.fail("§9.3", "on reconnecting "+when+", it asks for the contract first", "it made %s %s first", log[p].method, log[p].path)
		return
	}
	m.r.pass("§9.3", "on reconnecting "+when+", it asks for the contract first", "")
	m.event("the master is connected again")
	// A quiet phase, for the master to recover in: its rules clear once the
	// watermark is fresh again, which its recording should show.
	m.sleep(ctx, m.cfg.Phase)
}

// §9.2: the lease is renewed within its term whenever the master holds it.
func (m *masterRun) checkLease(from time.Time) {
	var expires time.Time
	var ttl time.Duration
	lapsed := ""
	renewals := 0
	for _, x := range m.since(from) {
		if !expires.IsZero() && x.at.After(expires) && lapsed == "" && !m.inFault(expires) {
			lapsed = fmt.Sprintf("its %v lease ran out at %s", ttl, expires.UTC().Format("15:04:05.000"))
		}
		switch {
		case isLeaseTake(x):
			ttl = 30 * time.Second
			if s, ok := x.body["ttl"].(string); ok {
				if d, err := time.ParseDuration(s); err == nil {
					ttl = d
				}
			}
			expires = x.at.Add(ttl)
		case x.method == "PUT" && x.path == "/dut/lease" && x.status < 300:
			expires = x.at.Add(ttl)
			renewals++
		case x.method == "DELETE" && x.path == "/dut/lease":
			expires = time.Time{}
		}
	}
	switch {
	case lapsed != "":
		m.r.fail("§9.2", "the lease is renewed within its term", "%s", lapsed)
	case renewals == 0:
		m.r.skip("§9.2", "the lease is renewed within its term", "no renewal in the run; make -phase longer than a third of the master's lease term")
	default:
		m.r.pass("§9.2", "the lease is renewed within its term", "%d renewals", renewals)
	}
}

// inFault says whether t fell while the rack was down or had another
// contract: a lease that runs out then is not the master's to keep.
func (m *masterRun) inFault(t time.Time) bool {
	var on bool
	for _, e := range m.r.Timeline {
		if e.At.After(t) {
			break
		}
		switch {
		case strings.HasPrefix(e.What, "FAULT link lost"), strings.HasPrefix(e.What, "FAULT another"):
			on = true
		case strings.HasPrefix(e.What, "cleared: the rack answers again"), strings.HasPrefix(e.What, "cleared: the rack states"):
			on = false
		}
	}
	return on
}
