package main

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rveen/logb"
)

// psuRec is one record of a supply channel's state stream.
type psuRec struct {
	t          float64
	seq        uint32
	event      uint64
	connected  bool
	outApplied bool // false when the supply was not answering, and the field absent
}

// The event field of a supply channel's stream, as its schema describes it.
const (
	psRequested    = 1
	psApplied      = 2
	psReadback     = 3
	psConnected    = 4
	psDisconnected = 5
	psOff          = 6
)

// psuChannel matches a supply channel's state stream, psu1.ch1, and not its
// measurements, psu1.ch1.meas.
var psuChannel = regexp.MustCompile(`^[A-Za-z0-9_-]+\.ch[0-9]+$`)

func readPsu(b *logb.Batch) ([]psuRec, error) {
	idx, err := fieldIndex(b.Schema, "seq", "event", "connected", "out.applied")
	if err != nil {
		return nil, err
	}
	var out []psuRec
	for i := 0; i < int(b.Count); i++ {
		axis, err := b.Axis(i)
		if err != nil {
			return nil, err
		}
		seq, err := uintField(b, i, idx["seq"])
		if err != nil {
			return nil, err
		}
		ev, err := uintField(b, i, idx["event"])
		if err != nil {
			return nil, err
		}
		conn, err := b.Value(i, idx["connected"])
		if err != nil {
			return nil, err
		}
		// Absent while the supply is not answering: a guard, not an error.
		on, _ := b.Value(i, idx["out.applied"])
		out = append(out, psuRec{t: axis.Seconds(b.Schema.AxisExp), seq: uint32(seq), event: ev,
			connected: conn == true, outApplied: on == true})
	}
	return out, nil
}

// limitOf is the limit the recording states for a channel write. stated is
// false when it states nothing for the channel, which a daemon that wrote it
// must not do; has is false when it states "none", or the quantity has no
// limit, as a switch does not.
func limitOf(meta map[string]string, channel string) (limit float64, has, stated bool) {
	base, q, ok := splitChannel(channel)
	if !ok {
		return 0, false, false
	}
	s, ok := meta["limits."+base+"."+q+"_max"]
	if !ok {
		return 0, false, false
	}
	if s == "none" {
		return 0, false, true
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, false
	}
	return v, true, true
}

// splitChannel takes a settable channel apart into the part it belongs to and
// the quantity: psu1.ch2.v.set into psu1.ch2 and v, scope1.ch1.vert.vdiv.set
// into scope1.ch1.vert and vdiv (§4). The part is whatever lies between the
// instrument and the quantity, because the instruments do not all divide the
// same way, and it is the key the limits are stated against.
func splitChannel(channel string) (base, q string, ok bool) {
	p := strings.Split(channel, ".")
	if len(p) < 4 || p[len(p)-1] != "set" {
		return "", "", false
	}
	return strings.Join(p[:len(p)-2], "."), p[len(p)-2], true
}

// instrumentOf is the instrument a channel names.
func instrumentOf(channel string) string {
	inst, _, _ := strings.Cut(channel, ".")
	return inst
}

// isSupply and isScope say what kind of instrument the recording declares.
func isSupply(r *recording, inst string) bool {
	_, ok := r.meta["psu."+inst+".channels"]
	return ok
}

func isScope(r *recording, inst string) bool {
	_, ok := r.meta["scope."+inst+".channels"]
	return ok
}

// channels is how many channels the recording says a supply has.
func channels(r *recording, inst string) int {
	n, _ := strconv.Atoi(r.meta["psu."+inst+".channels"])
	return n
}

