// Command psusim is a simulated bench supply: SCPI over TCP, in the Keysight
// dialect, with a resistive load on every channel.
//
//	psusim [flags]
//
// It exists so that ktestd's supply driver and the rack can be exercised with
// no hardware; see package psu for what it models and how. -transcript logs
// every command it receives, which is the supply's own account of what it was
// told, written by nothing that writes Logb.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/rveen/ktest/psu"
)

func main() {
	addr := flag.String("addr", ":5025", "where to listen; 5025 is SCPI's raw socket port")
	chans := flag.String("channels", "30:3:10,6:5:100",
		"channels as vmax:imax:load-ohms, comma separated")
	transcript := flag.String("transcript", "", "log every command received to this file")
	flag.Parse()

	sim := &psu.Sim{Model: "psusim"}
	for _, c := range strings.Split(*chans, ",") {
		p := strings.Split(c, ":")
		if len(p) != 3 {
			fatalf("channel %q: want vmax:imax:load", c)
		}
		var v [3]float64
		for i, s := range p {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil || f <= 0 {
				fatalf("channel %q: %q is not a positive number", c, s)
			}
			v[i] = f
		}
		sim.Channels = append(sim.Channels, psu.SimChannel{VMax: v[0], IMax: v[1], Load: v[2]})
	}
	if *transcript != "" {
		f, err := os.Create(*transcript)
		if err != nil {
			fatalf("%v", err)
		}
		defer f.Close()
		sim.Transcript = f
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Fprintf(os.Stderr, "psusim: %d channels on %s\n", len(sim.Channels), l.Addr())
	fatalf("%v", sim.Serve(l))
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "psusim: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}
