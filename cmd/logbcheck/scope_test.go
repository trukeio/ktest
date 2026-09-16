package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"testing"
)

// The scope check is exercised on a recording and a transcript made up for the
// purpose, because a daemon that records faithfully cannot be made to produce
// the recording that ought to fail it. Each case breaks one thing and the check
// must say so; the first breaks nothing and must pass, since a checker that
// fails everything proves as little as one that fails nothing.
//
// The faults §12 names for this slice are here: an acquisition missing, one
// recorded under the previous timebase, and — since §13 settled a sample as f32
// volts, which is what makes a volts/div change not a schema change — a change
// of the timebase that did not open a new segment, which is where §5.2's rule
// still bites.

// codesFor makes an acquisition's samples: a ramp through the code space, which
// exercises both signs under either rule.
func codesFor(n int) []byte {
	c := make([]byte, n)
	for i := range c {
		c[i] = byte((i * 7) % 256)
	}
	return c
}

// voltsOf is the daemon's conversion, done here so that the test's recording
// holds what a faithful daemon would have written.
func voltsOf(codes []byte, vdiv, ofst float64, sign string) []float32 {
	out := make([]float32, len(codes))
	for i, c := range codes {
		v := int(c)
		if c >= 128 {
			if sign == "sub255" {
				v = int(c) - 255
			} else {
				v = int(c) - 256
			}
		}
		out[i] = float32(float64(v)*(vdiv/scopeCodes) - ofst)
	}
	return out
}

func shaOf(codes []byte) string {
	sum := sha256.Sum256(codes)
	return hex.EncodeToString(sum[:])
}

// scopeCase builds a recording and a transcript that agree: two acquisitions on
// one channel, the second after the timebase was changed from 1 ms/div to
// 2 ms/div, which is a change of sample interval and so a new segment (§5.2).
func scopeCase() (*recording, *transcript) {
	const (
		inst = "scope1"
		vdiv = 0.5
		ofst = 0.25
	)
	type spec struct {
		run    uint32
		t      float64
		tdiv   float64
		sara   float64
		points int
		seg    int
	}
	specs := []spec{
		{run: 1, t: 1.0, tdiv: 1e-3, sara: 1e6, points: 14000, seg: 1},
		{run: 2, t: 1.1, tdiv: 1e-3, sara: 1e6, points: 14000, seg: 1},
		{run: 3, t: 2.0, tdiv: 2e-3, sara: 1e6, points: 28000, seg: 2},
		{run: 4, t: 2.1, tdiv: 2e-3, sara: 1e6, points: 28000, seg: 2},
	}

	r := &recording{
		meta: map[string]string{
			"scope.scope1.channels": "1",
			"scope.scope1.sign":     "sub256",
		},
		controls:      map[string][]ctlRec{},
		scopeAcqs:     map[string][]scopeAcq{},
		scopeSamples:  map[string]map[uint32]scopeSamples{"scope1.ch1": {}},
		scopeSettings: map[string][]scopeSetting{},
		scopeSteps:    map[string][]segSteps{},
	}
	tr := &transcript{}

	// The timebase write that separates the two acquisitions, in both accounts.
	r.controls[inst] = []ctlRec{{t: 1.55, seq: 1, request: rqChannelSet, verdict: vAccepted,
		channel: "scope1.timebase.tdiv.set", value: 2e-3}}
	tr.writes = []scopeWrite{{channel: "scope1.timebase.tdiv.set", value: 2e-3, line: 10}}
	// The simulator states what it holds before anyone sets anything, and then
	// after each setting; the recording's readback on connect is the first of
	// those, and the readback after the write is the second.
	tr.holds = []scopeWrite{
		{channel: "scope1.timebase.tdiv.set", value: 1e-3, line: 1},
		{channel: "scope1.timebase.tdiv.set", value: 2e-3, line: 11},
	}
	r.scopeSettings["scope1.timebase"] = []scopeSetting{
		{t: 0.5, seq: 0, event: psConnected,
			applied: map[string]float64{"tdiv": 1e-3}, present: map[string]bool{"tdiv": true}},
		{t: 1.55, seq: 1, event: psRequested,
			applied: map[string]float64{"tdiv": 1e-3}, present: map[string]bool{"tdiv": true}},
		{t: 1.56, seq: 1, event: psApplied,
			applied: map[string]float64{"tdiv": 2e-3}, present: map[string]bool{"tdiv": true}},
	}

	for i, sp := range specs {
		codes := codesFor(sp.points)
		step := 1 / sp.sara
		first := -sp.tdiv * scopeDivisions / 2
		r.scopeAcqs[inst] = append(r.scopeAcqs[inst], scopeAcq{
			t: sp.t, run: sp.run, ch: 1, event: acqRecorded, points: sp.points, sparse: 1,
			vdiv: vdiv, ofst: ofst, tdiv: sp.tdiv, sara: sp.sara, msiz: 14e3, frame: 1,
			sign: "sub256", step: step, first: first, full: true,
		})
		r.scopeSamples["scope1.ch1"][sp.run] = scopeSamples{
			run: sp.run, base: sp.t + first, step: step, volts: voltsOf(codes, vdiv, ofst, "sub256"),
		}
		r.noteStep("scope1.ch1", sp.seg, step)
		tr.acqs = append(tr.acqs, scopeAcq{
			n: i + 1, ch: 1, points: sp.points, sparse: 1, vdiv: vdiv, ofst: ofst,
			tdiv: sp.tdiv, sara: sp.sara, msiz: 14e3, frame: 1, sign: "sub256",
			sha: shaOf(codes), line: 20 + i,
		})
	}
	return r, tr
}

func TestScopeCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(r *recording, tr *transcript)
		want   string // "" for a recording that must pass
	}{
		{"a recording that says what the scope did", func(*recording, *transcript) {}, ""},

		// The faults §12 names for this slice.
		{"an acquisition missing", func(r *recording, tr *transcript) {
			a := r.scopeAcqs["scope1"]
			r.scopeAcqs["scope1"] = append(a[:1:1], a[2:]...)
		}, "acquisition 2"},
		{"an acquisition recorded under the previous timebase", func(r *recording, tr *transcript) {
			a := &r.scopeAcqs["scope1"][2]
			a.tdiv = 1e-3
			a.first = -1e-3 * scopeDivisions / 2
		}, "the recording says tdiv"},
		{"a timebase change that did not open a segment", func(r *recording, tr *transcript) {
			r.scopeSteps["scope1.ch1"] = nil
			r.noteStep("scope1.ch1", 1, 1e-6)
			r.noteStep("scope1.ch1", 1, 5e-7)
		}, "must open a segment"},

		// The conversion, which is the whole of §13's decision working or not.
		{"samples converted with the wrong volts a division", func(r *recording, tr *transcript) {
			s := r.scopeSamples["scope1.ch1"][1]
			s.volts = voltsOf(codesFor(14000), 0.2, 0.25, "sub256")
			r.scopeSamples["scope1.ch1"][1] = s
		}, "not a whole code"},
		{"samples converted with the wrong offset", func(r *recording, tr *transcript) {
			s := r.scopeSamples["scope1.ch1"][1]
			s.volts = voltsOf(codesFor(14000), 0.5, 0.3, "sub256")
			r.scopeSamples["scope1.ch1"][1] = s
		}, "not a whole code"},
		{"samples converted with the other sign rule", func(r *recording, tr *transcript) {
			s := r.scopeSamples["scope1.ch1"][1]
			s.volts = voltsOf(codesFor(14000), 0.5, 0.25, "sub255")
			r.scopeSamples["scope1.ch1"][1] = s
		}, "recover to"},
		{"a sign rule recorded that was not the one in force", func(r *recording, tr *transcript) {
			r.scopeAcqs["scope1"][0].sign = "sub255"
		}, "the recording says the sub255 rule"},
		{"one sample altered", func(r *recording, tr *transcript) {
			r.scopeSamples["scope1.ch1"][1].volts[7777] += float32(0.5 / scopeCodes)
		}, "recover to"},
		{"samples claiming a precision the digitiser has not", func(r *recording, tr *transcript) {
			r.scopeSamples["scope1.ch1"][1].volts[3] += 0.007
		}, "not a whole code"},

		// The axis, computed by the dialect's rule and not the daemon's.
		{"a sample interval that is not 1/sara", func(r *recording, tr *transcript) {
			r.scopeAcqs["scope1"][0].step = 2e-6
		}, "s apart"},
		{"sample 0 at the trigger rather than half a screen before it", func(r *recording, tr *transcript) {
			r.scopeAcqs["scope1"][0].first = 0
		}, "puts sample 0 at"},
		{"a trigger delay taken into the axis without saying so", func(r *recording, tr *transcript) {
			r.scopeAcqs["scope1"][0].first += 1e-3
		}, "puts sample 0 at"},
		{"samples placed at the acquisition's own time", func(r *recording, tr *transcript) {
			s := r.scopeSamples["scope1.ch1"][1]
			s.base = 1.0
			r.scopeSamples["scope1.ch1"][1] = s
		}, "puts sample 0 at"},

		// Full rate claimed and not delivered, which §10.4 makes a per-channel
		// decision and so a thing a recording can be wrong about.
		{"full rate claimed with no samples recorded", func(r *recording, tr *transcript) {
			delete(r.scopeSamples["scope1.ch1"], 1)
		}, "carries no run"},
		{"fewer samples than the scope handed over", func(r *recording, tr *transcript) {
			s := r.scopeSamples["scope1.ch1"][1]
			s.volts = s.volts[:13999]
			r.scopeSamples["scope1.ch1"][1] = s
		}, "holds 13999 samples"},

		// The settings, in both accounts.
		{"a setting the recording claims and the scope was never told", func(r *recording, tr *transcript) {
			r.controls["scope1"] = append(r.controls["scope1"], ctlRec{t: 1.7, seq: 2,
				request: rqChannelSet, verdict: vAccepted, channel: "scope1.ch1.vert.vdiv.set", value: 0.2})
		}, "no more settings"},
		{"a setting the scope was told and the recording does not have", func(r *recording, tr *transcript) {
			tr.writes = append(tr.writes, scopeWrite{channel: "scope1.ch1.vert.vdiv.set", value: 0.2, line: 30})
		}, "does not say was asked for"},
		{"the request recorded as the fact a clamp made it not", func(r *recording, tr *transcript) {
			// The instrument clamps to the steps it has, so the simulator holds
			// something other than what was asked for; a daemon that wrote the
			// request into the applied account passes every other check here.
			tr.holds[1].value = 1e-3
		}, "the recording says the scope held 0.002, and the simulator never held it"},
		{"a readback the daemon never took", func(r *recording, tr *transcript) {
			r.scopeSettings["scope1.timebase"] = r.scopeSettings["scope1.timebase"][:2]
		}, "the recording never says the scope did"},
		{"a setting the recording has and the instrument has not", func(r *recording, tr *transcript) {
			r.scopeSettings["scope1.ch1.vert"] = []scopeSetting{{t: 0.5,
				applied: map[string]float64{"vdiv": 0.5}, present: map[string]bool{"vdiv": true}}}
		}, "has no such setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, tr := scopeCase()
			tc.mutate(r, tr)
			_, err := compareScope(r, "scope1", tr, true)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("the check failed a recording that says what the scope did:\n%v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("the check passed a recording with %s", tc.name)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("the check failed, but not for the planted fault %q:\n%v", tc.want, err)
			}
		})
	}
}

