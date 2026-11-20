// Command peer-conformance checks an implementation of the Ktest peer
// protocol (doc/peer-protocol.md) against the specification, over the
// network, whatever it is written in.
//
//	peer-conformance secondary -url https://host:port -cacert rack.pem -contract dut-interface.json \
//	    -token operate.token [-other operate2.token] [-view view.token] [-mode m] [-action a]
//
// acts as a master towards a DUT rack and checks what it answers and streams.
// It takes the lease, arms, enters and leaves a mode, runs -action if given,
// and disarms: run it against a rack on a bench, never during a test.
//
//	peer-conformance master [-addr 127.0.0.1:8446] [-dir conformance] [-phase 3s]
//
// runs a reference DUT rack for the master under test to connect to, with
// the -peer settings it prints, puts it through every fault a master must
// protect against, and checks what the master does that a DUT rack can see.
// The report's timeline says when each fault began and ended: hold the
// master's own recording to it, since removing the stress is the master's.
//
// Either exits 1 if a check failed. The monitors' semantics are checked
// offline, on a rack's recording, with monitor-check -live.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/trukeio/ktest/conformance"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var rep *conformance.Report
	var err error
	switch os.Args[1] {
	case "secondary":
		fs := flag.NewFlagSet("secondary", flag.ExitOnError)
		var cfg conformance.SecondaryConfig
		fs.StringVar(&cfg.URL, "url", "", "the DUT rack, https://host:port")
		fs.StringVar(&cfg.CACert, "cacert", "", "its certificate, PEM")
		fs.StringVar(&cfg.Contract, "contract", "", "a copy of its contract, dut-interface.json, with its resource beside it")
		fs.StringVar(&cfg.Token, "token", "", "a file holding a token with the operate role")
		fs.StringVar(&cfg.Other, "other", "", "a file holding a second operate token, for the lease's exclusion")
		fs.StringVar(&cfg.View, "view", "", "a file holding a view token")
		fs.StringVar(&cfg.Mode, "mode", "", "the mode to enter and leave (default the contract's first)")
		fs.StringVar(&cfg.Action, "action", "", "an action to run; none if not given, since it acts on the DUT")
		fs.DurationVar(&cfg.Wait, "wait", 10*time.Second, "how long a mode change or an action may take")
		fs.Parse(os.Args[2:])
		if cfg.URL == "" || cfg.Contract == "" || cfg.Token == "" {
			fmt.Fprintln(os.Stderr, "peer-conformance secondary: -url, -contract and -token are required")
			fs.Usage()
			os.Exit(2)
		}
		rep, err = conformance.CheckSecondary(ctx, cfg)
	case "master":
		fs := flag.NewFlagSet("master", flag.ExitOnError)
		var cfg conformance.MasterConfig
		fs.StringVar(&cfg.Addr, "addr", "127.0.0.1:8446", "where the reference rack listens")
		fs.StringVar(&cfg.Dir, "dir", "conformance", "where it writes the contract, certificate and token for the master, and its recording")
		fs.DurationVar(&cfg.Phase, "phase", 3*time.Second, "how long each fault lasts")
		fs.DurationVar(&cfg.Connect, "connect", 2*time.Minute, "how long the master has to connect")
		fs.DurationVar(&cfg.Reconnect, "reconnect", 15*time.Second, "how long the master has to come back after a fault")
		fs.Parse(os.Args[2:])
		cfg.Say = func(s string) { fmt.Fprintf(os.Stderr, "peer-conformance: %s\n", s) }
		rep, err = conformance.Master(ctx, cfg)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "peer-conformance:", err)
		os.Exit(2)
	}
	rep.Write(os.Stdout)
	if rep.Failed() > 0 {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: peer-conformance secondary [flags]   check a DUT rack\n"+
		"       peer-conformance master [flags]      check a master against a reference DUT rack")
	os.Exit(2)
}
