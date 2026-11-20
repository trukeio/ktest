//go:build linux

package main

import (
	"context"
	"errors"
	"time"

	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/procedure"
)

// # The DUT, as the master's procedures see it
//
// dutPeer is lib.Peer over package peer's DUT operations (handoff §4.8): the
// same operations, their failures turned into the procedure's outcomes. The
// view they wait on is fed by the loop as it mirrors.
type dutPeer struct {
	d *peer.DUT
}

func (p *dutPeer) Monitor(n string) error { return p.d.Contract.Has("monitor", n) }
func (p *dutPeer) Mode(n string) error    { return p.d.Contract.Has("mode", n) }
func (p *dutPeer) Action(n string) error  { return p.d.Contract.Has("action", n) }

// outcome is a peer failure as a procedure's: the outcomes are the same
// words, recorded with the same codes.
func outcome(err error) error {
	var f *peer.Failure
	if errors.As(err, &f) {
		return procedure.Failf(procedure.Outcome(f.Outcome), "%s", f.Msg)
	}
	return err
}

func (p *dutPeer) SetMode(ctx context.Context, mode string, timeout time.Duration) error {
	return outcome(p.d.SetMode(ctx, mode, timeout))
}

func (p *dutPeer) Invoke(ctx context.Context, action string, timeout time.Duration) error {
	return outcome(p.d.Invoke(ctx, action, timeout))
}

func (p *dutPeer) Deviations(ctx context.Context, ms []string, t0, t1 time.Time) (map[string]string, error) {
	dev, err := p.d.Deviations(ctx, ms, t0, t1)
	return dev, outcome(err)
}

func (p *dutPeer) AllOK(ctx context.Context, ms []string, deadline time.Time) (time.Time, bool, error) {
	at, ok, err := p.d.AllOK(ctx, ms, deadline)
	return at, ok, outcome(err)
}

func (p *dutPeer) States(ms []string) map[string]string { return p.d.States(ms) }
func (p *dutPeer) Connected() bool                      { return p.d.Connected() }
