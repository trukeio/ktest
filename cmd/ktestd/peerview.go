//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rveen/ktest/channel"
	"github.com/rveen/ktest/monitor"
	"github.com/rveen/ktest/peer"
	"github.com/rveen/ktest/procedure"
)

// # The DUT, as the master's procedures see it
//
// peerView is what the loop learns of the DUT rack, kept for a procedure's
// keywords on their own goroutine: each monitor's states with the DUT rack's
// times, its watermark, the mode as applied, the actions' ends, the link and
// the clocks' offset. The loop writes it as it mirrors; a keyword waiting on
// it is woken by every change. dutPeer is lib.Peer over it: the five
// operations of handoff §4.8.
type peerView struct {
	mu      sync.Mutex
	changed chan struct{}

	connected bool
	offset    time.Duration // the DUT rack's clock less this one
	unc       time.Duration // its uncertainty
	haveOff   bool
	wm        int64 // the DUT rack's watermark, its clock; 0 for none

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
	outcome procedure.Outcome
	msg     string
}

// histMax bounds each monitor's history: a day of a monitor changing every
// second is more than a procedure looks back over, and less than a megabyte.
const histMax = 100_000

func newPeerView() *peerView {
	return &peerView{changed: make(chan struct{}), hist: map[string][]stamp{}, actions: map[uint32]actionEnd{}}
}

// update changes the view under its lock and wakes every waiter.
func (v *peerView) update(f func()) {
	v.mu.Lock()
	f()
	close(v.changed)
	v.changed = make(chan struct{})
	v.mu.Unlock()
}

// observe takes what a mirrored record says, on the loop.
func (v *peerView) observe(it *peer.Item) {
	name := it.Schema.Name
	field := func(f string) channel.Ref {
		i := fieldNamed(it.Schema, f)
		return channel.Ref{Schema: it.Schema, Field: i}
	}
	switch {
	case name == "mon.watermark":
		v.update(func() { v.wm = max(v.wm, it.PeerT) })
	case name == "mon.gap":
	case strings.HasPrefix(name, "mon."):
		st := field("state").Decode(it.Rec).Str
		m := strings.TrimPrefix(name, "mon.")
		v.update(func() {
			h := v.hist[m]
			if n := len(h); n > 0 && h[n-1].state == st && h[n-1].t <= it.PeerT {
				return // a restatement of what is already known
			}
			h = append(h, stamp{it.PeerT, st})
			if len(h) > histMax {
				h = h[len(h)-histMax:]
			}
			v.hist[m] = h
		})
	case name == "mode":
		ev := byte(field("event").Decode(it.Rec).Num)
		applied := field("applied").Decode(it.Rec).Str
		seq := uint32(field("seq").Decode(it.Rec).Num)
		msg := field("message").Decode(it.Rec).Str
		v.update(func() {
			if ev == modeApplied || it.Restated {
				v.mode, v.modeT = applied, it.PeerT
			}
			v.modeSeq, v.modeEvent, v.modeMsg = seq, ev, msg
		})
	case strings.HasPrefix(name, "dut.action."):
		if byte(field("event").Decode(it.Rec).Num) != 2 {
			return
		}
		seq := uint32(field("seq").Decode(it.Rec).Num)
		code := int(field("outcome").Decode(it.Rec).Num)
		o := procedure.Fail
		if code >= 1 && code <= len(procedure.Outcomes) {
			o = procedure.Outcomes[code-1]
		}
		msg := field("message").Decode(it.Rec).Str
		v.update(func() { v.actions[seq] = actionEnd{o, msg} })
	}
}

func (v *peerView) link(up bool) {
	v.update(func() {
		v.connected = up
		if !up {
			v.wm = 0
		}
	})
}

func (v *peerView) clock(e *peer.Estimate) {
	v.update(func() { v.offset, v.unc, v.haveOff = e.Offset, e.Uncertainty, true })
}

