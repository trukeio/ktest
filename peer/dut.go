package peer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/trukeio/ktest/contract"
)

// DUT is the master's operations on the DUT, over a View fed by a Link: what
// a procedure's keywords call (doc/peer-protocol.md §6.5, §6.6, §9.5).
//
// Each operation returns nil or a *Failure with its outcome; or ctx's error.
// Times passed in and returned are this rack's clock; the DUT rack's are
// reached through the view's clock offset.
type DUT struct {
	View     *View
	Client   *Client
	Contract *contract.Identity
	// Lease is the DUT's lease token, as the Link holds it; "" when it holds
	// none.
	Lease func() string
	// OnJudgement, if set, is told every judgement the monitors gave, so that
	// it can be recorded and later recomputed from the mirrored records.
	OnJudgement func(Judgement)
	// WatermarkGrace is how long past a window's end the watermark may take
	// to pass it before the link is taken as lost; 0 is 2 s. It should be
	// well beyond the protection rule's watermark age, so that the rule
	// says it first.
	WatermarkGrace time.Duration
}

// Judgement is one answer the monitors gave: which deviated in a window, or
// whether all were ok at a watermark. Times are the DUT rack's clock.
type Judgement struct {
	Kind     JudgementKind
	From, To int64
	Monitors []string
	// Result is "alive=dev,speed=unknown", "" for none, for Deviations;
	// "ok" or "not ok" for AllOK.
	Result      string
	Offset      time.Duration
	Uncertainty time.Duration
}

// JudgementKind says which operation judged.
type JudgementKind byte

const (
	JudgeDeviations JudgementKind = 1
	JudgeAllOK      JudgementKind = 2
)

func (d *DUT) token() (string, error) {
	if d.Lease != nil {
		if t := d.Lease(); t != "" {
			return t, nil
		}
	}
	return "", failf(Refused, "this rack does not hold the DUT's lease")
}

// refusal turns what a request to the DUT rack failed with into an outcome.
func refusal(what string, err error) error {
	var re *RefusedError
	if errors.As(err, &re) {
		return failf(Refused, "%s: %v", what, re)
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return failf(LinkLost, "%s: %v", what, err)
}

// SetMode changes the DUT's mode, "" for none, and returns once the DUT rack
// has applied it and its settle time has passed by the DUT rack's watermark.
func (d *DUT) SetMode(ctx context.Context, mode string, timeout time.Duration) error {
	tok, err := d.token()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	seq, err := d.Client.SetMode(ctx, tok, mode)
	if err != nil {
		return refusal("mode "+mode, err)
	}
	v := d.View
	var failed string
	err = v.wait(ctx, deadline, "mode "+mode+" applied", func() bool {
		if v.modeSeq != seq {
			return false
		}
		if v.modeEvent == ModeFailed {
			failed = v.modeMsg
			return true
		}
		return v.modeEvent == ModeApplied
	})
	if err != nil {
		return err
	}
	if failed != "" {
		return failf(Fail, "mode %s: %s", mode, failed)
	}
	// The settle time, by the DUT rack's watermark: its monitors are judged
	// from then on.
	settle := d.Contract.Settle[mode]
	v.mu.Lock()
	until := v.modeT + int64(settle)
	v.mu.Unlock()
	return v.wait(ctx, deadline.Add(settle), "mode "+mode+" settled", func() bool { return v.wm >= until })
}

// Invoke runs an action, and returns once its end has arrived.
func (d *DUT) Invoke(ctx context.Context, action string, timeout time.Duration) error {
	tok, err := d.token()
	if err != nil {
		return err
	}
	seq, err := d.Client.Invoke(ctx, tok, action)
	if err != nil {
		return refusal("action "+action, err)
	}
	v := d.View
	var end actionEnd
	err = v.wait(ctx, time.Now().Add(timeout), "action "+action+"'s end", func() bool {
		e, ok := v.actions[seq]
		end = e
		return ok
	})
	if err != nil {
		return err
	}
	if end.outcome != OK {
		return failf(end.outcome, "action %s: %s", action, end.msg)
	}
	return nil
}

// Deviations waits until the DUT rack's watermark has passed t1, and says
// which monitors were dev or unknown at any time in [t0, t1], with what
// (§9.5). The window is widened by the clock offset's uncertainty, so that a
// deviation at its edge is counted.
func (d *DUT) Deviations(ctx context.Context, monitors []string, t0, t1 time.Time) (map[string]string, error) {
	v := d.View
	v.mu.Lock()
	p0, u := v.toPeer(t0)
	p1, _ := v.toPeer(t1)
	p0, p1 = p0-int64(u), p1+int64(u)
	v.mu.Unlock()
	grace := d.WatermarkGrace
	if grace <= 0 {
		grace = 2 * time.Second
	}
	err := v.wait(ctx, t1.Add(u+grace), "the DUT rack's watermark", func() bool { return v.wm >= p1 })
	if err != nil {
		var f *Failure
		if errors.As(err, &f) && f.Outcome == Timeout {
			return nil, failf(LinkLost, "the DUT rack's watermark did not pass the window's end within %v", grace)
		}
		return nil, err
	}
	v.mu.Lock()
	dev := v.deviated(monitors, p0, p1)
	off := v.offset
	v.mu.Unlock()
	// Told with the lock released: a recorder may well feed the view.
	var parts []string
	for _, m := range monitors {
		if st, ok := dev[m]; ok {
			parts = append(parts, m+"="+st)
		}
	}
	d.judge(Judgement{Kind: JudgeDeviations, From: p0, To: p1, Monitors: monitors,
		Result: strings.Join(parts, ","), Offset: off, Uncertainty: u})
	return dev, nil
}

// AllOK waits until every monitor is ok at the DUT rack's watermark, and
// says when, by this rack's clock; ok is false if it did not happen by
// deadline.
func (d *DUT) AllOK(ctx context.Context, monitors []string, deadline time.Time) (at time.Time, ok bool, err error) {
	v := d.View
	var wm int64
	var off, unc time.Duration
	err = v.wait(ctx, deadline, "every monitor ok", func() bool {
		wm, off, unc = v.wm, v.offset, v.unc
		for _, m := range monitors {
			if v.stateAt(m, v.wm) != StateOK {
				return false
			}
		}
		return v.wm != 0
	})
	var f *Failure
	j := Judgement{Kind: JudgeAllOK, From: wm, To: wm, Monitors: monitors, Result: "ok", Offset: off, Uncertainty: unc}
	switch {
	case err == nil:
		d.judge(j)
		return time.Now(), true, nil
	case errors.As(err, &f) && f.Outcome == Timeout:
		j.Result = "not ok"
		d.judge(j)
		return time.Time{}, false, nil
	}
	return time.Time{}, false, err
}

// States are the monitors' states at the watermark.
func (d *DUT) States(monitors []string) map[string]string { return d.View.States(monitors) }

// Connected says whether the DUT rack's monitors are streaming.
func (d *DUT) Connected() bool { return d.View.Connected() }

func (d *DUT) judge(j Judgement) {
	if d.OnJudgement != nil {
		d.OnJudgement(j)
	}
}
