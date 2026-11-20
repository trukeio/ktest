package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/conformance"
)

// The fault check (-faults) holds a master's recording to what the
// conformance kit did to it (doc/peer-protocol.md §9.4, §12). peer-conformance
// master puts a master through faults on a reference DUT rack and writes them
// to faults.json, by the reference rack's clock. A master's protection is in
// its own world, so only its own recording can show it; this reads ktestd's.
//
// For each fault the master must take its rule — dut_abort for an abort,
// monitoring_lost for the rest — within -react of the fault's start (and,
// for a frozen rack, within the watermark age the recording declares as
// well, since that is when the master can know), hold it until the fault
// ends, and clear it within -recover of the end. Each rule's taking hold and
// clearing is in the rules stream. The fault times are put on the master's
// clock through the offset it recorded in <peer>.clock, so the two need not
// share a clock. A master that drives other DUT racks too names the
// reference one with -faults-peer; its rules hold for any rack, so the others
// must stay healthy through the run.

type ruleEvent struct {
	t     int64 // the master's clock, Unix ns
	event uint64
	rule  string
}

type clockEstimate struct {
	t           int64 // the master's clock when the sample was taken, Unix ns
	offset, unc int64 // the DUT rack's clock less this one, and its uncertainty
}

func compareFaults(path, faultsPath, peerName string, react, recoverIn time.Duration, verbose bool) (int, []string, error) {
	raw, err := os.ReadFile(faultsPath)
	if err != nil {
		return 0, nil, err
	}
	var ff conformance.FaultFile
	if err := json.Unmarshal(raw, &ff); err != nil {
		return 0, nil, fmt.Errorf("%s: %v", faultsPath, err)
	}
	if len(ff.Faults) == 0 {
		return 0, nil, fmt.Errorf("%s holds no faults", faultsPath)
	}
	rules, clocks, wmAge, err := readRules(path, peerName)
	if err != nil {
		return 0, nil, err
	}
	if len(clocks) == 0 {
		return 0, nil, errors.New("the recording has no <peer>.clock: it is not a master's, or its peer never answered")
	}
	var lines, faults []string
	for _, f := range ff.Faults {
		rule := "monitoring_lost"
		bound := react
		switch f.Kind {
		case conformance.FaultAbort:
			rule = "dut_abort"
		case conformance.FaultFrozen:
			bound += wmAge
		}
		est := estimateAt(clocks, f.Start.UnixNano())
		start := f.Start.UnixNano() - est.offset
		end := f.End.UnixNano() - est.offset
		slack := est.unc + int64(2*time.Millisecond)
		ms := func(ns int64) string { return fmt.Sprintf("%.1f ms", float64(ns)/1e6) }
		held, took, cleared := false, int64(0), int64(0)
		for _, e := range rules {
			if e.rule != rule {
				continue
			}
			switch {
			case e.t < start-slack:
				held = e.event == 1 || (held && e.event == 2)
			case took == 0 && (e.event == 1):
				took = e.t
			case took != 0 && cleared == 0 && e.event == 3:
				cleared = e.t
			}
		}
		what := fmt.Sprintf("%s at %s", f.Kind, f.Start.UTC().Format("15:04:05.000"))
		switch {
		case held:
			faults = append(faults, fmt.Sprintf("%s: %s already held when it began", what, rule))
		case took == 0:
			faults = append(faults, fmt.Sprintf("%s: %s never took hold", what, rule))
		case took-start > int64(bound):
			faults = append(faults, fmt.Sprintf("%s: %s took hold %s after it began, beyond %v", what, rule, ms(took-start), bound))
		case took > end+slack:
			faults = append(faults, fmt.Sprintf("%s: %s took hold only after it ended", what, rule))
		case cleared == 0:
			faults = append(faults, fmt.Sprintf("%s: %s never cleared", what, rule))
		case cleared < end-slack:
			faults = append(faults, fmt.Sprintf("%s: %s cleared %s before it ended", what, rule, ms(end-cleared)))
		case cleared-end > int64(recoverIn):
			faults = append(faults, fmt.Sprintf("%s: %s cleared %s after it ended, beyond %v", what, rule, ms(cleared-end), recoverIn))
		default:
			lines = append(lines, fmt.Sprintf("%-8s %-15s held %s after the start, cleared %s after the end",
				f.Kind, rule, ms(took-start), ms(cleared-end)))
		}
	}
	if len(faults) > 0 {
		if !verbose && len(faults) > 10 {
			faults = append(faults[:10], fmt.Sprintf("... and %d more", len(faults)-10))
		}
		return 0, nil, errors.New(strings.Join(faults, "\n"))
	}
	return len(ff.Faults), lines, nil
}

