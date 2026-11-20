package peer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/trukeio/ktest/contract"
)

// Link is a master's connection to a DUT rack, kept up until its context
// ends (doc/peer-protocol.md §9.1–§9.3): each connection checks the protocol
// and the contract again, since the DUT rack may have been restarted with
// another; then it holds the DUT's lease, armed, and reads the subscription.
// The clocks' offset is sampled throughout.
//
// It holds no policy. It hands over what happens as Events, for the master
// to record, to feed a View with, and to judge.
type Link struct {
	Client   *Client
	Contract *contract.Identity
	// Streams are the streams subscribed to; nil is Mirrored.
	Streams []string
	// LeaseTTL is the lease asked for, renewed at a third of it; 0 is 30 s.
	LeaseTTL time.Duration
	// Retry is the pause between connections; 0 is 500 ms.
	Retry time.Duration
	// ClockEvery is how often the clock is sampled; 0 is 1 s.
	ClockEvery time.Duration

	lease atomic.Pointer[string]
}

// Mirrored are the streams a master needs: every monitor, the mode, every
// action. The watermark and the gaps come with any subscription.
var Mirrored = []string{"mon.*", "mode", "dut.action.*"}

// Event is one thing a Link hands over: exactly one of its fields is set.
type Event struct {
	Link  *LinkEvent
	Clock *Estimate
	Item  *Item
}

// LinkEvent is the link changing, or a refusal, with why.
type LinkEvent struct {
	Kind LinkKind
	Msg  string
}

// LinkKind is what happened to the link.
type LinkKind byte

const (
	// LinkDown: the connection was lost, or could not be made.
	LinkDown LinkKind = iota
	// LinkUp: connected, the contract checked, the subscription open.
	LinkUp
	// LinkRefused: the DUT rack answered, and refused: another contract or
	// protocol, or a refused subscription. The link is down.
	LinkRefused
	// LeaseRefused: the DUT's lease could not be taken, armed or renewed.
	// The link stands: the monitors protect whether or not this rack may
	// change the DUT's mode.
	LeaseRefused
)

// ContractError is a DUT rack running another contract, or speaking another
// protocol, than this rack's.
type ContractError struct{ Err error }

func (e ContractError) Error() string { return e.Err.Error() }
func (e ContractError) Unwrap() error { return e.Err }

// Matches checks that a DUT rack speaks this protocol and runs this
// contract, resource included (§9.1).
func Matches(id *contract.Identity, info Info) error {
	if err := info.Supported(); err != nil {
		return err
	}
	switch {
	case info.Hash != id.Hash:
		return fmt.Errorf("it runs contract %s, sha256 %.12s…, and this rack's copy, %s, is %s, sha256 %.12s…",
			info.Contract, info.Hash, id.Path, id.Name, id.Hash)
	case info.ResourceHash != id.ResourceHash:
		return fmt.Errorf("its contract's resource has sha256 %.12s…, and this rack's copy has %.12s…: "+
			"the actions would not be the ones this rack's procedures were checked against",
			info.ResourceHash, id.ResourceHash)
	}
	return nil
}

// Lease is the DUT's lease token as currently held, "" for none.
func (l *Link) Lease() string {
	if t := l.lease.Load(); t != nil {
		return *t
	}
	return ""
}

// Run keeps the link up until ctx ends, handing each event to send; send
// returning false ends it too. send is called from the session's, the
// lease's and the clock's goroutines, so it must be safe for concurrent use,
// as a send on a channel is; events of one kind arrive in order.
func (l *Link) Run(ctx context.Context, send func(Event) bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := func(e Event) bool {
		if !send(e) {
			cancel()
			return false
		}
		return true
	}
	go l.clockLoop(ctx, stop)
	retry := l.Retry
	if retry <= 0 {
		retry = 500 * time.Millisecond
	}
	for ctx.Err() == nil {
		err := l.session(ctx, stop)
		if ctx.Err() != nil {
			return
		}
		kind := LinkDown
		var re *RefusedError
		var ce ContractError
		if errors.As(err, &re) || errors.As(err, &ce) {
			kind = LinkRefused
		}
		if !stop(Event{Link: &LinkEvent{kind, err.Error()}}) {
			return
		}
		select {
		case <-time.After(retry):
		case <-ctx.Done():
			return
		}
	}
}

func (l *Link) session(ctx context.Context, send func(Event) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	info, err := l.Client.Info(ctx)
	if err != nil {
		return err
	}
	if err := Matches(l.Contract, info); err != nil {
		return ContractError{err}
	}
	streams := l.Streams
	if streams == nil {
		streams = Mirrored
	}
	s, err := l.Client.Subscribe(ctx, streams)
	if err != nil {
		return err
	}
	defer s.Close()
	go l.leaseLoop(ctx, send)
	if !send(Event{Link: &LinkEvent{LinkUp, fmt.Sprintf("contract %s, sha256 %.12s…", info.Contract, info.Hash)}}) {
		return ctx.Err()
	}
	for {
		it, err := s.Next()
		if err != nil {
			return err
		}
		if !send(Event{Item: &it}) {
			return ctx.Err()
		}
	}
}

func (l *Link) leaseLoop(ctx context.Context, send func(Event) bool) {
	ttl := l.LeaseTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	token := ""
	defer l.lease.Store(nil)
	for {
		var err error
		if token == "" {
			// Taken, then armed: holding the DUT's lease is this rack's
			// intent to change its mode and run its actions.
			if token, err = l.Client.Lease(ctx, "", ttl); err == nil {
				if err = l.Client.Arm(ctx, token); err != nil {
					token = ""
				}
			}
		} else if err = l.Client.Renew(ctx, token); err != nil {
			token = ""
		}
		if token != "" {
			t := token
			l.lease.Store(&t)
		} else {
			l.lease.Store(nil)
		}
		if err != nil && ctx.Err() == nil {
			send(Event{Link: &LinkEvent{LeaseRefused, "the DUT's lease: " + err.Error()}})
		}
		select {
		case <-time.After(ttl / 3):
		case <-ctx.Done():
			if token != "" {
				rctx, cancel := context.WithTimeout(context.Background(), time.Second)
				l.Client.Release(rctx, token)
				cancel()
			}
			return
		}
	}
}

func (l *Link) clockLoop(ctx context.Context, send func(Event) bool) {
	every := l.ClockEvery
	if every <= 0 {
		every = time.Second
	}
	var o Offsets
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if s, err := l.Client.Clock(ctx); err == nil {
			e := o.Add(s)
			if !send(Event{Clock: &e}) {
				return
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}
