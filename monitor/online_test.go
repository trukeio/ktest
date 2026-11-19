package monitor

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

const aliveContract = `{"contract": "c", "schema": 1,
  "inputs": {"Alive": "DutStatus", "Vx": "psu1.ch1.meas.v"},
  "monitors": {"alive": {"ok": "age(Alive) < 30ms and delta(Alive.counter, 16) == 1"},
               "vx": {"ok": "Vx > 9V"}}}`

func online(t *testing.T, js string, window time.Duration) *Online {
	t.Helper()
	o, err := NewOnline(mustLoad(t, js), window, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// t0 is an arbitrary Unix time, so that nothing depends on zero.
const t0 = int64(1_763_000_000) * 1_000_000_000

func alive(at, t int64, counter int) Item {
	return Item{Kind: ItemRecord, At: t0 + at, T: t0 + t, Stream: status.Name, Rec: rec(status, float64(counter))}
}

func vx(at, t int64, v float64) Item {
	return Item{Kind: ItemRecord, At: t0 + at, T: t0 + t, Stream: meas.Name, Rec: rec(meas, v, 0.1)}
}

func tick(at int64) Item { return Item{Kind: ItemTick, At: t0 + at} }

// run steps every item through o, collecting what it produced.
func runOnline(o *Online, items []Item) (tr []Transition, wms []int64, gaps []*Gap) {
	for _, it := range items {
		res := o.Step(it)
		tr = append(tr, res.Transitions...)
		if res.Watermark != 0 {
			wms = append(wms, res.Watermark)
		}
		if res.Gap != nil {
			gaps = append(gaps, res.Gap)
		}
	}
	return
}

func rel(tr []Transition) []Transition {
	out := make([]Transition, len(tr))
	for i, x := range tr {
		x.T -= t0
		out[i] = x
	}
	return out
}

// Records arriving out of order within the window are evaluated in time
// order, and give exactly what an offline replay of the same records gives.
func TestOnlineReorders(t *testing.T) {
	o := online(t, aliveContract, 5*time.Millisecond)
	var items []Item
	for i := 0; i < 10; i++ {
		ft := int64(i) * 10 * ms
		// The supply's reading for the same instant arrives 2 ms after the
		// frame, but is stamped 1 ms before it: out of order on arrival.
		items = append(items, alive(ft+100_000, ft, (3+i)%16), vx(ft+2*ms, ft-ms, 12), tick(ft+5*ms))
	}
	items = append(items, tick(200*ms))
	tr, wms, gaps := runOnline(o, items)
	if len(gaps) != 0 {
		t.Fatalf("gaps %+v", gaps[0])
	}
	// vx is sampled: its first reading has no period yet, so it is ok from
	// the second, at 9 ms, and stale 3 periods and a nanosecond after its
	// last, at 89 ms.
	expectTr(t, rel(tr), "9000000 vx unknown>ok", "20000000 alive unknown>ok",
		"119000001 vx ok>unknown", "120000000 alive ok>dev")

	// The same records, sorted, through the offline evaluator.
	e := evaluator(t, mustLoad(t, aliveContract))
	var steps []step
	for i := 0; i < 10; i++ {
		ft := int64(i) * 10 * ms
		steps = append(steps, step{t: ft - ms, s: meas, vals: []any{12.0, 0.1}},
			step{t: ft, s: status, vals: []any{float64((3 + i) % 16)}})
	}
	steps = append(steps, step{t: 195 * ms, adv: true})
	expectTr(t, play(t, e, steps), brief(rel(tr))...)

	for i := 1; i < len(wms); i++ {
		if wms[i] <= wms[i-1] {
			t.Fatalf("watermark went from %d to %d", wms[i-1], wms[i])
		}
	}
	if last := wms[len(wms)-1] - t0; last != 195*ms {
		t.Errorf("last watermark %v, want 195ms: the tick at 200ms less the window", time.Duration(last))
	}
}

// The watermark only ever says what has been evaluated: it stands at the
// daemon's clock less the window, and every transition up to it has been
// handed over before it.
func TestOnlineWatermarkMonotonic(t *testing.T) {
	o := online(t, aliveContract, 2*time.Millisecond)
	r := rand.New(rand.NewPCG(1, 2))
	var at int64
	var last int64
	var lastTr int64
	for i := 0; i < 5000; i++ {
		at += int64(r.IntN(3000)) * 1000 // 0 to 3 ms
		var it Item
		switch r.IntN(3) {
		case 0:
			it = tick(at)
		case 1:
			it = alive(at, at-int64(r.IntN(1500))*1000, i%16)
		default:
			it = vx(at, at-int64(r.IntN(1500))*1000, 8+float64(r.IntN(3)))
		}
		res := o.Step(it)
		if res.Gap != nil {
			t.Fatalf("item %d: gap %+v", i, *res.Gap)
		}
		for _, x := range res.Transitions {
			if x.T < lastTr {
				t.Fatalf("transition at %d after one at %d", x.T, lastTr)
			}
			lastTr = x.T
		}
		if w := res.Watermark; w != 0 {
			if w <= last {
				t.Fatalf("watermark %d after %d", w, last)
			}
			if w > t0+at-2*ms {
				t.Fatalf("watermark %d ahead of the clock less the window, %d", w, t0+at-2*ms)
			}
			if lastTr > w {
				t.Fatalf("a transition at %d was handed over with a watermark %d before it", lastTr, w)
			}
			last = w
		}
	}
	if last == 0 {
		t.Fatal("no watermark")
	}
}

// A record behind the watermark is a gap: every monitor unknown from the
// watermark, the evaluator starting again, and the monitors known again
// from the records after it.
func TestOnlineLate(t *testing.T) {
	o := online(t, aliveContract, 2*time.Millisecond)
	var items []Item
	for i := 0; i < 5; i++ {
		ft := int64(i) * 10 * ms
		items = append(items, alive(ft, ft, i), vx(ft, ft, 12))
	}
	items = append(items, tick(45*ms))
	items = append(items, vx(46*ms, 30*ms, 12)) // 13 ms behind the watermark at 43 ms
	for i := 5; i < 10; i++ {
		ft := int64(i) * 10 * ms
		items = append(items, alive(ft, ft, i), vx(ft, ft, 12))
	}
	items = append(items, tick(200*ms))
	tr, _, gaps := runOnline(o, items)
	if len(gaps) != 1 || gaps[0].Lost != 1 || gaps[0].T-t0 != 43*ms || !strings.HasPrefix(gaps[0].Reason, "late: psu1.ch1.meas") {
		t.Fatalf("gaps %+v", gaps)
	}
	// alive needs two frames after the gap to have a period and a delta
	// again: ok at 70 ms. vx is a sampled input too, and its first reading
	// after the gap is not used until the second, at 60 ms.
	expectTr(t, rel(tr), "10000000 vx unknown>ok", "20000000 alive unknown>ok",
		"43000000 alive ok>unknown", "43000000 vx ok>unknown",
		"60000000 vx unknown>ok", "70000000 alive unknown>ok", "120000000 alive ok>dev",
		"120000001 vx ok>unknown")
}

// A full queue loses records, and the tap says so once there is room: a gap
// with how many, and from when to when. A lost tick is not a loss.
func TestTapOverflow(t *testing.T) {
	o := online(t, aliveContract, 2*time.Millisecond)
	tap, in := NewTap(o, 4)
	base := time.Unix(0, t0)
	at := func(d int64) time.Time { return base.Add(time.Duration(d)) }
	r := rec(status, 1.0)
	for i := 0; i < 4; i++ {
		tap.RecordAt(at(int64(i)*ms), status.Name, at(int64(i)*ms), r)
	}
	tap.Tick(at(5 * ms))                                  // does not fit, and is not a loss
	tap.RecordAt(at(6*ms), status.Name, at(6*ms), r)      // lost
	tap.RecordAt(at(7*ms), "something.else", at(7*ms), r) // not wanted: not a loss either
	tap.RecordAt(at(8*ms), status.Name, at(8*ms), r)      // lost
	for i := 0; i < 4; i++ {
		<-in
	}
	tap.RecordAt(at(9*ms), status.Name, at(9*ms), r)
	g := <-in
	if g.Kind != itemGap || g.gap.Lost != 2 || g.gap.From != t0+6*ms || g.gap.To != t0+8*ms || g.gap.Reason != "overflow" {
		t.Fatalf("got %+v, want a gap of 2 from 6 to 8 ms", g)
	}
	if next := <-in; next.Kind != ItemRecord || next.T != t0+9*ms {
		t.Fatalf("after the gap %+v, want the record at 9 ms", next)
	}
}

// A stalled evaluator stops the watermark, and its queue fills into a gap
// instead of holding up the loop.
func TestStalledEvaluator(t *testing.T) {
	o := online(t, aliveContract, 2*time.Millisecond)
	tap, in := NewTap(o, 16)
	out := make(chan Output) // nobody reads it: the evaluator stalls on its first output
	go o.Run(in, out)
	base := time.Unix(0, t0)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		d := time.Duration(i) * time.Millisecond
		tap.RecordAt(base.Add(d), status.Name, base.Add(d), rec(status, float64(i%16)))
		tap.Tick(base.Add(d))
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the tap took %v: it blocked", el)
	}
	if tap.lost == 0 {
		t.Fatal("no records lost behind a stalled evaluator")
	}
	// Once it is read again, the evaluator works through what was queued
	// before the loss, and then reports the gap.
	timeout := time.After(time.Second)
	for {
		select {
		case res := <-out:
			if res.Gap != nil {
				if res.Gap.Reason != "overflow" || res.Gap.Lost == 0 {
					t.Fatalf("gap %+v", *res.Gap)
				}
				goto done
			}
		case <-timeout:
			t.Fatal("no gap after the stall ended")
		}
	}
done:
	tap.Close()
	go func() {
		for range out {
		}
	}()
}