// checkOff asserts that a disarm or a stop switched off every output of the
// supply that was on, promptly: the stop is the one request whose effect must
// never be in doubt (§7).
func checkOff(r *recording, inst string, c ctlRec, report func(string, ...any)) {
	if !isSupply(r, inst) {
		// A scope has no output to switch off. Disarming one stops its settings
		// being written and leaves the instrument as it is, which is §7's rule
		// that a stop is for outputs.
		return
	}
	const grace = 1.0 // an instrument answers over a network, not a kernel call
	for ch := 1; ch <= channels(r, inst); ch++ {
		name := fmt.Sprintf("%s.ch%d", inst, ch)
		recs := r.psu[name]
		var before *psuRec
		for i := range recs {
			if recs[i].t >= c.t {
				break
			}
			before = &recs[i]
		}
		if before == nil || !before.connected || !before.outApplied {
			continue
		}
		off := false
		for _, p := range recs {
			if p.t >= c.t && p.t <= c.t+grace && p.event == psOff && p.connected && !p.outApplied {
				off = true
				break
			}
		}
		if !off {
			report("%s: output on when a %s at %.3fs switched the supply off, and not off within %.0f ms",
				name, requestNames[c.request], c.t, grace*1000)
		}
	}
}

// checkStops asserts that a stop is one decision: recorded in every
// instrument's stream, under the same number.
func checkStops(r *recording, report func(string, ...any)) {
	streams := map[string][]ctlRec{"the bus": r.control}
	maps.Copy(streams, r.controls)
	seen := map[uint32]string{}
	for _, name := range sortedKeys(streams) {
		for _, c := range streams[name] {
			if c.request == rqStop {
				seen[c.seq] = name
			}
		}
	}
	for seq, where := range seen {
		for _, name := range sortedKeys(streams) {
			found := false
			for _, c := range streams[name] {
				if c.request == rqStop && c.seq == seq {
					found = true
					break
				}
			}
			if !found {
				report("seq %d: a stop recorded for %s and not for %s", seq, where, name)
			}
		}
	}
}

// checkSupplyWrites asserts that a supply was written only as its control
// stream allowed: every write in a channel's stream answers a channel write
// the control plane accepted for that channel, every output switched off
// answers a disarm or a stop, and every accepted write was recorded as asked.
func checkSupplyWrites(r *recording, report func(string, ...any)) {
	for _, inst := range sortedKeys(r.controls) {
		if !isSupply(r, inst) {
			// A scope has no channel streams to answer for its writes; what it
			// does with one is checked against the simulator's transcript
			// instead, with -scope.
			continue
		}
		accepted := map[uint32]ctlRec{}
		for _, c := range r.controls[inst] {
			if c.verdict == vAccepted && c.seq != 0 {
				accepted[c.seq] = c
			}
		}
		requested := map[uint32]bool{}
		for ch := 1; ch <= channels(r, inst); ch++ {
			name := fmt.Sprintf("%s.ch%d", inst, ch)
			for _, p := range r.psu[name] {
				c, ok := accepted[p.seq]
				switch p.event {
				case psRequested, psApplied:
					base, _, _ := splitChannel(c.channel)
					if !ok || c.request != rqChannelSet || base != name {
						report("%s: a write under seq %d at %.3fs, and %s accepted no write to this channel under it",
							name, p.seq, p.t, inst)
					}
					if p.event == psRequested {
						requested[p.seq] = true
					}
				case psOff:
					if !ok || (c.request != rqDisarm && c.request != rqStop) {
						report("%s: the output switched off under seq %d at %.3fs, which is no disarm or stop of %s",
							name, p.seq, p.t, inst)
					}
				}
			}
		}
		for seq, c := range accepted {
			if c.request == rqChannelSet && !requested[seq] {
				report("%s: seq %d, a write to %s accepted and never recorded as requested", inst, seq, c.channel)
			}
		}
	}
}

// A scpiWrite is one setting command, from a transcript or from a recording.
type scpiWrite struct {
	what  string // "v", "i" or "out"
	ch    int
	value float64
	line  int // the transcript's line, 0 for the recording's
}

func (w scpiWrite) String() string {
	return fmt.Sprintf("%s.ch%d = %g", w.what, w.ch, w.value)
}

