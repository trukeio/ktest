// Command library-rack is a DUT rack that is not ktestd: a program of its own,
// built on package secondary, that a ktestd lab rack drives as its peer
// exactly as it drives a ktestd DUT rack (doc/peer-protocol.md).
//
//	library-rack [flags]
//
// Its DUT is the simulated one of package dut, run in-process rather than on
// a CAN bus: the program reads the DUT's supply from a psusim channel, the lab
// rack's stress, and records what the DUT reports every -period in three
// streams of its own, DutStatus, EngineData and DutSupply. The contract,
// dut-interface.json, reads them under the same names as the two-rack demo's,
// so the lab rack's procedures run against this rack unchanged.
//
// Its actions are Go functions, not Robot keywords: actions.txt is their
// resource, which the contract names and both racks hash, and which says in
// prose what each does.
//
// It is meant to be read: everything a program must do to be a DUT rack is
// in main and run, and everything that is the DUT is in the dutRack type.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/dut"
	"github.com/trukeio/ktest/peer"
	"github.com/trukeio/ktest/record"
	"github.com/trukeio/ktest/secondary"
)

// The streams this rack records of its DUT, laid out with record.Schema: the
// time axis first, then the fields. The contract's inputs name them.
var (
	statusSchema = record.Schema("DutStatus", 8*16, []logb.Field{
		{Name: "counter", BitOffset: 64, BitWidth: 8, Type: logb.TypeUint,
			Desc: "the DUT's alive counter, modulo 16, restarting at 0 on every boot"},
	}, map[string]string{"source": "library-rack, the simulated DUT"})
	engineSchema = record.Schema("EngineData", 8*16, []logb.Field{
		{Name: "EngineSpeed", BitOffset: 64, BitWidth: 64, Type: logb.TypeFloat, Unit: "rpm",
			Desc: "the engine speed the DUT reports"},
	}, map[string]string{"source": "library-rack, the simulated DUT"})
	supplySchema = record.Schema("DutSupply", 8*16, []logb.Field{
		{Name: "Voltage", BitOffset: 64, BitWidth: 64, Type: logb.TypeFloat, Unit: "V",
			Desc: "the supply voltage as the DUT last read it"},
	}, map[string]string{"source": "library-rack, the simulated DUT"})
)