// estimateAt is the clock estimate in force at t: the last taken at or
// before it, or the first.
func estimateAt(cs []clockEstimate, t int64) clockEstimate {
	i := sort.Search(len(cs), func(i int) bool { return cs[i].t > t })
	if i == 0 {
		return cs[0]
	}
	return cs[i-1]
}

// readRules reads the rules stream, every peer's clock estimates, and the
// watermark age the recording declares.
func readRules(path, peerName string) ([]ruleEvent, []clockEstimate, time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, err
	}
	defer f.Close()
	r, err := logb.NewReader(f)
	if err != nil {
		return nil, nil, 0, err
	}
	var rules []ruleEvent
	var clocks []clockEstimate
	epoch := int64(-1)
	peers := map[string]bool{}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, 0, err
		}
		if epoch < 0 {
			for _, kv := range r.Meta {
				if kv.Key == "recording.epoch" {
					epoch, _ = strconv.ParseInt(kv.Value, 10, 64)
				}
			}
			if epoch < 0 {
				return nil, nil, 0, errors.New("the recording states no recording.epoch")
			}
		}
		name := b.Schema.Name
		isClock := strings.HasSuffix(name, ".clock") && fieldOf(b.Schema, "offset") >= 0 && fieldOf(b.Schema, "uncertainty") >= 0
		if name != "rules" && !isClock {
			continue
		}
		if isClock {
			pn := strings.TrimSuffix(name, ".clock")
			peers[pn] = true
			if peerName != "" && pn != peerName {
				continue
			}
		}
		for i := 0; i < int(b.Count); i++ {
			ax, err := b.Axis(i)
			if err != nil {
				return nil, nil, 0, err
			}
			d, err := ax.Duration(b.Schema.AxisExp)
			if err != nil {
				return nil, nil, 0, err
			}
			t := epoch + int64(d)
			val := func(f string) any { v, _ := b.Value(i, fieldOf(b.Schema, f)); return v }
			if isClock {
				off, _ := val("offset").(int64)
				unc, _ := val("uncertainty").(int64)
				clocks = append(clocks, clockEstimate{t, off, unc})
				continue
			}
			ev, _ := uintField(b, i, fieldOf(b.Schema, "event"))
			rule, _ := val("rule").(string)
			rules = append(rules, ruleEvent{t, ev, rule})
		}
	}
	switch {
	case peerName != "" && !peers[peerName]:
		return nil, nil, 0, fmt.Errorf("the recording has no DUT rack named %s", peerName)
	case peerName == "" && len(peers) > 1:
		return nil, nil, 0, fmt.Errorf("the recording has %d DUT racks; name the reference rack with -faults-peer", len(peers))
	}
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].t < rules[j].t })
	sort.SliceStable(clocks, func(i, j int) bool { return clocks[i].t < clocks[j].t })
	var wmAge time.Duration
	for _, kv := range r.Meta {
		if kv.Key == "rules.monitoring_lost.watermark_age" {
			wmAge, _ = time.ParseDuration(kv.Value)
		}
	}
	if wmAge == 0 {
		return nil, nil, 0, errors.New("the recording declares no rules.monitoring_lost.watermark_age: its master has no protection rules")
	}
	return rules, clocks, wmAge, nil
}

func fieldOf(s *logb.Schema, name string) int {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			return i
		}
	}
	return -1
}