// readTranscript reads the setting commands out of a psusim transcript: one
// command a line, after the time. Queries and commands that set nothing are
// passed over.
func readTranscript(path string) ([]scpiWrite, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []scpiWrite
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		_, cmd, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		head, args, _ := strings.Cut(strings.TrimSpace(cmd), " ")
		head = strings.ToUpper(head)
		if strings.HasSuffix(head, "?") {
			continue
		}
		for _, p := range []string{"SOURCE:", "SOUR:"} {
			head = strings.TrimPrefix(head, p)
		}
		w := scpiWrite{line: n}
		switch head {
		case "VOLT", "VOLTAGE":
			w.what = "v"
		case "CURR", "CURRENT":
			w.what = "i"
		case "OUTP", "OUTPUT":
			w.what = "out"
		default:
			continue
		}
		val, chs, ok := strings.Cut(args, ",")
		if !ok || !strings.HasPrefix(chs, "(@") || !strings.HasSuffix(chs, ")") {
			return nil, fmt.Errorf("%s:%d: %q has no channel list", path, n, cmd)
		}
		if w.ch, err = strconv.Atoi(chs[2 : len(chs)-1]); err != nil {
			return nil, fmt.Errorf("%s:%d: %q: %v", path, n, cmd, err)
		}
		switch strings.ToUpper(val) {
		case "ON", "1":
			w.value = 1
		case "OFF", "0":
			w.value = 0
		default:
			if w.value, err = strconv.ParseFloat(val, 64); err != nil {
				return nil, fmt.Errorf("%s:%d: %q: %v", path, n, cmd, err)
			}
		}
		out = append(out, w)
	}
	return out, sc.Err()
}

// compareSupply asserts a supply's recording against the supply's own
// transcript of what it was told, which nothing that writes Logb wrote: the
// writes the recording says the supply carried out — each channel write it
// read back, each output the stop switched off — must be the setting
// commands the transcript has, in order, with the same channel and value.
func compareSupply(r *recording, inst string, transcript []scpiWrite, verbose bool) (int, error) {
	if _, ok := r.meta["psu."+inst+".channels"]; !ok {
		return 0, fmt.Errorf("the recording has no supply %q", inst)
	}
	bySeq := map[uint32]ctlRec{}
	for _, c := range r.controls[inst] {
		if c.verdict == vAccepted && c.seq != 0 {
			bySeq[c.seq] = c
		}
	}
	type done struct {
		t float64
		w scpiWrite
	}
	var got []done
	for ch := 1; ch <= channels(r, inst); ch++ {
		for _, p := range r.psu[fmt.Sprintf("%s.ch%d", inst, ch)] {
			switch p.event {
			case psApplied:
				c := bySeq[p.seq]
				_, q, ok := splitChannel(c.channel)
				if !ok {
					return 0, fmt.Errorf("%s.ch%d: a readback under seq %d, which names no channel write", inst, ch, p.seq)
				}
				got = append(got, done{p.t, scpiWrite{what: q, ch: ch, value: c.value}})
			case psOff:
				got = append(got, done{p.t, scpiWrite{what: "out", ch: ch, value: 0}})
			}
		}
	}
	sort.SliceStable(got, func(i, j int) bool { return got[i].t < got[j].t })

	var bad []string
	for i := 0; i < max(len(got), len(transcript)); i++ {
		if len(bad) >= 10 && !verbose {
			bad = append(bad, "(more differences suppressed; -v shows them all)")
			break
		}
		switch {
		case i >= len(got):
			bad = append(bad, fmt.Sprintf("transcript line %d: %v, which the recording does not say was done",
				transcript[i].line, transcript[i]))
		case i >= len(transcript):
			bad = append(bad, fmt.Sprintf("the recording has %v done at %.3fs, and the transcript has no more writes",
				got[i].w, got[i].t))
		case got[i].w.what != transcript[i].what || got[i].w.ch != transcript[i].ch ||
			!(got[i].w.value == transcript[i].value || math.Abs(got[i].w.value-transcript[i].value) < 1e-12):
			bad = append(bad, fmt.Sprintf("write %d: the recording has %v at %.3fs, transcript line %d has %v",
				i+1, got[i].w, got[i].t, transcript[i].line, transcript[i]))
		}
	}
	if len(bad) > 0 {
		return 0, errors.New(strings.Join(bad, "\n"))
	}
	return len(got), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
