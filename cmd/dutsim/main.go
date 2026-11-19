//go:build linux

// Command dutsim is a simulated device under test on a CAN interface.
//
//	dutsim [flags] interface
//
// While it runs it sends three frames every -period: DutStatus, an alive
// message with a counter modulo 16; EngineData, with an engine speed; and
// DutSupply, the supply voltage as it last read it. It reads its
// supply voltage by polling a psusim channel, the supply that is the lab
// rack's stress source in the two-rack demo, and reacts to the voltage falling
// below -vmin in the way -behaviour names:
//
//	glitch   keeps talking, but reports -glitch-speed while the supply is low
//	restore  falls silent, and boots again once the supply is back
//	reset    falls silent, and stays silent until it receives a reset frame
//	never    falls silent for good
//
// A reset frame is DutReset with Request = 1. The frames are laid out by
// dut/dutsim.dbc, which is the database to give ktestd with -dbc; package dut
// says what the DUT models and how.
//
// -transcript writes the DUT's own account of the run: its configuration,
// every supply crossing, every reset frame, every change of state, each with
// its time. Nothing that writes Logb writes it, so a check can hold a
// recording and a procedure's verdicts to it.
//
// dutsim only queries the supply, with MEAS:VOLT?, and never clears its error
// queue: the supply belongs to the other rack, whose daemon reads that queue.
// The queries do appear in psusim's transcript, which logbcheck passes over.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rveen/ktest/can"
	"github.com/rveen/ktest/dut"
)

type options struct {
	iface      string
	supply     string // host:port of the SCPI supply
	ch         int    // its channel, from 1
	poll       time.Duration
	period     time.Duration
	cfg        dut.Config
	transcript io.Writer
}

func main() {
	var (
		o          options
		behaviour  = flag.String("behaviour", "restore", "what a dip below -vmin does: glitch, restore, reset or never")
		transcript = flag.String("transcript", "", "write the DUT's account of the run to this file")
	)
	flag.StringVar(&o.supply, "supply", "127.0.0.1:5025", "the psusim (or SCPI supply) to read the supply voltage from")
	flag.IntVar(&o.ch, "ch", 1, "the supply channel the DUT is powered from")
	flag.DurationVar(&o.poll, "poll", 5*time.Millisecond, "how often to read the supply voltage")
	flag.DurationVar(&o.period, "period", 10*time.Millisecond, "how often to send DutStatus and EngineData")
	flag.Float64Var(&o.cfg.VMin, "vmin", 9, "volts: below this the supply is low")
	flag.Float64Var(&o.cfg.VRestore, "vrestore", 0, "volts: at or above this the supply is restored (default vmin + 0.5)")
	flag.DurationVar(&o.cfg.Boot, "boot", 100*time.Millisecond, "how long the DUT is silent after power returns or a reset")
	flag.Float64Var(&o.cfg.Speed, "speed", 3000, "the engine speed it reports, rpm")
	flag.Float64Var(&o.cfg.GlitchSpeed, "glitch-speed", 0, "the engine speed it reports while glitched, rpm")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: dutsim [flags] interface\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	o.iface = flag.Arg(0)

	var err error
	if o.cfg.Behaviour, err = dut.ParseBehaviour(*behaviour); err != nil {
		fatalf("-behaviour: %v", err)
	}
	if o.cfg.VRestore == 0 {
		o.cfg.VRestore = o.cfg.VMin + 0.5
	}
	switch {
	case o.ch < 1:
		fatalf("-ch %d: supply channels count from 1", o.ch)
	case o.poll <= 0 || o.period <= 0:
		fatalf("-poll and -period must be positive")
	case o.poll > o.period:
		// A dip shorter than a poll can go unseen, and one shorter than a
		// period would then not show on the bus at all: say so rather than
		// run a DUT that misses what it is there to react to.
		fatalf("-poll %v is slower than -period %v: the DUT would see the supply less often than it talks", o.poll, o.period)
	}
	if *transcript != "" {
		f, err := os.Create(*transcript)
		if err != nil {
			fatalf("%v", err)
		}
		defer f.Close()
		o.transcript = f
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o, func(s string) { fmt.Fprintln(os.Stderr, "dutsim:", s) }); err != nil {
		fatalf("%v", err)
	}
}

// reading is one supply voltage, or the error that stopped the poller.
type reading struct {
	t   time.Time
	v   float64
	err error
}

