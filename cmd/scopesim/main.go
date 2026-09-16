// Command scopesim is a simulated oscilloscope: SCPI over TCP, in the Siglent
// SDS1000X-E dialect, with a described signal on every channel.
//
//	scopesim [flags]
//
// It exists so that ktestd's scope driver, the rack's waveform widget and
// logbcheck can be exercised with no hardware; see package scope for the
// dialect it speaks and where that dialect's own documentation disagrees with
// itself. The disputed readings are flags here, so that the hardware slice
// changes a flag rather than a design.
//
// -transcript logs the simulator's own account of the run — every command it
// received, what it holds after each setting, and one line per acquisition with
// its settings, its frame time and the sha256 of its bytes. Nothing that writes
// Logb writes it, which is what makes it able to falsify a recording:
//
//	scopesim -transcript scope.log -channels 'sine:amp=2,freq=1000;square:amp=1,freq=100' &
//	ktestd -scope scope1=127.0.0.1:5026 -sock k.sock -d 8s -o run.logb vcan0 &
//	logbcheck -scope scope1=scope.log run.logb ref.log
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/rveen/ktest/scope"
)

func main() {
	addr := flag.String("addr", ":5026", "where to listen; 5025 is SCPI's raw socket port, and psusim has it")
	chans := flag.String("channels", "sine:amp=1,freq=1000;dc:amp=0",
		"one signal a channel, semicolon separated: kind:key=value,...\n"+
			"\tkinds are sine, square, ramp, step, noise and dc; keys are amp, freq, ofst, phase, noise and seed")
	vdiv := flag.Float64("vdiv", 1, "volts a division each channel starts at")
	on := flag.String("on", "1", "which channels are displayed at start, comma separated; two share the memory")
	sign := flag.String("sign", "sub256", "how a code above 127 becomes negative: sub256 (guide PG01-E02D) or sub255 (PG01-E02C)")
	trdelay := flag.Bool("trigger-delay", false, "take TRDL into the time axis, as third-party readers do and the guides do not")
	acquire := flag.Duration("acquire", 10*time.Millisecond, "how long an acquisition takes to complete after ARM")
	interval := flag.Duration("interval", time.Millisecond, "how far apart Sequence segments are triggered")
	rate := flag.Float64("transfer-rate", 0, "hand a readout over this slowly, in bytes a second; 0 is as fast as it can")
	transcript := flag.String("transcript", "", "write the simulator's own account of the run to this file")
	flag.Parse()

	rule, err := scope.ParseSignRule(*sign)
	if err != nil {
		fatalf("%v", err)
	}
	sim := &scope.Sim{Model: "scopesim", Sign: rule, TrigDelay: *trdelay,
		Acquire: *acquire, Interval: *interval, TransferRate: *rate}
	for _, c := range strings.Split(*chans, ";") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		sig, err := scope.ParseSignal(c)
		if err != nil {
			fatalf("%v", err)
		}
		sim.Channels = append(sim.Channels, scope.SimChannel{Signal: sig, VDiv: *vdiv})
	}
	if len(sim.Channels) == 0 {
		fatalf("a scope has at least one channel")
	}
	for _, n := range strings.Split(*on, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		var ch int
		if _, err := fmt.Sscanf(n, "%d", &ch); err != nil || ch < 1 || ch > len(sim.Channels) {
			fatalf("-on %q: this scope has channels 1 to %d", n, len(sim.Channels))
		}
		sim.Channels[ch-1].On = true
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
	fmt.Fprintf(os.Stderr, "scopesim: %d channels on %s, sign rule %s, trigger delay %t\n",
		len(sim.Channels), l.Addr(), rule, *trdelay)
	for i, c := range sim.Channels {
		fmt.Fprintf(os.Stderr, "scopesim:   C%d %s%s\n", i+1, c.Signal,
			map[bool]string{true: " (displayed)"}[c.On])
	}
	fatalf("%v", sim.Serve(l))
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "scopesim: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}
