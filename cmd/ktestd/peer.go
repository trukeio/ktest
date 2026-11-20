//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/contract"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/procedure"
	"github.com/trukeio/ktest/record"
)

// # The master's peer
//
// With -peer, the daemon is a master and the peer a DUT rack, driven as an
// instrument (handoff §4.6, §4.7):
//
//	-peer dut=https://host:8443,token=dut.token,cacert=dut.pem,contract=dut-interface.json
//
// At start the daemon asks the DUT rack which contract it runs, and refuses to
// start unless it is the master's own copy, byte for byte, resource included.
// Then the driver (package peer) holds the DUT's lease, subscribes to its
// monitors, mode and actions, and samples its clock; the daemon records:
//
//	<peer>.<stream>  every record the DUT rack streams (dut.mon.alive, dut.mode, ...),
//	                 at the time this rack received it, with the DUT rack's own time
//	                 in peer_t and restated true for a state restated rather than written
//	<peer>.link      connected, held; each connection, loss and refusal, with why
//	<peer>.clock     the DUT rack's clock less this one, and its uncertainty
//	rules            each rule taking hold and clearing, with its cause
//
// # Rules
//
// limits.json's rules are what the daemon does about a DUT rack, whatever any
// client asks:
//
//	"rules": {
//	  "monitoring_lost": {"watermark_age": "100ms", "action": "remove_stress"},
//	  "dut_abort":       {"monitor": "abort", "action": "remove_stress"},
//	  "remove_stress":   ["psu1"]
//	}
//
// monitoring_lost holds while the link is down, or the DUT rack's watermark is
// older than watermark_age by this rack's clock; dut_abort while the monitor is
// dev. When a rule takes hold, and again whenever a listed instrument is armed
// while it holds, the listed instruments are disarmed with their outputs off,
// as the stop does, under one number, recorded as request 15 with the rule's
// name; a procedure under way fails with the rule as its cause. They are
// evaluated on every mirrored record and every 10 ms.

// rules is limits.json's rules section.
type rules struct {
	MonitoringLost *struct {
		WatermarkAge duration `json:"watermark_age"`
		Action       string   `json:"action"`
	} `json:"monitoring_lost"`
	DutAbort *struct {
		Monitor string `json:"monitor"`
		Action  string `json:"action"`
	} `json:"dut_abort"`
	RemoveStress []string `json:"remove_stress"`
}

// The rules by name, as recorded.
const (
	ruleLost  = "monitoring_lost"
	ruleAbort = "dut_abort"
)

// checkRules refuses rules that would do nothing, or something doubtful: rules
// without a peer, a peer without monitoring_lost, an action other than
// remove_stress, an instrument this daemon does not have, a monitor the
// contract lacks.
func checkRules(r *rules, havePeer bool, insts map[string]bool, ids []*contract.Identity) error {
	if r == nil {
		if havePeer {
			return errors.New(`-peer needs "rules": {"monitoring_lost": ...} in limits.json: ` +
				"a master that kept its stress on a DUT nobody was watching would protect nothing")
		}
		return nil
	}
	switch {
	case !havePeer:
		return errors.New(`"rules" are about a DUT rack, and there is no -peer`)
	case r.MonitoringLost == nil:
		return errors.New(`rules: "monitoring_lost" is required with a peer`)
	case r.MonitoringLost.WatermarkAge <= 0:
		return errors.New(`rules: monitoring_lost needs a "watermark_age", such as "100ms"`)
	case r.MonitoringLost.Action != "remove_stress":
		return fmt.Errorf(`rules: monitoring_lost's action is %q; the action there is is "remove_stress"`, r.MonitoringLost.Action)
	case len(r.RemoveStress) == 0:
		return errors.New(`rules: "remove_stress" names no instrument, so removing the stress would do nothing`)
	}
	if a := r.DutAbort; a != nil {
		switch {
		case a.Action != "remove_stress":
			return fmt.Errorf(`rules: dut_abort's action is %q; the action there is is "remove_stress"`, a.Action)
		case a.Monitor == "":
			return errors.New(`rules: dut_abort needs the "monitor" that asks for protection, "abort"`)
		}
		// The rule is about every DUT rack, so every contract must have the
		// monitor it watches.
		for _, id := range ids {
			if !slices.Contains(id.Monitors, a.Monitor) {
				return fmt.Errorf("rules: dut_abort watches monitor %q, which contract %s does not declare", a.Monitor, id.Name)
			}
		}
	}
	for _, n := range r.RemoveStress {
		if !insts[n] {
			return fmt.Errorf("rules: remove_stress names %q, which is no instrument of this daemon", n)
		}
	}
	return nil
}