// The recovery of the digitiser's codes from the recorded volts is what makes
// the sha256 comparison mean anything, so it is checked on its own: every code
// must come back, under either rule, for any settings the instrument has.
func TestCodesRoundTrip(t *testing.T) {
	for _, sign := range []string{"sub256", "sub255"} {
		for _, vdiv := range []float64{500e-6, 0.002, 0.5, 1, 10} {
			// The offsets the instrument can reach, which are multiples of the
			// volts a division. f32 carries a code exactly over that range —
			// the worst case, an offset of a hundred divisions, is 1.6e-4 of a
			// code out — and would not over an offset millions of divisions
			// away, which no scope has.
			for _, ofst := range []float64{0, 0.5 * vdiv, -3 * vdiv, 100 * vdiv} {
				codes := codesFor(256)
				if sign == "sub255" {
					// This rule reaches neither end of the byte. 0x80 is -127
					// under it and -128 under the other, and 0xFF is -0, which
					// is 0x00 again: subtracting 255 is not a bijection, and no
					// instrument that sent 0xFF could have its byte recovered
					// from a stored volt. The simulator never emits it, and a
					// scope that did would be evidence for the other reading.
					for i := range codes {
						switch codes[i] {
						case 0x80:
							codes[i] = 0x81
						case 0xFF:
							codes[i] = 0x01
						}
					}
				}
				volts := voltsOf(codes, vdiv, ofst, sign)
				got, err := codesOf(volts, vdiv, ofst, sign)
				if err != nil {
					t.Fatalf("%s vdiv=%g ofst=%g: %v", sign, vdiv, ofst, err)
				}
				for i := range codes {
					if got[i] != codes[i] {
						t.Fatalf("%s vdiv=%g ofst=%g: code %02X came back as %02X (%g V)",
							sign, vdiv, ofst, codes[i], got[i], volts[i])
					}
				}
			}
		}
	}
}

// A volt that is not a whole number of codes is the recording claiming a
// precision the digitiser has not, and must be reported rather than rounded
// away — rounding it away is exactly how a wrong conversion would pass.
func TestCodesRefusesHalfCodes(t *testing.T) {
	if _, err := codesOf([]float32{float32(0.5 / scopeCodes / 2)}, 0.5, 0, "sub256"); err == nil {
		t.Error("half a code was accepted")
	}
	if _, err := codesOf([]float32{float32(200 * 0.5 / scopeCodes)}, 0.5, 0, "sub256"); err == nil {
		t.Error("a code past the 8 bits the digitiser has was accepted")
	}
	if _, err := codesOf([]float32{0}, 0, 0, "sub256"); err == nil {
		t.Error("a zero volts a division was accepted, and it cannot be undone")
	}
}

// near has to tolerate a decimal round trip and nothing more: the transcript
// carries the instrument's numbers as text and the recording as f64.
func TestNear(t *testing.T) {
	if !near(1e-3, 0.001) || !near(0, 0) || !near(1e9, 1e9+1) {
		t.Error("near called two of the same number different")
	}
	if near(1e-3, 2e-3) || near(0, 1e-6) || near(math.NaN(), math.NaN()) {
		t.Error("near called two different numbers the same")
	}
}