// run is the DUT until ctx ends, or until the bus or the supply fails. It is
// one goroutine deciding, fed by two that only read: the supply poller and the
// bus receiver. Every decision is the model's, with the time of the event that
// caused it.
func run(ctx context.Context, o options, say func(string)) error {
	codec, err := dut.NewCodec()
	if err != nil {
		return err
	}
	sim, err := dut.New(o.cfg, time.Now(), o.transcript)
	if err != nil {
		return err
	}
	bus, err := can.Open(o.iface, can.Options{Stamp: can.StampTimestampNS})
	if err != nil {
		return err
	}
	defer bus.Close()
	// Receive with a timeout, so that the receiver sees ctx end: closing a
	// socket does not reliably wake a recvmsg blocked on it.
	if err := bus.SetReadTimeout(100 * time.Millisecond); err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", o.supply, time.Second)
	if err != nil {
		return fmt.Errorf("supply: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readings := make(chan reading, 1)
	resets := make(chan time.Time, 16)
	busErr := make(chan error, 1)
	go pollSupply(ctx, conn, o.ch, o.poll, readings)
	go receive(ctx, bus, codec, resets, busErr)

	say(fmt.Sprintf("on %s, powered from %s channel %d; %s below %gV",
		o.iface, o.supply, o.ch, o.cfg.Behaviour, o.cfg.VMin))

	tick := time.NewTicker(o.period)
	defer tick.Stop()
	status := can.New(codec.Status.ID, nil)
	engine := can.New(codec.Engine.ID, nil)
	supply := can.New(codec.Supply.ID, nil)
	for {
		select {
		case <-ctx.Done():
			sim.Note(time.Now(), "stop")
			return nil
		case r := <-readings:
			if r.err != nil {
				sim.Note(r.t, "supply unreadable: %v", r.err)
				return fmt.Errorf("supply: %w", r.err)
			}
			sim.Supply(r.t, r.v)
		case t := <-resets:
			sim.ResetFrame(t)
		case err := <-busErr:
			sim.Note(time.Now(), "bus failed: %v", err)
			return err
		case <-tick.C:
			counter, speed, ok := sim.Tick(time.Now())
			if !ok {
				continue
			}
			for _, f := range []struct {
				frame *can.Frame
				enc   func() ([]byte, error)
			}{
				{status, func() ([]byte, error) { return codec.Alive(counter) }},
				{engine, func() ([]byte, error) { return codec.Speed(speed) }},
				{supply, func() ([]byte, error) { return codec.SupplyVoltage(sim.Voltage()) }},
			} {
				p, err := f.enc()
				if err != nil {
					return err
				}
				f.frame.Len = copy(f.frame.Data[:], p)
				if err := bus.Send(f.frame); err != nil {
					sim.Note(time.Now(), "bus failed: %v", err)
					return fmt.Errorf("send: %w", err)
				}
			}
		}
	}
}

// pollSupply reads the channel's measured voltage every poll, until ctx ends
// or a read fails. A reading is stamped halfway through its query, which is
// as near as the outside of a socket gets to when the supply measured it.
//
// It writes MEAS:VOLT? and nothing else. In particular no *CLS: the error
// queue is the supply's own and the other rack's daemon reads it.
func pollSupply(ctx context.Context, conn net.Conn, ch int, poll time.Duration, out chan<- reading) {
	r := bufio.NewReader(conn)
	q := fmt.Sprintf("MEAS:VOLT? (@%d)\n", ch)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	send := func(rd reading) bool {
		select {
		case out <- rd:
			return rd.err == nil
		case <-ctx.Done():
			return false
		}
	}
	for {
		t0 := time.Now()
		conn.SetDeadline(t0.Add(time.Second))
		_, err := io.WriteString(conn, q)
		var line string
		if err == nil {
			line, err = r.ReadString('\n')
		}
		t1 := time.Now()
		var v float64
		if err == nil {
			line = strings.TrimSpace(line)
			if v, err = strconv.ParseFloat(line, 64); err != nil {
				err = fmt.Errorf("%q answered %q, which is not a number", strings.TrimSpace(q), line)
			}
		}
		if ctx.Err() != nil || !send(reading{t: t0.Add(t1.Sub(t0) / 2), v: v, err: err}) {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

// receive passes on the reset frames the bus carries, each with its kernel
// timestamp, or the time it was read when the kernel gave none.
func receive(ctx context.Context, bus *can.Conn, codec *dut.Codec, resets chan<- time.Time, failed chan<- error) {
	var f can.Frame
	for ctx.Err() == nil {
		if err := bus.Recv(&f); err != nil {
			if errors.Is(err, unix.EAGAIN) {
				continue
			}
			if ctx.Err() == nil {
				failed <- fmt.Errorf("receive: %w", err)
			}
			return
		}
		if f.Err() || f.RTR() || f.FD() || !codec.IsReset(f.Arbitration(), f.Extended(), f.Payload()) {
			continue
		}
		t := f.Time
		if !f.Stamped {
			t = time.Now()
		}
		select {
		case resets <- t:
		case <-ctx.Done():
			return
		}
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "dutsim: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}
