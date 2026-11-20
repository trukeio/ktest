package conformance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/contract"
	"github.com/trukeio/ktest/monitor"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/secondary"
)

// SecondaryConfig is a DUT rack to check, and what the checker may do to it.
type SecondaryConfig struct {
	URL      string // https://host:port
	CACert   string // the rack's certificate, PEM
	Contract string // the checker's copy of the rack's contract
	// Token is a file holding a token with the operate role; required.
	Token string
	// Other is a second operate token's file, for the lease's exclusion;
	// optional.
	Other string
	// View is a view token's file, for what the view role may and may not
	// do; optional.
	View string
	// Mode is a mode to enter and leave; "" is the contract's first. Action
	// is an action to run; "" runs none, since an action does something to
	// the DUT.
	Mode, Action string
	// Wait is how long a mode change or an action may take to end; 0 is
	// 10 s.
	Wait time.Duration
}

// CheckSecondary checks a DUT rack.
func CheckSecondary(ctx context.Context, cfg SecondaryConfig) (*Report, error) {
	if cfg.Wait <= 0 {
		cfg.Wait = 10 * time.Second
	}
	id, err := contract.Identify(cfg.Contract)
	if err != nil {
		return nil, err
	}
	op, err := newClient(cfg, cfg.Token)
	if err != nil {
		return nil, err
	}
	raw, err := rawClient(cfg.CACert)
	if err != nil {
		return nil, err
	}
	tok, err := readToken(cfg.Token)
	if err != nil {
		return nil, err
	}
	c := &secChecker{cfg: cfg, id: id, op: op, raw: raw, tok: tok,
		r: &Report{Subject: fmt.Sprintf("DUT rack %s, contract %s (sha256 %.12s…)", cfg.URL, id.Name, id.Hash)}}
	c.run(ctx)
	return c.r, nil
}

type secChecker struct {
	cfg SecondaryConfig
	id  *contract.Identity
	op  *peer.Client
	raw *http.Client
	tok string
	r   *Report

	sub    *peer.Stream
	items  chan peer.Item
	subErr chan error

	wms      []int64 // the watermarks read, in order
	badState string  // a monitor state code §5.1 does not have, if one was read
}

func (c *secChecker) run(ctx context.Context) {
	c.auth(ctx)
	if !c.contractInfo(ctx) {
		return
	}
	c.clock(ctx)
	if !c.subscribe(ctx) {
		return
	}
	defer c.sub.Close()
	c.watermark()
	c.badGlob(ctx)
	lease := c.lease(ctx)
	if lease == "" {
		return
	}
	c.refusals(ctx, lease)
	c.modeAndAction(ctx, lease)
	c.disarm(ctx, lease)
	c.release(ctx, lease)
}

// §2: no token, or one the rack does not know, is 401 and nothing else.
func (c *secChecker) auth(ctx context.Context) {
	for _, tc := range []struct{ name, header string }{
		{"no token", ""},
		{"an unknown token", "Bearer 0000000000000000000000000000000000000000000000000000000000000000"},
	} {
		code, _, err := c.do(ctx, "GET", "/dut/contract", tc.header, "", nil)
		switch {
		case err != nil:
			c.r.fail("§2", "a request with "+tc.name+" is refused 401", "%v", err)
		case code != http.StatusUnauthorized:
			c.r.fail("§2", "a request with "+tc.name+" is refused 401", "answered %d", code)
		default:
			c.r.pass("§2", "a request with "+tc.name+" is refused 401", "")
		}
	}
}

// §6.2, §11: the contract, its hashes, and the protocol version.
func (c *secChecker) contractInfo(ctx context.Context) bool {
	in, err := c.op.Info(ctx)
	if !c.r.check("§6.2", "GET /dut/contract answers", err) {
		return false
	}
	switch {
	case in.Protocol == 0:
		c.r.fail("§11", `GET /dut/contract states "protocol"`, "it states none, which is read as 1")
	case in.Protocol != peer.Protocol:
		c.r.fail("§11", `GET /dut/contract states "protocol"`, "it states %d; this checks %d", in.Protocol, peer.Protocol)
	default:
		c.r.pass("§11", `GET /dut/contract states "protocol"`, "%d", in.Protocol)
	}
	ok := c.r.check("§3.3", "the contract's sha256 is the checker's copy's", func() error {
		if in.Hash != c.id.Hash {
			return fmt.Errorf("the rack's is %s, the copy's %s", in.Hash, c.id.Hash)
		}
		return nil
	}())
	ok = c.r.check("§3.3", "the resource's sha256 is the checker's copy's", func() error {
		if in.ResourceHash != c.id.ResourceHash {
			return fmt.Errorf("the rack's is %q, the copy's %q", in.ResourceHash, c.id.ResourceHash)
		}
		return nil
	}()) && ok
	if len(in.Provenance) == 0 {
		c.r.fail("§6.2", `GET /dut/contract gives "provenance"`, "it gives none")
	} else {
		c.r.pass("§6.2", `GET /dut/contract gives "provenance"`, "%d keys", len(in.Provenance))
	}
	return ok
}