func main() {
	var (
		contractPath = flag.String("contract", "dut-interface.json", "the DUT's contract")
		addr         = flag.String("https", "127.0.0.1:8445", "the address to serve the peer protocol on")
		tokensPath   = flag.String("tokens", "tokens", `who may connect: lines "name role sha256-of-token", as ktestd -new-token writes them`)
		certPath     = flag.String("cert", "cert.pem", "where to write this run's TLS certificate, for the master to trust")
		out          = flag.String("o", "dut.logb", "the recording")
		supply       = flag.String("supply", "127.0.0.1:5025", "the psusim the DUT is powered from")
		ch           = flag.Int("ch", 1, "its channel")
		period       = flag.Duration("period", 10*time.Millisecond, "how often the DUT reports")
		behaviour    = flag.String("behaviour", "reset", "what a dip below 9 V does: glitch, restore, reset or never")
		transcript   = flag.String("transcript", "", "write the DUT's own account of the run here, for logbcheck -dutsim")
	)
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *contractPath, *addr, *tokensPath, *certPath, *out, *supply, *ch, *period, *behaviour, *transcript); err != nil {
		fmt.Fprintln(os.Stderr, "library-rack:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, contractPath, addr, tokensPath, certPath, out, supply string, ch int,
	period time.Duration, behaviour, transcript string) error {

	// The DUT.
	b, err := dut.ParseBehaviour(behaviour)
	if err != nil {
		return err
	}
	var tr io.Writer
	if transcript != "" {
		f, err := os.Create(transcript)
		if err != nil {
			return err
		}
		defer f.Close()
		tr = f
	}
	sim, err := dut.New(dut.Config{Behaviour: b, VMin: 9, VRestore: 9.5, Boot: 100 * time.Millisecond, Speed: 3000},
		time.Now(), tr)
	if err != nil {
		return err
	}
	d := &dutRack{sim: sim}

	// The rack: the contract against these streams, the actions, who may
	// connect, and the recording.
	auth, err := secondary.LoadTokens(tokensPath)
	if err != nil {
		return err
	}
	rec, err := os.Create(out)
	if err != nil {
		return err
	}
	defer rec.Close()
	host, _ := os.Hostname()
	srv, err := secondary.New(secondary.Config{
		Contract:  contractPath,
		Streams:   []*logb.Schema{statusSchema, engineSchema, supplySchema},
		Actions:   d,
		Auth:      auth,
		Recording: rec,
		Name:      "library-rack",
		Source:    "library-rack",
		Meta:      map[string]string{"daemon.version": "library-rack (example)", "host.name": host},
		OnDisarm:  func() { fmt.Fprintln(os.Stderr, "library-rack: disarmed") },
	})
	if err != nil {
		return err
	}
	d.srv = srv
	d.status, d.engine, d.supply = srv.Stream("DutStatus"), srv.Stream("EngineData"), srv.Stream("DutSupply")

	// The network: TLS with a certificate made for this run, which the
	// master pins.
	cert, err := secondary.SelfSigned(certPath, 365*24*time.Hour, "localhost", "127.0.0.1")
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	defer hs.Close()

	// The supply, polled.
	conn, err := net.DialTimeout("tcp", supply, time.Second)
	if err != nil {
		return fmt.Errorf("supply: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	pollDone := make(chan error, 1)
	go func() { pollDone <- d.poll(ctx, conn, ch) }()
	fmt.Fprintf(os.Stderr, "library-rack: contract %s (sha256 %.12s…) on https://%s, certificate %s; DUT %s below 9V, powered from %s channel %d\n",
		srv.Contract().Name, srv.Contract().Hash, addr, certPath, b, supply, ch)

	tick := time.NewTicker(period)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return <-srvDone
		case err := <-pollDone:
			cancel()
			<-srvDone
			return err
		case err := <-srvDone:
			return err
		case t := <-tick.C:
			if err := d.report(t); err != nil {
				cancel()
				<-srvDone
				return err
			}
		}
	}
}

// dutRack is the DUT as the rack runs it: the simulation, fed by the supply
// poller and the tick, and the actions the master may ask for.
type dutRack struct {
	mu  sync.Mutex
	sim *dut.Sim

	srv                    *secondary.Server
	status, engine, supply *secondary.Stream
}

// report is one transmit slot: what the DUT says, if it is talking,
// recorded at t.
func (d *dutRack) report(t time.Time) error {
	d.mu.Lock()
	counter, speed, ok := d.sim.Tick(t)
	v := d.sim.Voltage()
	d.mu.Unlock()
	if !ok {
		return nil // silent: the monitors notice by the deadlines age() sets
	}
	if err := d.srv.Write(d.status, t, func(rec []byte) { rec[8] = counter }); err != nil {
		return err
	}
	if err := d.srv.Write(d.engine, t, func(rec []byte) { putFloat(rec[8:], speed) }); err != nil {
		return err
	}
	return d.srv.Write(d.supply, t, func(rec []byte) { putFloat(rec[8:], v) })
}

func putFloat(b []byte, v float64) { binary.LittleEndian.PutUint64(b, math.Float64bits(v)) }

// Run is the contract's actions (secondary.Actions), as actions.txt says.
func (d *dutRack) Run(ctx context.Context, action string) error {
	switch action {
	case "enter_active", "leave_active":
		// The simulated DUT needs nothing to enter or leave a mode; a real
		// one would be sent its tester-present, or stopped being sent it.
		return nil
	case "reset":
		d.mu.Lock()
		d.sim.ResetFrame(time.Now())
		d.mu.Unlock()
		// Done when the DUT runs again, within a second.
		deadline := time.Now().Add(time.Second)
		for {
			d.mu.Lock()
			st := d.sim.State()
			d.mu.Unlock()
			if st == dut.Running {
				return nil
			}
			if time.Now().After(deadline) {
				return &peer.Failure{Outcome: peer.Fail, Msg: fmt.Sprintf("the DUT is %v a second after its reset", st)}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return fmt.Errorf("no action %q", action)
}

// poll reads the DUT's supply every 5 ms, as cmd/dutsim does: MEAS:VOLT? and
// nothing else, since the supply's error queue is the lab rack's to read.
// A reading is stamped halfway through its query.
func (d *dutRack) poll(ctx context.Context, conn net.Conn, ch int) error {
	r := bufio.NewReader(conn)
	q := fmt.Sprintf("MEAS:VOLT? (@%d)\n", ch)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		t0 := time.Now()
		conn.SetDeadline(t0.Add(time.Second))
		if _, err := io.WriteString(conn, q); err != nil {
			return fmt.Errorf("supply: %w", err)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("supply: %w", err)
		}
		t1 := time.Now()
		v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err != nil {
			return fmt.Errorf("supply answered %q", strings.TrimSpace(line))
		}
		d.mu.Lock()
		d.sim.Supply(t0.Add(t1.Sub(t0)/2), v)
		d.mu.Unlock()
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil
		}
	}
}