// The events of <peer>.link.
const (
	linkUp      = 1
	linkDown    = 2
	linkRefused = 3 // the DUT rack answered, and refused: a lease, or a contract that is not the master's
)

// linkCode is a link event's code in <peer>.link: a lease refused is a
// refusal, as a contract refused is, though the link stands.
func linkCode(k peer.LinkKind) byte {
	switch k {
	case peer.LinkUp:
		return linkUp
	case peer.LinkDown:
		return linkDown
	}
	return linkRefused
}

type linkEvent struct {
	event byte
	msg   string
}

// peerLink is the loop's side of one DUT rack.
type peerLink struct {
	c      *peer.Client
	id     *contract.Identity
	driver *peer.Link
	guard  peer.Guard

	link, clock, judgeSt *teeStream
	mirrors              map[string]*mirror

	// view is the DUT as the rules and the procedures' keywords see it.
	view *peer.View

	connected bool
	lastLink  linkEvent // the last link record, so that a retry failing as the last did is not recorded again
}

// peerSet is the loop's side of every DUT rack it drives, and of the rules
// about them. The stress is the lab rack's, one for all of them — a chamber
// holds every DUT, a supply may power several — so a rule holds while it
// holds for any rack, and says which.
type peerSet struct {
	links  []*peerLink
	rules  *rules
	ruleSt *teeStream
	holds  map[string]bool
	events chan peerMsg
}

// peerMsg is what one rack's driver handed over.
type peerMsg struct {
	p *peerLink
	e peer.Event
}

// mirror is one of the DUT rack's streams as this rack records it.
type mirror struct {
	from *logb.Schema
	to   *teeStream
}

// The layouts. <peer>.link: t 0–7, event 8, connected 9 (held), message
// 16–175. <peer>.clock: t 0–7, offset 8–15, uncertainty 16–23, rtt 24–31.
// rules: t 0–7, seq 8–11, event 12, holds 13 bit 0 monitoring_lost, bit 1
// dut_abort (held), rule 16–47, message 48–207.
const (
	lkEvent     = 8
	lkConnected = 9
	lkMessage   = 16
	lkBytes     = lkMessage + 160

	ckOffset = 8
	ckUnc    = 16
	ckRTT    = 24
	ckBytes  = 32

	rlSeq     = 8
	rlEvent   = 12
	rlHolds   = 13
	rlRule    = 16
	rlMessage = 48
	rlBytes   = rlMessage + 160

	// A mirrored record: the axis, peer_t, then restated, then the DUT
	// rack's record after its own axis.
	mrPeerT    = 8
	mrFlags    = 16
	mrRecord   = 24
	mrOverhead = mrRecord - 8
)

func linkSchema(name string) *logb.Schema {
	return record.Schema(name+".link", 8*lkBytes, []logb.Field{
		uintF("event", lkEvent, 8, "", "1 connected, 2 the connection was lost, 3 the DUT rack refused: a lease, or its contract"),
		holdF(logb.Field{Name: "connected", BitOffset: 8 * lkConnected, BitWidth: 1, Type: logb.TypeBool,
			Desc: "the DUT rack's monitors are streaming to this rack"}),
		strF("message", lkMessage, 160, "why"),
	}, map[string]string{"source": "the ktestd peer driver", "clock": "userspace: this rack's clock"})
}