// §6.7: the clock, uncached, moving forward.
func (c *secChecker) clock(ctx context.Context) {
	code, h, err := c.do(ctx, "GET", "/clock", "Bearer "+c.tok, "", nil)
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("answered %d", code)
	}
	if err == nil && !strings.Contains(h.Get("Cache-Control"), "no-store") {
		err = fmt.Errorf("Cache-Control is %q", h.Get("Cache-Control"))
	}
	c.r.check("§6.7", "GET /clock answers 200, not to be cached", err)
	var last int64
	var minRTT time.Duration = time.Hour
	for i := 0; i < 5; i++ {
		s, err := c.op.Clock(ctx)
		if err != nil {
			c.r.fail("§6.7", "the clock moves forward", "%v", err)
			return
		}
		if s.Peer <= last {
			c.r.fail("§6.7", "the clock moves forward", "%d after %d", s.Peer, last)
			return
		}
		last, minRTT = s.Peer, min(minRTT, s.RTT())
	}
	c.r.pass("§6.7", "the clock moves forward", "shortest round trip %v", minRTT.Round(time.Microsecond))
}

// §7: the subscription: Logb, its metadata, every monitor's stream and the
// mode's as §7.4 lays them out.
func (c *secChecker) subscribe(ctx context.Context) bool {
	s, err := c.op.Subscribe(ctx, []string{"mon.*", "dut.action.*"})
	if !c.r.check("§7.1", "GET /stream/sub answers a Logb stream", err) {
		return false
	}
	c.sub, c.items, c.subErr = s, make(chan peer.Item, 1<<16), make(chan error, 1)
	go func() {
		for {
			it, err := s.Next()
			if err != nil {
				c.subErr <- err
				return
			}
			c.items <- it
		}
	}()
	// A second of it: the schemas, the held values restated, and enough
	// watermarks to judge their cadence.
	seen := map[string]*logb.Schema{}
	deadline := time.After(time.Second)
	c.wms = nil
read:
	for {
		select {
		case it := <-c.items:
			seen[it.Schema.Name] = it.Schema
			c.note(it)
		case err := <-c.subErr:
			c.r.fail("§7.1", "the subscription stays open", "%v", err)
			return false
		case <-deadline:
			break read
		}
	}
	meta := map[string]string{}
	for _, m := range s.Meta() {
		meta[m.Key] = m.Value
	}
	if meta["recording.epoch"] == "" {
		c.r.fail("§7.2", "the stream states recording.epoch", "it does not")
	} else {
		c.r.pass("§7.2", "the stream states recording.epoch", "")
	}
	if meta["peer.protocol"] != fmt.Sprint(peer.Protocol) {
		c.r.fail("§7.3", "the stream states peer.protocol", "%q", meta["peer.protocol"])
	} else {
		c.r.pass("§7.3", "the stream states peer.protocol", "%s", meta["peer.protocol"])
	}
	c.layouts(seen)
	return true
}

// wms are the watermarks read so far: their values, and the records' own
// times.
func (c *secChecker) note(it peer.Item) {
	if it.Schema.Name == "mon.watermark" && !it.Restated {
		c.wms = append(c.wms, int64(peer.Num(&it, "upto")))
	}
	if strings.HasPrefix(it.Schema.Name, "mon.") && it.Schema.Name != "mon.watermark" && it.Schema.Name != "mon.gap" {
		st := it.Rec[8]
		if st > 4 && c.badState == "" {
			c.badState = fmt.Sprintf("%s has state code %d", it.Schema.Name, st)
		}
	}
}

