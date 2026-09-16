//go:build linux

// Command vcanprobe measures what a SocketCAN interface can and cannot do.
//
// It exists because doc/test-software-outline.md rests on measurements rather
// than on what the manual pages promise. §8 chooses SO_TIMESTAMPNS over
// SO_TIMESTAMPING, and CAN_BCM over a userspace timer, on the strength of
// numbers this command produces; §10.5 states a rule about unreported gaps
// that the overflow probe demonstrates. Re-run it on a new kernel, adapter or
// distribution before trusting any of those figures there.
//
// Nothing here configures an interface. Per §8 interfaces are opened, not
// configured, so the operator brings one up first:
//
//	modprobe vcan
//	ip link add dev vcan0 type vcan
//	ip link set up vcan0
//
// and this command runs against whatever it finds, as a plain user.
//
// The pacing and overflow probes generate heavy traffic for a second or two
// each. Point it at a vcan interface, not at a bus with something attached.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
)

func main() {
	var (
		iface    = flag.String("i", "vcan0", "interface to probe")
		run      = flag.String("run", "", "run only probes whose name matches this regexp")
		asJSON   = flag.Bool("json", false, "emit results as JSON")
		list     = flag.Bool("list", false, "list the probes and exit")
		stampOne = flag.String("stamp-one", "", "internal: measure one timestamp option in this process")
	)
	flag.Usage = usage
	flag.Parse()

	// The timestamp probe re-executes this command once per option, because
	// the options interfere with each other within a process. See
	// probeTimestamp.
	if *stampOne != "" {
		if err := stampOneMain(*iface, *stampOne); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	all := probes()
	if *list {
		for _, p := range all {
			fmt.Printf("  %-11s %s\n", p.name, p.desc)
		}
		return
	}

	var only *regexp.Regexp
	if *run != "" {
		re, err := regexp.Compile(*run)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad -run pattern:", err)
			os.Exit(2)
		}
		only = re
	}

	type report struct {
		Probe   string         `json:"probe"`
		Summary string         `json:"summary,omitempty"`
		Detail  map[string]any `json:"detail,omitempty"`
		Error   string         `json:"error,omitempty"`
	}
	var out []report
	failed := false

	if !*asJSON {
		fmt.Printf("vcanprobe on %s\n\n", *iface)
	}
	for _, p := range all {
		if only != nil && !only.MatchString(p.name) {
			continue
		}
		summary, detail, err := p.run(*iface)
		r := report{Probe: p.name, Summary: summary, Detail: detail}
		if err != nil {
			r.Error = err.Error()
			failed = true
		}
		out = append(out, r)
		if *asJSON {
			continue
		}
		if err != nil {
			fmt.Printf("  %-11s FAIL  %v\n", p.name, err)
			continue
		}
		fmt.Printf("  %-11s %s\n", p.name, summary)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"interface":     *iface,
			"probes":        out,
			"eintr_retries": eintrs,
		})
	} else {
		fmt.Printf("\n  %-11s %d blocking recvmsg calls were interrupted and retried\n", "eintr", eintrs)
		if eintrs > 0 {
			fmt.Println("              (a raw CAN reader without an EINTR retry loses these frames silently)")
		}
	}
	if failed {
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
vcanprobe measures what a SocketCAN interface can and cannot do.

It does not configure interfaces. Bring one up first:

    modprobe vcan
    ip link add dev vcan0 type vcan
    ip link set up vcan0

then run the probes:

    vcanprobe                  all probes against vcan0
    vcanprobe -i can0          against a real adapter
    vcanprobe -run pacing      one probe
    vcanprobe -json            machine-readable, for recording as capabilities

The pacing, throughput and overflow probes generate heavy traffic for a second
or two each. Point this at a vcan interface, not at a live bus.

Flags:
`, "\n"))
	flag.PrintDefaults()
}