// wait calls done under the view's lock until it says so, the deadline
// passes, ctx ends, or the link is down, which is link_lost.
func (v *peerView) wait(ctx context.Context, deadline time.Time, what string, done func() bool) error {
	within := time.Until(deadline)
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		v.mu.Lock()
		if !v.connected {
			v.mu.Unlock()
			return procedure.Failf(procedure.LinkLost, "the DUT rack is not connected, waiting for %s", what)
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
			return procedure.Failf(procedure.Timeout, "%s did not happen within %v", what, within.Round(time.Millisecond))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// toPeer is a time of this rack's clock on the DUT rack's, with the offset's
// uncertainty, which is 0 until there is an estimate. Called with the lock.
func (v *peerView) toPeer(t time.Time) (int64, time.Duration) {
	if !v.haveOff {
		return t.UnixNano(), 0
	}
	return t.UnixNano() + int64(v.offset), v.unc
}

// stateAt is a monitor's state at t, the DUT rack's clock: its last stamp at
// or before t. Called with the lock.
func (v *peerView) stateAt(m string, t int64) string {
	h := v.hist[m]
	i, _ := slices.BinarySearchFunc(h, t, func(s stamp, t int64) int {
		if s.t <= t {
			return -1
		}
		return 1
	})
	if i == 0 {
		return string(monitor.Unknown)
	}
	return h[i-1].state
}

// dutPeer is lib.Peer: the master's DUT operations over the peer.
type dutPeer struct {
	v     *peerView
	c     *peer.Client
	id    *monitor.Identity
	lease *atomic.Pointer[string] // the DUT's lease, held by the driver
	// judged records each judgement the keywords make, so that a check can
	// hold it to the mirrored states it was made from.
	judged func(judgement)
}

// judgement is one answer the DUT's monitors gave a keyword: which deviated
// in a window, or whether all were ok at a watermark. Times are the DUT
// rack's clock, so that it can be recomputed from the mirrored records alone.
type judgement struct {
	kind     byte // judgeDeviations or judgeAllOK
	from, to int64
	monitors []string
	result   string // "alive=dev,speed=unknown", "" for none; "ok" or "not ok"
	offset   time.Duration
	unc      time.Duration
}

const (
	judgeDeviations = 1
	judgeAllOK      = 2
)

func (d *dutPeer) check(kind string, names []string, n string) error {
	if !slices.Contains(names, n) {
		return fmt.Errorf("contract %s has no %s %q; its %ss are %s", d.id.Name, kind, n, kind, strings.Join(names, ", "))
	}
	return nil
}

func (d *dutPeer) Monitor(n string) error { return d.check("monitor", d.id.Monitors, n) }
func (d *dutPeer) Mode(n string) error    { return d.check("mode", d.id.Modes, n) }
func (d *dutPeer) Action(n string) error  { return d.check("action", d.id.Actions, n) }

func (d *dutPeer) token() (string, error) {
	if t := d.lease.Load(); t != nil && *t != "" {
		return *t, nil
	}
	return "", procedure.Failf(procedure.Refused, "this rack does not hold the DUT's lease (see %s.link)", d.c.Name())
}

// refusal turns what a request to the DUT rack failed with into an outcome.
func refusal(what string, err error) error {
	var re *peer.RefusedError
	if errors.As(err, &re) {
		return procedure.Failf(procedure.Refused, "%s: %v", what, re)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return procedure.Failf(procedure.LinkLost, "%s: %v", what, err)
}

func (d *dutPeer) SetMode(ctx context.Context, mode string, timeout time.Duration) error {
	tok, err := d.token()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	seq, err := d.c.SetMode(ctx, tok, mode)
	if err != nil {
		return refusal("mode "+mode, err)
	}
	var failed string
	err = d.v.wait(ctx, deadline, "mode "+mode+" applied", func() bool {
		if d.v.modeSeq != seq {
			return false
		}
		if d.v.modeEvent == modeFailed {
			failed = d.v.modeMsg
			return true
		}
		return d.v.modeEvent == modeApplied
	})
	if err != nil {
		return err
	}
	if failed != "" {
		return procedure.Failf(procedure.Fail, "mode %s: %s", mode, failed)
	}
	// The settle time, by the DUT rack's watermark: its monitors are judged
	// from then on.
	settle := d.id.Settle[mode]
	d.v.mu.Lock()
	until := d.v.modeT + int64(settle)
	d.v.mu.Unlock()
	return d.v.wait(ctx, deadline.Add(settle), "mode "+mode+" settled", func() bool { return d.v.wm >= until })
}

func (d *dutPeer) Invoke(ctx context.Context, action string, timeout time.Duration) error {
	tok, err := d.token()
	if err != nil {
		return err
	}
	seq, err := d.c.Invoke(ctx, tok, action)
	if err != nil {
		return refusal("action "+action, err)
	}
	var end actionEnd
	err = d.v.wait(ctx, time.Now().Add(timeout), "action "+action+"'s end", func() bool {
		e, ok := d.v.actions[seq]
		end = e
		return ok
	})
	if err != nil {
		return err
	}
	if end.outcome != procedure.OK {
		return procedure.Failf(end.outcome, "action %s: %s", action, end.msg)
	}
	return nil
}

// wmGrace is how long past a window's end the DUT rack's watermark may take
// to pass it before the link is taken as lost: well beyond the rules'
// watermark_age, so that a rule says it first.
const wmGrace = 2 * time.Second

func (d *dutPeer) Deviations(ctx context.Context, ms []string, t0, t1 time.Time) (map[string]string, error) {
	d.v.mu.Lock()
	p0, u := d.v.toPeer(t0)
	p1, _ := d.v.toPeer(t1)
	p0, p1 = p0-int64(u), p1+int64(u)
	d.v.mu.Unlock()
	err := d.v.wait(ctx, t1.Add(u+wmGrace), "the DUT rack's watermark", func() bool { return d.v.wm >= p1 })
	if err != nil {
		var f *procedure.Failure
		if errors.As(err, &f) && f.Outcome == procedure.Timeout {
			return nil, procedure.Failf(procedure.LinkLost, "the DUT rack's watermark did not pass the window's end within %v", wmGrace)
		}
		return nil, err
	}
	d.v.mu.Lock()
	dev := map[string]string{}
	bad := func(s string) bool { return s == string(monitor.Dev) || s == string(monitor.Unknown) }
	for _, m := range ms {
		if s := d.v.stateAt(m, p0); bad(s) {
			dev[m] = s
			continue
		}
		for _, x := range d.v.hist[m] {
			if x.t > p0 && x.t <= p1 && bad(x.state) {
				dev[m] = x.state
				break
			}
		}
	}
	off := d.v.offset
	d.v.mu.Unlock()
	// Recorded with the lock released: recording goes through the loop,
	// which takes the lock to update the view.
	var parts []string
	for _, m := range ms {
		if st, ok := dev[m]; ok {
			parts = append(parts, m+"="+st)
		}
	}
	d.judge(judgement{kind: judgeDeviations, from: p0, to: p1, monitors: ms, result: strings.Join(parts, ","),
		offset: off, unc: u})
	return dev, nil
}

func (d *dutPeer) AllOK(ctx context.Context, ms []string, deadline time.Time) (time.Time, bool, error) {
	var at int64
	var off, unc time.Duration
	err := d.v.wait(ctx, deadline, "every monitor ok", func() bool {
		at, off, unc = d.v.wm, d.v.offset, d.v.unc
		for _, m := range ms {
			if d.v.stateAt(m, d.v.wm) != string(monitor.OK) {
				return false
			}
		}
		return d.v.wm != 0
	})
	var f *procedure.Failure
	j := judgement{kind: judgeAllOK, from: at, to: at, monitors: ms, result: "ok", offset: off, unc: unc}
	switch {
	case err == nil:
		d.judge(j)
		return time.Now(), true, nil
	case errors.As(err, &f) && f.Outcome == procedure.Timeout:
		j.result = "not ok"
		d.judge(j)
		return time.Time{}, false, nil
	}
	return time.Time{}, false, err
}

func (d *dutPeer) judge(j judgement) {
	if d.judged != nil {
		d.judged(j)
	}
}

func (d *dutPeer) States(ms []string) map[string]string {
	d.v.mu.Lock()
	defer d.v.mu.Unlock()
	out := map[string]string{}
	for _, m := range ms {
		out[m] = d.v.stateAt(m, d.v.wm)
	}
	return out
}

func (d *dutPeer) Connected() bool {
	d.v.mu.Lock()
	defer d.v.mu.Unlock()
	return d.v.connected
}