// layouts holds each stream to §7.4: the fields a reader reads by name,
// at the offsets, widths and types given there.
func (c *secChecker) layouts(seen map[string]*logb.Schema) {
	mc := &monitor.Contract{Name: c.id.Name, Monitors: map[string]*monitor.Monitor{}}
	for _, m := range c.id.Monitors {
		mc.Monitors[m] = &monitor.Monitor{Name: m}
	}
	want := map[string]*logb.Schema{
		"mon.watermark": secondary.WatermarkSchema(""),
		"mode":          secondary.ModeSchema(""),
	}
	for _, m := range c.id.Monitors {
		want["mon."+m] = secondary.MonSchema(mc, m, "")
	}
	var missing, wrong []string
	for _, name := range slices.Sorted(mapsKeys(want)) {
		got := seen[name]
		if got == nil {
			missing = append(missing, name)
			continue
		}
		if err := sameLayout(want[name], got); err != nil {
			wrong = append(wrong, err.Error())
		}
	}
	if len(missing) > 0 {
		c.r.fail("§7.1", "a subscriber is sent every monitor, the watermark and the mode", "no %s within a second",
			strings.Join(missing, ", "))
	} else {
		c.r.pass("§7.1", "a subscriber is sent every monitor, the watermark and the mode", "%d streams", len(want))
	}
	if len(wrong) > 0 {
		c.r.fail("§7.4", "the streams are laid out as §7.4 says", "%s", strings.Join(wrong, "; "))
	} else if len(missing) < len(want) {
		c.r.pass("§7.4", "the streams are laid out as §7.4 says", "")
	}
	if c.badState != "" {
		c.r.fail("§5.1", "monitor states are codes 0 to 4", "%s", c.badState)
	} else {
		c.r.pass("§5.1", "monitor states are codes 0 to 4", "")
	}
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// sameLayout compares what a reader relies on: each field's name, offset,
// width, type and whether it is held, and the record's size.
func sameLayout(want, got *logb.Schema) error {
	if got.AxisKind != logb.AxisTime || got.AxisField != 0 || got.AxisExp != -9 {
		return fmt.Errorf("%s: its axis is not time in ns, first", got.Name)
	}
	if got.RecordBits != want.RecordBits {
		return fmt.Errorf("%s: %d bits a record, not %d", got.Name, got.RecordBits, want.RecordBits)
	}
	for _, wf := range want.Fields {
		i := peer.Field(got, wf.Name)
		if i < 0 {
			return fmt.Errorf("%s: no field %s", got.Name, wf.Name)
		}
		gf := got.Fields[i]
		if gf.BitOffset != wf.BitOffset || gf.BitWidth != wf.BitWidth || gf.Type != wf.Type || gf.Hold != wf.Hold {
			return fmt.Errorf("%s.%s: at bit %d, %d bits, type %v, held %v; want %d, %d, %v, %v", got.Name, wf.Name,
				gf.BitOffset, gf.BitWidth, gf.Type, gf.Hold, wf.BitOffset, wf.BitWidth, wf.Type, wf.Hold)
		}
	}
	return nil
}

// §5.6: the watermark never goes back, and is recorded at least every 10 ms.
func (c *secChecker) watermark() {
	wms := c.wms
	if len(wms) < 3 {
		c.r.fail("§5.6", "the watermark is recorded at least every 10 ms", "%d in a second", len(wms))
		return
	}
	var steps []time.Duration
	for i := 1; i < len(wms); i++ {
		if wms[i] < wms[i-1] {
			c.r.fail("§5.6", "the watermark never goes back", "from %d to %d", wms[i-1], wms[i])
			return
		}
		steps = append(steps, time.Duration(wms[i]-wms[i-1]))
	}
	c.r.pass("§5.6", "the watermark never goes back", "%d watermarks", len(wms))
	sort.Slice(steps, func(i, j int) bool { return steps[i] < steps[j] })
	median, worst := steps[len(steps)/2], steps[len(steps)-1]
	// The median is the cadence; the worst is allowed the jitter of a busy
	// machine, but not a stall a master would act on.
	if median > 15*time.Millisecond || worst > 50*time.Millisecond {
		c.r.fail("§5.6", "the watermark is recorded at least every 10 ms", "median step %v, longest %v", median, worst)
	} else {
		c.r.pass("§5.6", "the watermark is recorded at least every 10 ms", "median step %v, longest %v", median, worst)
	}
}

// §7.1: a glob that is not one is refused.
func (c *secChecker) badGlob(ctx context.Context) {
	code, _, err := c.do(ctx, "GET", "/stream/sub?ch=mon.%5Bx", "Bearer "+c.tok, "", nil)
	if err == nil && code != http.StatusBadRequest {
		err = fmt.Errorf("answered %d", code)
	}
	c.r.check("§7.1", "a malformed glob is refused 400", err)
}

// §6.3: the lease, exclusive, in the caller's own name.
func (c *secChecker) lease(ctx context.Context) string {
	if c.cfg.View != "" {
		v, err := newClient(c.cfg, c.cfg.View)
		if err == nil {
			_, err = v.Lease(ctx, "", 30*time.Second)
			if refusedWith(err) == http.StatusForbidden {
				err = nil
			} else if err == nil {
				err = errors.New("it was granted")
			}
		}
		c.r.check("§2", "the view role may not take the lease", err)
	} else {
		c.r.skip("§2", "the view role may not take the lease", "no view token given")
	}
	code, _, err := c.do(ctx, "POST", "/dut/lease", "Bearer "+c.tok, "", map[string]string{"holder": "conformance-impostor-name", "ttl": "30s"})
	if err == nil && code != http.StatusBadRequest {
		err = fmt.Errorf("answered %d", code)
	}
	c.r.check("§6.3", "a lease in a name other than the token's is refused 400", err)

	lease, err := c.op.Lease(ctx, "", 30*time.Second)
	if !c.r.check("§6.3", "the lease is taken: 201, a token", err) {
		return ""
	}
	other := c.op
	what := "the same caller"
	if c.cfg.Other != "" {
		if o, err := newClient(c.cfg, c.cfg.Other); err == nil {
			other, what = o, "another caller"
		}
	}
	_, err = other.Lease(ctx, "", 30*time.Second)
	switch refusedWith(err) {
	case http.StatusConflict:
		c.r.pass("§6.3", "a lease held is refused 409", "asked by %s", what)
	default:
		c.r.fail("§6.3", "a lease held is refused 409", "asked by %s: %v", what, err)
	}
	c.r.check("§6.3", "the lease is renewed", c.op.Renew(ctx, lease))
	return lease
}

// §6.1, §6.4–§6.6: what needs the lease, the arming or a declared name, without it.
func (c *secChecker) refusals(ctx context.Context, lease string) {
	// Disarmed first: the rack may have been left armed.
	c.r.check("§6.4", "POST /dut/disarm is accepted", c.op.Disarm(ctx))
	mode := c.mode()
	if mode == "" {
		c.r.skip("§6.4", "a mode change needs the lease", "the contract declares no mode")
	} else {
		_, err := c.op.SetMode(ctx, "not-the-lease", mode)
		c.refusal("§6.1", "a mode change without the lease is refused 403", err, http.StatusForbidden)
		_, err = c.op.SetMode(ctx, lease, mode)
		c.refusal("§6.4", "a mode change disarmed is refused 409", err, http.StatusConflict)
	}
	c.r.check("§6.4", "the DUT is armed under the lease", c.op.Arm(ctx, lease))
	_, err := c.op.SetMode(ctx, lease, "no-such-mode-here")
	c.refusal("§6.2", "an undeclared mode is refused 404", err, http.StatusNotFound)
	_, err = c.op.Invoke(ctx, lease, "no-such-action-here")
	c.refusal("§6.2", "an undeclared action is refused 404", err, http.StatusNotFound)
}

func (c *secChecker) refusal(section, title string, err error, code int) {
	var re *peer.RefusedError
	switch {
	case errors.As(err, &re) && re.Status == code:
		if re.Seq == 0 && code != http.StatusNotFound {
			c.r.fail(section, title, "refused %d with no seq: a recorded refusal has its number", code)
			return
		}
		c.r.pass(section, title, "")
	case err == nil:
		c.r.fail(section, title, "it was accepted")
	default:
		c.r.fail(section, title, "%v", err)
	}
}

func (c *secChecker) mode() string {
	if c.cfg.Mode != "" {
		return c.cfg.Mode
	}
	if len(c.id.Modes) > 0 {
		return c.id.Modes[0]
	}
	return ""
}

// §6.5, §6.6, §7.4: a mode entered and left, and an action run, each
// answered with a number and its outcome on the stream under it.
func (c *secChecker) modeAndAction(ctx context.Context, lease string) {
	if mode := c.mode(); mode != "" {
		c.setMode(ctx, lease, mode)
		c.setMode(ctx, lease, "")
	}
	if c.cfg.Action == "" {
		c.r.skip("§6.6", "an action's start and end are streamed under its number", "no action given to run")
		return
	}
	seq, err := c.op.Invoke(ctx, lease, c.cfg.Action)
	if !c.r.check("§6.6", "an action is accepted with a number", err) {
		return
	}
	var started bool
	err = c.await(seq, "dut.action."+c.cfg.Action, func(it *peer.Item) (bool, error) {
		switch ev := peer.Num(it, "event"); ev {
		case 1:
			started = true
		case 2:
			if !started {
				return false, errors.New("it ended without a start")
			}
			o := int(peer.Num(it, "outcome"))
			if o < 1 || o > len(peer.Outcomes) {
				return false, fmt.Errorf("it ended with outcome code %d", o)
			}
			return true, nil
		default:
			return false, fmt.Errorf("event %v", ev)
		}
		return false, nil
	})
	c.r.check("§6.6", "an action's start and end are streamed under its number", err)
}

func (c *secChecker) setMode(ctx context.Context, lease, mode string) {
	what := "mode " + mode
	if mode == "" {
		what = "no mode"
	}
	seq, err := c.op.SetMode(ctx, lease, mode)
	if !c.r.check("§6.5", what+" is accepted with a number", err) {
		return
	}
	var requested bool
	err = c.await(seq, "mode", func(it *peer.Item) (bool, error) {
		switch ev := peer.Num(it, "event"); ev {
		case secondary.ModeRequested:
			requested = true
		case secondary.ModeApplied:
			if !requested {
				return false, errors.New("applied with no request before it")
			}
			if got := peer.Str(it, "applied"); got != mode {
				return false, fmt.Errorf("applied is %q", got)
			}
			return true, nil
		case secondary.ModeFailed:
			return false, fmt.Errorf("its entry action failed: %s", peer.Str(it, "message"))
		}
		return false, nil
	})
	c.r.check("§6.5", what+": requested, then applied, on the mode stream under its number", err)
}

// await reads the subscription until done says the stream's records with
// that seq are complete, or Wait passes.
func (c *secChecker) await(seq uint32, stream string, done func(*peer.Item) (bool, error)) error {
	timeout := time.After(c.cfg.Wait)
	for {
		select {
		case it := <-c.items:
			c.note(it)
			if it.Schema.Name != stream || it.Restated || uint32(peer.Num(&it, "seq")) != seq {
				continue
			}
			ok, err := done(&it)
			if err != nil || ok {
				return err
			}
		case err := <-c.subErr:
			return fmt.Errorf("the subscription ended: %v", err)
		case <-timeout:
			return fmt.Errorf("nothing complete on %s under seq %d within %v", stream, seq, c.cfg.Wait)
		}
	}
}

// §6.4: anyone may disarm; a disarmed DUT takes no mode change.
func (c *secChecker) disarm(ctx context.Context, lease string) {
	by := c.op
	who := "the operate role"
	if c.cfg.View != "" {
		if v, err := newClient(c.cfg, c.cfg.View); err == nil {
			by, who = v, "the view role"
		}
	}
	if !c.r.check("§6.4", "POST /dut/disarm is accepted from "+who, by.Disarm(ctx)) {
		return
	}
	if mode := c.mode(); mode != "" {
		_, err := c.op.SetMode(ctx, lease, mode)
		c.refusal("§6.4", "after a disarm, a mode change is refused 409", err, http.StatusConflict)
	}
}

// §6.3: a released lease is gone.
func (c *secChecker) release(ctx context.Context, lease string) {
	if !c.r.check("§6.3", "the lease is released", c.op.Release(ctx, lease)) {
		return
	}
	err := c.op.Renew(ctx, lease)
	c.refusal("§6.3", "a released lease's token is refused 403", err, http.StatusForbidden)
}

// do is a request as it is written, for what the typed client never sends.
func (c *secChecker) do(ctx context.Context, method, path, auth, lease string, body any) (int, http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if lease != "" {
		req.Header.Set("Ktest-Lease", lease)
	}
	resp, err := c.raw.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, nil
}

func refusedWith(err error) int {
	var re *peer.RefusedError
	if errors.As(err, &re) {
		return re.Status
	}
	return 0
}

func newClient(cfg SecondaryConfig, token string) (*peer.Client, error) {
	return peer.New(peer.Config{Name: "conformance", URL: strings.TrimSuffix(cfg.URL, "/"), Token: token,
		CACert: cfg.CACert, Contract: cfg.Contract})
}

func rawClient(caCert string) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caCert != "" {
		pem, err := os.ReadFile(caCert)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", caCert)
		}
		tc.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 10 * time.Second}, nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}
