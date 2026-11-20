package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/trukeio/logb"
)

// The judgement check (-judgements) replays a lab rack's verdicts on a DUT
// from its own recording. Every answer the DUT's monitors gave a keyword —
// which deviated in a window, whether all were ok at a watermark — is in
// <peer>.judgement, with its window on the DUT rack's clock. The monitors'
// states are in the mirrored <peer>.mon.* streams, with the DUT rack's times
// in peer_t. Each judgement is recomputed from those alone and must come out
// the same: the DUT rack's watermark had passed the window's end before the
// keyword judged, so every state it could have seen is in the recording.
//
// A class (Classify Recovery) follows from its judgements by fixed rules, so
// judgements that replay are verdicts that replay.
//
// A master driving several DUT racks records each one's judgements and
// mirrored monitors under its name, and each judgement is replayed from its
// own rack's states.

type judged struct {
	peer     string // the DUT rack it was about, the streams' prefix
	t        float64
	kind     uint64
	from, to int64
	monitors []string
	result   string
}

type mstamp struct {
	t     int64
	state string
}

// readJudged reads every judgement and every mirrored monitor state.
func readJudged(path string) ([]judged, map[string][]mstamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	r, err := logb.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	var js []judged
	hist := map[string][]mstamp{}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		name := b.Schema.Name
		switch {
		case strings.HasSuffix(name, ".judgement"):
			idx, err := fieldIndex(b.Schema, "kind", "from", "to", "monitors", "result")
			if err != nil {
				return nil, nil, err
			}
			for i := 0; i < int(b.Count); i++ {
				ax, err := b.Axis(i)
				if err != nil {
					return nil, nil, err
				}
				val := func(f string) any { v, _ := b.Value(i, idx[f]); return v }
				kind, _ := uintField(b, i, idx["kind"])
				from, _ := val("from").(int64)
				to, _ := val("to").(int64)
				ms, _ := val("monitors").(string)
				res, _ := val("result").(string)
				js = append(js, judged{peer: strings.TrimSuffix(name, ".judgement"), t: ax.Seconds(b.Schema.AxisExp),
					kind: kind, from: from, to: to,
					monitors: strings.Split(ms, ","), result: res})
			}
		case strings.Contains(name, ".mon.") && b.Schema.Meta["mirror.of"] != "":
			m := strings.TrimPrefix(b.Schema.Meta["mirror.of"], "mon.")
			if m == "watermark" || m == "gap" {
				continue
			}
			m = histKey(strings.TrimSuffix(name, "."+b.Schema.Meta["mirror.of"]), m)
			idx, err := fieldIndex(b.Schema, "peer_t", "state")
			if err != nil {
				return nil, nil, err
			}
			for i := 0; i < int(b.Count); i++ {
				pt, _ := b.Value(i, idx["peer_t"])
				st, _ := b.Value(i, idx["state"])
				t, _ := pt.(int64)
				s, _ := st.(string)
				hist[m] = append(hist[m], mstamp{t, s})
			}
		}
	}
	for m := range hist {
		slices.SortStableFunc(hist[m], func(a, b mstamp) int { return compareInt(a.t, b.t) })
	}
	return js, hist, nil
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// stateAt is a monitor's state at t: its last state at or before t, unknown
// before any, as the daemon's view has it.
func stateAt(h []mstamp, t int64) string {
	s := "unknown"
	for _, x := range h {
		if x.t > t {
			break
		}
		s = x.state
	}
	return s
}

// histKey is where a rack's monitor's states are kept.
func histKey(peer, monitor string) string {
	if peer == "" {
		return monitor
	}
	return peer + "\x00" + monitor
}

// replay recomputes one judgement, from its own rack's states.
func replay(j judged, all map[string][]mstamp) string {
	hist := map[string][]mstamp{}
	for _, m := range j.monitors {
		hist[m] = all[histKey(j.peer, m)]
	}
	if j.kind == 2 {
		for _, m := range j.monitors {
			if stateAt(hist[m], j.to) != "ok" {
				return "not ok"
			}
		}
		return "ok"
	}
	bad := func(s string) bool { return s == "dev" || s == "unknown" }
	var parts []string
	for _, m := range j.monitors {
		s := stateAt(hist[m], j.from)
		if !bad(s) {
			s = ""
			for _, x := range hist[m] {
				if x.t > j.from && x.t <= j.to && bad(x.state) {
					s = x.state
					break
				}
			}
		}
		if s != "" {
			parts = append(parts, m+"="+s)
		}
	}
	return strings.Join(parts, ",")
}

// compareJudged replays every judgement in a recording.
func compareJudged(path string, verbose bool) (int, error) {
	js, hist, err := readJudged(path)
	if err != nil {
		return 0, err
	}
	if len(js) == 0 {
		return 0, errors.New("the recording has no judgements: it was not made by a lab rack running DUT keywords")
	}
	var bad []string
	for _, j := range js {
		got := replay(j, hist)
		// An all-ok judgement that timed out was read at the last watermark
		// before the deadline; a later one may have shown it ok, so only "ok"
		// is held to the states, and "not ok" to their not being all ok then.
		if got != j.result && (len(bad) < 10 || verbose) {
			what := "which monitors deviated"
			if j.kind == 2 {
				what = "whether all were ok"
			}
			bad = append(bad, fmt.Sprintf("at %.3fs, %s, %s (%s): the keyword was told %q, and the mirrored states say %q",
				j.t, j.peer, what, strings.Join(j.monitors, ","), j.result, got))
		}
	}
	if len(bad) > 0 {
		return len(js), errors.New(strings.Join(bad, "\n"))
	}
	return len(js), nil
}
