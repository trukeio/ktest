package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/rveen/logb"
)

// panelRec is one save of the rack's panel, as rack.panel records it, with
// the panel in force after it.
type panelRec struct {
	t       float64
	seq     uint32
	verdict uint64
	via     uint64
	rev     uint64
	sha     string
	savedBy string
	by      string
}

// The verdicts a panel save has beside acceptance and vForbidden, as the
// stream's schema describes them.
const (
	pnStale     = 7
	pnUnwritten = 8
)

// readPanel reads one batch of rack.panel.
func readPanel(b *logb.Batch) ([]panelRec, error) {
	idx, err := fieldIndex(b.Schema, "seq", "verdict", "via", "revision", "sha256", "saved_by", "by")
	if err != nil {
		return nil, err
	}
	var out []panelRec
	for i := 0; i < int(b.Count); i++ {
		var first error
		num := func(name string) uint64 {
			v, err := uintField(b, i, idx[name])
			if err != nil && first == nil {
				first = err
			}
			return v
		}
		text := func(name string) string {
			v, err := b.Value(i, idx[name])
			if err != nil && first == nil {
				first = err
			}
			s, _ := v.(string)
			return s
		}
		axis, err := b.Axis(i)
		if err != nil {
			return nil, err
		}
		p := panelRec{
			t: axis.Seconds(b.Schema.AxisExp), seq: uint32(num("seq")), verdict: num("verdict"), via: num("via"),
			rev: num("revision"), sha: text("sha256"), savedBy: text("saved_by"), by: text("by"),
		}
		if first != nil {
			return nil, fmt.Errorf("record %d: %w", i, first)
		}
		out = append(out, p)
	}
	return out, nil
}

// checkPanelSaves holds rack.panel to what a save may do: the stream opens
// with the panel the daemon started with, each save makes the next revision
// and each refusal none, only a name with the operate role saves over the
// network, and the panel the recording ends with is the one its last record
// names. A recording made before the rack could edit its panel has no such
// stream, and nothing here to check.
func checkPanelSaves(r *recording, roles map[string]string, outer func(string, ...any)) {
	if len(r.panel) == 0 {
		return
	}
	report := func(format string, a ...any) { outer("rack.panel: "+format, a...) }

	first := r.panel[0]
	start := uint64(0)
	if _, ok := r.meta["panel.source"]; ok {
		start = 1
	}
	if first.seq != 0 || first.via != 0 || first.verdict != vAccepted {
		report("the stream does not open with the panel the daemon started with, but with seq %d", first.seq)
	} else if first.rev != start {
		report("the daemon started with revision %d, and the recording's panel.source says %d", first.rev, start)
	}

	prev := first
	for _, p := range r.panel[1:] {
		switch p.verdict {
		case vAccepted:
			if p.rev != prev.rev+1 {
				report("seq %d: a save made revision %d of revision %d", p.seq, p.rev, prev.rev)
			}
			if p.savedBy != p.by {
				report("seq %d: a save by %q, recorded as saved by %q", p.seq, p.by, p.savedBy)
			}
		case vForbidden, pnStale, pnUnwritten:
			if p.rev != prev.rev || p.sha != prev.sha || p.savedBy != prev.savedBy {
				report("seq %d: a refused save changed the panel in force, to revision %d", p.seq, p.rev)
			}
		default:
			report("seq %d: verdict %d, which no panel save has", p.seq, p.verdict)
		}
		if p.seq == 0 {
			report("a save at %.3fs with no request number", p.t)
		}
		// Who may save, by the same rules as every other decision.
		if p.via == viaTLS {
			role, known := roles[p.by]
			switch {
			case !known:
				report("seq %d: a save over TLS by %q, a name the recording does not list", p.seq, p.by)
			case p.verdict == vAccepted && role != "operate":
				report("seq %d: a save accepted from %q, whose role is %s", p.seq, p.by, role)
			case p.verdict == vForbidden && role == "operate":
				report("seq %d: a save refused as not permitted to %q, whose role is operate", p.seq, p.by)
			}
		} else if p.verdict == vForbidden {
			report("seq %d: a save refused as not permitted, through a listener that has no roles", p.seq)
		}
		prev = p
	}

	last := r.panel[len(r.panel)-1]
	switch {
	case r.panelAttach == nil && last.rev != 0:
		report("revision %d is in force at the end, and the recording carries no panel.json", last.rev)
	case r.panelAttach != nil && last.rev == 0:
		report("the recording carries a panel.json, and its last record says there is no panel")
	case r.panelAttach != nil:
		sum := sha256.Sum256(r.panelAttach)
		if got := hex.EncodeToString(sum[:]); got != last.sha {
			report("the panel.json the recording ends with has sha256 %.12s…, and revision %d is recorded as %.12s…",
				got, last.rev, last.sha)
		}
	}
}