func clockSchema(name string) *logb.Schema {
	return record.Schema(name+".clock", 8*ckBytes, []logb.Field{
		{Name: "offset", BitOffset: 8 * ckOffset, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the DUT rack's clock less this rack's, from the round trip with the shortest time of the last eight"},
		{Name: "uncertainty", BitOffset: 8 * ckUnc, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "half that round trip: the offset is right to within this"},
		{Name: "rtt", BitOffset: 8 * ckRTT, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Desc: "that round trip"},
	}, map[string]string{"source": "the ktestd peer driver, GET /clock once a second",
		"clock": "userspace: this rack's clock when the sample the estimate rests on was taken"})
}

func rulesSchema() *logb.Schema {
	return record.Schema("rules", 8*rlBytes, []logb.Field{
		uintF("seq", rlSeq, 32, "", "the number the rule's decisions were recorded under; 0 when it cleared"),
		uintF("event", rlEvent, 8, "", "1 the rule took hold and removed the stress, 2 it removed the stress again "+
			"from an instrument armed while it held, 3 it cleared"),
		holdF(logb.Field{Name: ruleLost, BitOffset: 8 * rlHolds, BitWidth: 1, Type: logb.TypeBool,
			Desc: "monitoring_lost holds"}),
		holdF(logb.Field{Name: ruleAbort, BitOffset: 8*rlHolds + 1, BitWidth: 1, Type: logb.TypeBool,
			Desc: "dut_abort holds"}),
		strF("rule", rlRule, 32, "the rule"),
		strF("message", rlMessage, 160, "its cause"),
	}, map[string]string{"source": "the ktestd daemon rules (limits.json)", "clock": "userspace: when the daemon decided"})
}

// <peer>.judgement: t 0–7, kind 8, from 16–23, to 24–31, offset 32–39,
// uncertainty 40–47, monitors 48–111, result 112–207.
const (
	jgKind     = 8
	jgFrom     = 16
	jgTo       = 24
	jgOffset   = 32
	jgUnc      = 40
	jgMonitors = 48
	jgResult   = 112
	jgBytes    = jgResult + 96
)

func judgementSchema(name string) *logb.Schema {
	return record.Schema(name+".judgement", 8*jgBytes, []logb.Field{
		uintF("kind", jgKind, 8, "", "1 which monitors deviated in [from, to]; 2 whether all were ok at the watermark to"),
		{Name: "from", BitOffset: 8 * jgFrom, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the window's start, the DUT rack's clock, widened by the offset's uncertainty"},
		{Name: "to", BitOffset: 8 * jgTo, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the window's end, likewise; for kind 2, the watermark the states were read at"},
		{Name: "offset", BitOffset: 8 * jgOffset, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the clocks' offset the window was put on the DUT rack's clock with"},
		{Name: "uncertainty", BitOffset: 8 * jgUnc, BitWidth: 64, Type: logb.TypeSint, Unit: "ns", Desc: "its uncertainty"},
		strF("monitors", jgMonitors, 64, "the monitors judged, comma separated"),
		strF("result", jgResult, 96, "kind 1: monitor=state for each that was dev or unknown, empty for none; kind 2: ok or not ok"),
	}, map[string]string{"source": "the master's DUT keywords",
		"clock": "userspace: this rack's clock when the judgement was made"})
}

func (p *peerLink) writeJudgement(w *tee, t time.Time, j peer.Judgement) error {
	return w.Write(t, p.judgeSt, func(rec []byte) {
		rec[jgKind] = byte(j.Kind)
		putUint64(rec[jgFrom:], uint64(j.From))
		putUint64(rec[jgTo:], uint64(j.To))
		putUint64(rec[jgOffset:], uint64(j.Offset))
		putUint64(rec[jgUnc:], uint64(j.Uncertainty))
		putStr(rec, jgMonitors, 64, strings.Join(j.Monitors, ","))
		putStr(rec, jgResult, 96, j.Result)
	})
}

// mirrorSchema is a DUT rack stream as this rack records it.
func mirrorSchema(name string, from *logb.Schema) (*logb.Schema, error) {
	if from.AxisField != 0 || len(from.Fields) == 0 || from.Fields[0].BitWidth != 64 || from.RecordBits < 64 {
		return nil, fmt.Errorf("%s: a stream with its time axis elsewhere than first is not mirrored", from.Name)
	}
	fields := []logb.Field{
		{Name: "peer_t", BitOffset: 8 * mrPeerT, BitWidth: 64, Type: logb.TypeSint, Unit: "ns",
			Desc: "the record's own time on the DUT rack's clock, Unix ns; the record's time is when this rack received it"},
		{Name: "restated", BitOffset: 8 * mrFlags, BitWidth: 1, Type: logb.TypeBool,
			Desc: "a state the DUT rack restated when the subscription joined, not one it wrote then; peer_t is when it was written"},
	}
	for _, f := range from.Fields[1:] {
		f.BitOffset += 8 * mrOverhead
		if f.Guarded {
			f.GuardField += 2 // peer_t and restated come before it
		}
		fields = append(fields, f)
	}
	meta := map[string]string{}
	maps.Copy(meta, from.Meta)
	meta["mirror.of"] = from.Name
	meta["clock"] = "this rack's, when the record was received; peer_t is the DUT rack's"
	return record.Schema(name+"."+from.Name, from.RecordBits+8*mrOverhead, fields, meta), nil
}

// newPeerLink checks the peer before anything is written: it must answer, and
// with the master's contract. The hash of the contract and of its resource
// are compared, since a changed resource changes what an action does under
// an unchanged contract.
func newPeerLink(cfg peer.Config, r *rules, meta map[string]string) (*peerLink, error) {
	id, err := contract.Identify(cfg.Contract)
	if err != nil {
		return nil, err
	}
	c, err := peer.New(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: the DUT rack must answer at start, so that its contract can be checked: %v",
			cfg.Name, cfg.URL, err)
	}
	if err := peer.Matches(id, info); err != nil {
		return nil, fmt.Errorf("%s at %s: %v", cfg.Name, cfg.URL, err)
	}
	p := cfg.Name
	meta["peer."+p+".url"] = cfg.URL
	meta["peer."+p+".contract"] = id.Name
	meta["peer."+p+".contract.sha256"] = id.Hash
	meta["peer."+p+".resource.sha256"] = id.ResourceHash
	for _, k := range []string{"daemon.version", "host.name", "bus.interface", "tls.cert.sha256", "recording.epoch"} {
		if v, ok := info.Provenance[k]; ok {
			meta["peer."+p+".provenance."+k] = v
		}
	}
	pl := &peerLink{c: c, id: id, mirrors: map[string]*mirror{}, view: peer.NewView(),
		driver: &peer.Link{Client: c, Contract: id}}
	if r != nil {
		pl.guard.WatermarkAge = time.Duration(r.MonitoringLost.WatermarkAge)
		if r.DutAbort != nil {
			pl.guard.Abort = r.DutAbort.Monitor
		}
	}
	return pl, nil
}

// newPeerSet gathers the links, with the rules about them; monitoring is
// lost from the start, until every rack is connected and vouching.
func newPeerSet(links []*peerLink, r *rules) *peerSet {
	return &peerSet{links: links, rules: r, holds: map[string]bool{ruleLost: true}, events: make(chan peerMsg, 1024)}
}

// declare lays out every rack's streams and the rules', so that a reader
// meets them from the start, the links down.
func (ps *peerSet) declare(w *tee, t time.Time) error {
	for _, p := range ps.links {
		if err := p.declare(w, t); err != nil {
			return err
		}
	}
	var err error
	if ps.ruleSt, err = w.NewStream(rulesSchema); err != nil {
		return err
	}
	return ps.writeRule(w, t, 0, 0, "", "")
}

// declare lays out a rack's own streams.
func (p *peerLink) declare(w *tee, t time.Time) error {
	var err error
	if p.link, err = w.NewStream(func() *logb.Schema { return linkSchema(p.c.Name()) }); err != nil {
		return err
	}
	if p.clock, err = w.NewStream(func() *logb.Schema { return clockSchema(p.c.Name()) }); err != nil {
		return err
	}
	if p.judgeSt, err = w.NewStream(func() *logb.Schema { return judgementSchema(p.c.Name()) }); err != nil {
		return err
	}
	return p.writeLink(w, t, linkDown, "not yet connected")
}

func (p *peerLink) writeLink(w *tee, t time.Time, ev byte, msg string) error {
	return w.Write(t, p.link, func(rec []byte) {
		rec[lkEvent] = ev
		if p.connected {
			rec[lkConnected] = 1
		}
		putStr(rec, lkMessage, 160, msg)
	})
}

func (ps *peerSet) writeRule(w *tee, t time.Time, seq uint32, ev byte, rule, msg string) error {
	return w.Write(t, ps.ruleSt, func(rec []byte) {
		putUint32(rec[rlSeq:], seq)
		rec[rlEvent] = ev
		if ps.holds[ruleLost] {
			rec[rlHolds] |= 1
		}
		if ps.holds[ruleAbort] {
			rec[rlHolds] |= 2
		}
		putStr(rec, rlRule, 32, rule)
		putStr(rec, rlMessage, 160, msg)
	})
}

// run is the driver, connected or trying to be until ctx ends: package
// peer's Link, handing what happens to the loop.
func (p *peerLink) run(ctx context.Context, out chan<- peerMsg) {
	p.driver.Run(ctx, func(e peer.Event) bool {
		select {
		case out <- peerMsg{p, e}:
			return true
		case <-ctx.Done():
			return false
		}
	})
}

// peerEvent handles what the driver handed over, on the loop.
func (l *loop) peerEvent(m peerMsg) {
	p, e := m.p, m.e
	t := time.Now()
	switch {
	case e.Link != nil:
		p.view.Apply(e)
		p.connected = p.view.Connected()
		le := linkEvent{linkCode(e.Link.Kind), e.Link.Msg}
		if le == p.lastLink {
			break // the driver retrying, and failing as it did: nothing new to say
		}
		p.lastLink = le
		if err := p.writeLink(l.w, t, le.event, le.msg); err != nil {
			fatalf("write: %v", err)
		}
	case e.Clock != nil:
		p.view.Apply(e)
		err := l.w.Write(e.Clock.At, p.clock, func(rec []byte) {
			putUint64(rec[ckOffset:], uint64(e.Clock.Offset))
			putUint64(rec[ckUnc:], uint64(e.Clock.Uncertainty))
			putUint64(rec[ckRTT:], uint64(e.Clock.RTT))
		})
		if err != nil {
			fatalf("write: %v", err)
		}
	case e.Item != nil:
		l.mirror(p, e.Item)
	}
	l.evalRules(t)
}

// mirror records one of the DUT rack's records, and takes from it what the
// rules watch.
func (l *loop) mirror(p *peerLink, it *peer.Item) {
	name := it.Schema.Name
	m := p.mirrors[name]
	if m != nil && (m.from.RecordBits != it.Schema.RecordBits || len(m.from.Fields) != len(it.Schema.Fields)) {
		// The DUT rack restarted with another layout for the stream. A
		// recording holds one schema to a name, so the record is said and
		// not kept, rather than misread.
		p.writeLink(l.w, time.Now(), linkRefused, fmt.Sprintf("%s changed its layout; not mirrored", name))
		return
	}
	if m == nil {
		if _, err := mirrorSchema(p.c.Name(), it.Schema); err != nil {
			p.writeLink(l.w, time.Now(), linkRefused, err.Error())
			return
		}
		// Each writer is given a schema of its own, as NewStream asks.
		st, err := l.w.NewStream(func() *logb.Schema { s, _ := mirrorSchema(p.c.Name(), it.Schema); return s })
		if err != nil {
			fatalf("%v", err)
		}
		m = &mirror{from: it.Schema, to: st}
		p.mirrors[name] = m
	}
	err := l.w.Write(it.Recv, m.to, func(rec []byte) {
		putUint64(rec[mrPeerT:], uint64(it.PeerT))
		if it.Restated {
			rec[mrFlags] = 1
		}
		copy(rec[mrRecord:], it.Rec[8:])
	})
	if err != nil {
		fatalf("write: %v", err)
	}
	p.view.Observe(it)
}

// evalRules decides, on the loop, whether a rule holds, and removes the
// stress when one takes hold or when a listed instrument is armed while it
// holds.
func (l *loop) evalRules(t time.Time) {
	ps := l.peers
	if ps == nil || ps.rules == nil {
		return
	}
	// A rule holds while it holds for any rack; its cause names each one it
	// holds for, by name when there are several.
	var lost, abort []string
	for _, p := range ps.links {
		v := p.guard.Check(p.view, t)
		who := ""
		if len(ps.links) > 1 {
			who = p.c.Name() + ": "
		}
		if v.MonitoringLost {
			lost = append(lost, who+v.LostWhy)
		}
		if v.DUTAbort {
			abort = append(abort, who+v.AbortWhy)
		}
	}
	l.rule(t, ruleLost, len(lost) > 0, strings.Join(lost, "; "))
	if ps.rules.DutAbort != nil {
		l.rule(t, ruleAbort, len(abort) > 0, strings.Join(abort, "; "))
	}
}

func (l *loop) rule(t time.Time, name string, holds bool, why string) {
	p := l.peers
	was := p.holds[name]
	p.holds[name] = holds
	if !holds {
		if was {
			if err := p.writeRule(l.w, t, 0, 3, name, "cleared"); err != nil {
				fatalf("write: %v", err)
			}
		}
		return
	}
	var armed []string
	for _, n := range p.rules.RemoveStress {
		if l.insts[n].armed {
			armed = append(armed, n)
		}
	}
	if was && len(armed) == 0 {
		return
	}
	// Taking hold, the stress is removed from every listed instrument, armed
	// or not: an output can be on with nobody holding the lease. Holding
	// still, from the ones armed since.
	targets, ev := p.rules.RemoveStress, byte(1)
	if was {
		targets, ev = armed, 2
	}
	l.seq++
	r := &request{kind: reqRule, name: name}
	for _, n := range slices.Sorted(slices.Values(targets)) {
		l.disarm(l.insts[n], t, l.seq, record.ControlRule, r, true)
	}
	if err := p.writeRule(l.w, t, l.seq, ev, name, why); err != nil {
		fatalf("write: %v", err)
	}
	outcome := procedure.LinkLost
	if name == ruleAbort {
		outcome = procedure.Fail
	}
	l.d.procs.failRun(outcome, name+": "+why)
}

// peerList is -peer, once for each DUT rack.
type peerList []string

func (f *peerList) String() string     { return strings.Join(*f, " ") }
func (f *peerList) Set(v string) error { *f = append(*f, v); return nil }

// contractsHash is what procedure.run records as the DUT contract: the one
// rack's contract hash, or with several, the sha256 of a line for each,
// "name sha256", in name order. Each rack's own is in the recording's
// metadata, under peer.<name>.contract.sha256.
func contractsHash(links []*peerLink) string {
	if len(links) == 1 {
		return links[0].id.Hash
	}
	var lines []string
	for _, p := range links {
		lines = append(lines, p.c.Name()+" "+p.id.Hash+"\n")
	}
	slices.Sort(lines)
	return contract.Hash([]byte(strings.Join(lines, "")))
}
