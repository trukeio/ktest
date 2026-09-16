package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rveen/logb"
)

// The scope half of the checker.
//
// The reference is the simulator's transcript, which nothing that writes Logb
// wrote, and that is the whole point: comparing a recording against another
// recording made by the same library lets a fault in the writer or the reader
// cancel out. The transcript is an account in a foreign form — SCPI commands,
// the settings the instrument holds after each, and a sha256 of the bytes it
// handed over — so it can falsify the recording rather than agree with it.
//
// What is asserted:
//
//   - Every acquisition the recording carries is the transcript's, at the same
//     position, with the same settings, and its samples hash to what the
//     simulator says it handed over. The hash is taken over the codes recovered
//     from the recorded volts by the recording's own stated conversion, so a
//     daemon that converted wrongly, or that stated a conversion it did not
//     use, produces different bytes and says so.
//   - The axis of every acquisition is what the dialect's rule gives for the
//     transcript's numbers — step 1/sara, sample 0 at -(tdiv x 14 / 2), and the
//     trigger delay only where the recording says it was taken — and not
//     whatever the daemon computed.
//   - Every setting the recording says was applied is the transcript's, in
//     order, by channel and value; and what the recording says the scope holds
//     after each is what the simulator says it holds, which is where clamping
//     to the instrument's 1-2-5 steps shows up.
//   - A change of sample interval opened a new segment (§5.2), because a
//     recording that changed axis_step inside one decodes every sample of that
//     frame to a wrong position and says nothing.

// scopeAcq is one acquisition, from a transcript or from a recording.
type scopeAcq struct {
	n      int // the simulator's own count, 0 for a recording's
	ch     int
	points int
	sparse int

	vdiv, ofst             float64
	tdiv, sara, trdl, msiz float64
	sign                   string
	trdelay                bool
	frame                  int
	ftim                   string
	sha                    string

	// From the recording only.
	t     float64 // where the acquisition sits on the recording's timeline
	run   uint32
	event uint64
	step  float64
	first float64
	full  bool
	line  int // the transcript's line, 0 for a recording's
}

func (a scopeAcq) String() string {
	return fmt.Sprintf("ch%d, %d points at %g Sa/s, %g V/div, %g s/div", a.ch, a.points, a.sara, a.vdiv, a.tdiv)
}

// What an acquisition record says happened, as <scope>.acq's schema describes it.
const (
	acqRecorded = 1
	acqDropped  = 2
	acqMissed   = 3
	acqFailed   = 4
)

// scopeSamples is one acquisition's samples, as the recording carries them.
type scopeSamples struct {
	run   uint32
	base  float64 // where sample 0 lies on the recording's timeline
	step  float64
	volts []float32
}

// scopeSetting is one setting record: what was asked for and what the scope
// reported holding after it.
type scopeSetting struct {
	t       float64
	seq     uint32
	event   uint64
	applied map[string]float64
	present map[string]bool
}

// scopeStream matches a scope's samples stream, scope1.ch1, and not its
// envelope, scope1.ch1.env, nor its settings, scope1.ch1.vert.
var scopeStream = regexp.MustCompile(`^[A-Za-z0-9_-]+\.ch[0-9]+$`)

// readScopeAcq reads the scope's account of its acquisitions.
func readScopeAcq(b *logb.Batch) ([]scopeAcq, error) {
	names := []string{"run", "channel", "event", "sign", "trigger_delay", "full_rate",
		"points", "vdiv", "ofst", "tdiv", "sara", "trdl", "msiz", "step", "first", "frame"}
	idx, err := fieldIndex(b.Schema, names...)
	if err != nil {
		return nil, err
	}
	var out []scopeAcq
	for i := 0; i < int(b.Count); i++ {
		axis, err := b.Axis(i)
		if err != nil {
			return nil, err
		}
		a := scopeAcq{t: axis.Seconds(b.Schema.AxisExp)}
		u := func(n string) (uint64, error) { return uintField(b, i, idx[n]) }
		f := func(n string) (float64, error) { return floatField(b, i, idx[n]) }
		run, err := u("run")
		if err != nil {
			return nil, err
		}
		a.run = uint32(run)
		if a.event, err = u("event"); err != nil {
			return nil, err
		}
		ch, err := u("channel")
		if err != nil {
			return nil, err
		}
		a.ch = int(ch)
		sign, err := u("sign")
		if err != nil {
			return nil, err
		}
		a.sign = map[uint64]string{0: "sub256", 1: "sub255"}[sign]
		points, err := u("points")
		if err != nil {
			return nil, err
		}
		a.points, a.sparse = int(points), 1
		frame, err := u("frame")
		if err != nil {
			return nil, err
		}
		a.frame = int(frame)
		td, err := b.Value(i, idx["trigger_delay"])
		if err != nil {
			return nil, err
		}
		a.trdelay = td == true
		full, err := b.Value(i, idx["full_rate"])
		if err != nil {
			return nil, err
		}
		a.full = full == true
		for _, p := range []struct {
			name string
			to   *float64
		}{
			{"vdiv", &a.vdiv}, {"ofst", &a.ofst}, {"tdiv", &a.tdiv}, {"sara", &a.sara},
			{"trdl", &a.trdl}, {"msiz", &a.msiz}, {"step", &a.step}, {"first", &a.first},
		} {
			if *p.to, err = f(p.name); err != nil {
				return nil, err
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// readScopeSamples reads one acquisition's samples. A waveform is one batch per
// acquisition, on an implicit uniform axis: no axis bytes in the record, and
// the run says which acquisition it is.
func readScopeSamples(b *logb.Batch) (scopeSamples, error) {
	idx, err := fieldIndex(b.Schema, "v")
	if err != nil {
		return scopeSamples{}, err
	}
	if b.Schema.AxisMode != logb.AxisImplicit {
		return scopeSamples{}, fmt.Errorf("stream %q is not on a uniform axis; §5.4 says a waveform is", b.Schema.Name)
	}
	s := scopeSamples{run: b.RunID, step: float64(int64(b.Schema.AxisStep)) * math.Pow10(int(b.Schema.AxisExp))}
	axis, err := b.Axis(0)
	if err != nil {
		return s, err
	}
	s.base = axis.Seconds(b.Schema.AxisExp)
	s.volts = make([]float32, b.Count)
	for i := range s.volts {
		v, err := floatField(b, i, idx["v"])
		if err != nil {
			return s, err
		}
		s.volts[i] = float32(v)
	}
	return s, nil
}

// readScopeSetting reads a settings stream, <scope>.ch<N>.vert or
// <scope>.timebase: what the scope reported holding, and under which request.
func readScopeSetting(b *logb.Batch) ([]scopeSetting, error) {
	idx, err := fieldIndex(b.Schema, "seq", "event")
	if err != nil {
		return nil, err
	}
	// The applied account of every quantity the stream carries, whichever they
	// are, so that a stream growing a setting does not need this changed.
	applied := map[string]int{}
	for i := range b.Schema.Fields {
		if q, ok := strings.CutSuffix(b.Schema.Fields[i].Name, ".applied"); ok {
			applied[q] = i
		}
	}
	var out []scopeSetting
	for i := 0; i < int(b.Count); i++ {
		axis, err := b.Axis(i)
		if err != nil {
			return nil, err
		}
		s := scopeSetting{t: axis.Seconds(b.Schema.AxisExp),
			applied: map[string]float64{}, present: map[string]bool{}}
		seq, err := uintField(b, i, idx["seq"])
		if err != nil {
			return nil, err
		}
		s.seq = uint32(seq)
		if s.event, err = uintField(b, i, idx["event"]); err != nil {
			return nil, err
		}
		for q, f := range applied {
			// Absent while the scope is not answering: a guard, not an error.
			v, err := b.Value(i, f)
			if err != nil || v == nil {
				continue
			}
			// A false is a value, not an absence: recording it only when true
			// would lose every "the scope reports this channel off", which is
			// exactly the readback a check of what the instrument held needs.
			switch x := v.(type) {
			case float64:
				s.applied[q], s.present[q] = x, true
			case bool:
				s.applied[q], s.present[q] = 0, true
				if x {
					s.applied[q] = 1
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func floatField(b *logb.Batch, i, f int) (float64, error) {
	v, err := b.Value(i, f)
	if err != nil {
		return 0, err
	}
	x, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("field %d of record %d is %T, not a number", f, i, v)
	}
	return x, nil
}

// scopeWrite is one setting command, from a transcript or from a recording.
type scopeWrite struct {
	channel string // psu-style: scope1.ch1.vert.vdiv.set
	value   float64
	line    int
}

func (w scopeWrite) String() string { return fmt.Sprintf("%s = %g", w.channel, w.value) }

// A transcript is the simulator's own account of a run.
type transcript struct {
	acqs   []scopeAcq
	writes []scopeWrite
	// holds is what the simulator says it holds after each setting command, in
	// the order the commands came, keyed as the writes are.
	holds []scopeWrite
}

// readScopeTranscript reads a scopesim transcript: "cmd <command>" for every
// command received, "hold <NAME>=<value>" for what it holds after a setting,
// and "acq <key>=<value>..." for each acquisition handed over.
func readScopeTranscript(path, inst string) (*transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr := &transcript{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 0; sc.Scan(); {
		n++
		_, rest, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		kind, body, _ := strings.Cut(strings.TrimSpace(rest), " ")
		switch kind {
		case "cmd":
			w, ok, err := parseScopeCommand(inst, body)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, n, err)
			}
			if ok {
				w.line = n
				tr.writes = append(tr.writes, w)
			}
		case "hold":
			name, value, ok := strings.Cut(body, "=")
			if !ok {
				return nil, fmt.Errorf("%s:%d: %q is not NAME=value", path, n, body)
			}
			w, ok, err := parseScopeCommand(inst, name+" "+value)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, n, err)
			}
			if ok {
				w.line = n
				tr.holds = append(tr.holds, w)
			}
		case "acq":
			a, err := parseScopeAcq(body)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, n, err)
			}
			a.line = n
			tr.acqs = append(tr.acqs, a)
		}
	}
	return tr, sc.Err()
}

// parseScopeCommand turns one SCPI command into the channel it sets, or reports
// that it sets nothing. Queries and commands that set nothing are passed over.
func parseScopeCommand(inst, cmd string) (scopeWrite, bool, error) {
	head, args, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	head = strings.ToUpper(strings.TrimSpace(head))
	args = strings.TrimSpace(args)
	if strings.HasSuffix(head, "?") || args == "" {
		return scopeWrite{}, false, nil
	}
	ch := 0
	if c, rest, isChan := strings.Cut(head, ":"); isChan && strings.HasPrefix(c, "C") {
		n, err := strconv.Atoi(c[1:])
		if err != nil {
			return scopeWrite{}, false, nil
		}
		ch, head = n, rest
	}
	var part, what string
	switch head {
	case "VDIV":
		part, what = fmt.Sprintf("ch%d.vert", ch), "vdiv"
	case "OFST":
		part, what = fmt.Sprintf("ch%d.vert", ch), "ofst"
	case "TRA", "TRACE":
		part, what = fmt.Sprintf("ch%d.vert", ch), "on"
	case "TDIV":
		part, what = "timebase", "tdiv"
	case "TRDL":
		part, what = "timebase", "trdl"
	case "MSIZ":
		part, what = "timebase", "msiz"
	default:
		return scopeWrite{}, false, nil
	}
	if (head == "VDIV" || head == "OFST" || head == "TRA" || head == "TRACE") && ch < 1 {
		return scopeWrite{}, false, fmt.Errorf("%q names no channel", cmd)
	}
	w := scopeWrite{channel: fmt.Sprintf("%s.%s.%s.set", inst, part, what)}
	switch strings.ToUpper(args) {
	case "ON", "1":
		w.value = 1
	case "OFF", "0":
		w.value = 0
	default:
		v, err := parseScopeNumber(args)
		if err != nil {
			return scopeWrite{}, false, fmt.Errorf("%q: %w", cmd, err)
		}
		w.value = v
	}
	return w, true, nil
}

// parseScopeNumber reads a value as the instrument writes one: a number with a
// unit, or a memory depth as 7K or 14M.
func parseScopeNumber(s string) (float64, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult, s = 1e3, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		mult, s = 1e6, s[:len(s)-1]
	case strings.HasSuffix(s, "V"), strings.HasSuffix(s, "S"):
		s = s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	return v * mult, err
}

// parseScopeAcq reads one acq line: key=value pairs, which is a form nothing in
// Logb produced and nothing in Logb can read back.
func parseScopeAcq(body string) (scopeAcq, error) {
	a := scopeAcq{sparse: 1}
	for _, kv := range strings.Fields(body) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return a, fmt.Errorf("%q is not key=value", kv)
		}
		var err error
		num := func(to *float64) { *to, err = parseScopeNumber(v) }
		ival := func(to *int) { *to, err = strconv.Atoi(v) }
		switch k {
		case "n":
			ival(&a.n)
		case "ch":
			ival(&a.ch)
		case "points":
			ival(&a.points)
		case "sparse":
			ival(&a.sparse)
		case "frame":
			ival(&a.frame)
		case "vdiv":
			num(&a.vdiv)
		case "ofst":
			num(&a.ofst)
		case "tdiv":
			num(&a.tdiv)
		case "sara":
			num(&a.sara)
		case "trdl":
			num(&a.trdl)
		case "msiz":
			num(&a.msiz)
		case "sign":
			a.sign = v
		case "trdelay":
			a.trdelay, err = strconv.ParseBool(v)
		case "ftim":
			a.ftim = v
		case "sha256":
			a.sha = v
		default:
			return a, fmt.Errorf("no such key %q", k)
		}
		if err != nil {
			return a, fmt.Errorf("%q: %w", kv, err)
		}
	}
	if a.ch < 1 || a.points < 1 || a.sha == "" {
		return a, fmt.Errorf("an acquisition needs at least ch, points and sha256: %q", body)
	}
	return a, nil
}

// codesOf recovers the digitiser's codes from the recorded volts, by the
// conversion the recording itself states: volts = code x (vdiv / 25) - ofst.
//
// This is the step that makes the sha256 comparison mean something. §13 stores a
// sample as f32 volts, so what is in the file is not the bytes the instrument
// sent; running the recording's own stated arithmetic backwards is what turns it
// back into them. A daemon that converted with a different vdiv, a different
// offset or the other sign rule, or that recorded a conversion it did not use,
// ends up with different codes here and the hash says so.
func codesOf(volts []float32, vdiv, ofst float64, sign string) ([]byte, error) {
	scale := vdiv / 25
	if !(scale > 0) {
		return nil, fmt.Errorf("vdiv %g: the conversion cannot be undone", vdiv)
	}
	out := make([]byte, len(volts))
	for i, v := range volts {
		x := (float64(v) + ofst) / scale
		n := math.Round(x)
		// A recorded volt that is not a whole code is the recording claiming a
		// precision the digitiser does not have, which is a finding rather than
		// something to round away.
		if math.Abs(x-n) > 0.01 {
			return nil, fmt.Errorf("sample %d is %g V, which is %g codes at %g V/div and offset %g: not a whole code",
				i, v, x, vdiv, ofst)
		}
		switch {
		case n >= 0 && n <= 127:
			out[i] = byte(n)
		case sign == "sub255" && n >= -127 && n < 0:
			out[i] = byte(n + 255)
		case sign != "sub255" && n >= -128 && n < 0:
			out[i] = byte(n + 256)
		default:
			return nil, fmt.Errorf("sample %d is code %g, which is not an 8-bit code under the %s rule", i, n, sign)
		}
	}
	return out, nil
}

// Divisions and Codes are the dialect's own arithmetic, repeated here rather
// than imported, so that the checker computes the axis from the instrument's
// documented rule and not from the same code the daemon used. A shared constant
// would make an agreement between two callers of one function look like an
// agreement between the daemon and the instrument.
const (
	scopeDivisions = 14
	scopeCodes     = 25
)

// compareScope asserts a scope's recording against the simulator's transcript.
func compareScope(r *recording, inst string, tr *transcript, verbose bool) (int, error) {
	if _, ok := r.meta["scope."+inst+".channels"]; !ok {
		return 0, fmt.Errorf("the recording has no scope %q", inst)
	}
	var bad []string
	report := func(format string, a ...any) {
		if len(bad) < 10 || verbose {
			bad = append(bad, fmt.Sprintf(format, a...))
		} else if len(bad) == 10 {
			bad = append(bad, "(more differences suppressed; -v shows them all)")
		}
	}

	got := r.scopeAcqs[inst]
	sort.SliceStable(got, func(i, j int) bool { return got[i].t < got[j].t })
	// The recording's acquisitions must be the transcript's, in order and from
	// its start. A transcript may run on past the last one the daemon recorded,
	// because the simulator answers a readout the daemon was still in the
	// middle of when it stopped; it may never be short, and it may never differ
	// in the middle.
	//
	// The one shortfall that is legitimate is at the very end: the simulator
	// writes its acq line as it builds the block, so a daemon that stopped
	// mid-transfer leaves the transcript one trigger ahead — at most one
	// acquisition a channel. Anything more, or anything missing before the end,
	// is a hole.
	channels, _ := strconv.Atoi(r.meta["scope."+inst+".channels"])
	if len(got) > len(tr.acqs) {
		report("the recording has %d acquisitions and the transcript only handed over %d", len(got), len(tr.acqs))
	} else if short := len(tr.acqs) - len(got); short > max(1, channels) {
		report("the transcript handed over %d acquisitions and the recording has %d; at most %d, the trigger in "+
			"flight when the daemon stopped, may be missing", len(tr.acqs), len(got), max(1, channels))
	}
	checked := 0
	for i := range got {
		if i >= len(tr.acqs) {
			break
		}
		g, want := got[i], tr.acqs[i]
		where := fmt.Sprintf("acquisition %d (run %d at %.3fs, transcript line %d)", i+1, g.run, g.t, want.line)
		if g.ch != want.ch {
			report("%s: the recording says channel %d, the transcript says %d", where, g.ch, want.ch)
			continue
		}
		for _, c := range []struct {
			name      string
			got, want float64
		}{
			{"points", float64(g.points), float64(want.points)},
			{"vdiv", g.vdiv, want.vdiv},
			{"ofst", g.ofst, want.ofst},
			{"tdiv", g.tdiv, want.tdiv},
			{"sara", g.sara, want.sara},
			{"trdl", g.trdl, want.trdl},
			{"msiz", g.msiz, want.msiz},
			{"frame", float64(g.frame), float64(want.frame)},
		} {
			if !near(c.got, c.want) {
				report("%s: the recording says %s %g, the transcript says %g", where, c.name, c.got, c.want)
			}
		}
		if g.sign != want.sign {
			report("%s: the recording says the %s rule, the transcript says %s", where, g.sign, want.sign)
		}
		if g.trdelay != want.trdelay {
			report("%s: the recording says trigger_delay %t, the transcript says %t", where, g.trdelay, want.trdelay)
		}

		// The axis, by the dialect's rule on the transcript's numbers.
		if want.sara > 0 {
			if step := 1 / want.sara; !near(g.step, step) {
				report("%s: the recording puts the samples %g s apart; %g Sa/s is %g s",
					where, g.step, want.sara, step)
			}
		}
		first := -want.tdiv * scopeDivisions / 2
		if g.trdelay {
			first += want.trdl
		}
		if !near(g.first, first) {
			report("%s: the recording puts sample 0 at %g s from the trigger; %g s/div over %d divisions is %g s",
				where, g.first, want.tdiv, scopeDivisions, first)
		}

		if g.event != acqRecorded && g.event != acqDropped {
			// Missed or failed: there is nothing to hash, and the recording
			// saying so is exactly what §10.5 asks of it.
			continue
		}
		checked++
		if !g.full {
			continue // the envelope only; there are no samples to hash
		}
		name := fmt.Sprintf("%s.ch%d", inst, g.ch)
		s, ok := r.scopeSamples[name][g.run]
		if !ok {
			report("%s: %s says it was recorded at full rate, and %s carries no run %d",
				where, name, name, g.run)
			continue
		}
		if len(s.volts) != want.points {
			report("%s: %s holds %d samples, the transcript handed over %d", where, name, len(s.volts), want.points)
			continue
		}
		if !near(s.step, g.step) {
			report("%s: %s is written with axis_step %g s and the acquisition says %g s", where, name, s.step, g.step)
		}
		if !near(s.base, g.t+g.first) {
			report("%s: %s puts sample 0 at %.9fs; the acquisition is at %.9fs and sample 0 is %g s before it",
				where, name, s.base, g.t, -g.first)
		}
		codes, err := codesOf(s.volts, g.vdiv, g.ofst, g.sign)
		if err != nil {
			report("%s: %s: %v", where, name, err)
			continue
		}
		sum := sha256.Sum256(codes)
		if h := hex.EncodeToString(sum[:]); h != want.sha {
			report("%s: the samples recover to %s..., the transcript handed over %s...",
				where, h[:16], want.sha[:min(16, len(want.sha))])
		}
	}

	checkScopeWrites(r, inst, tr, report)
	checkScopeSegments(r, inst, report)

	if len(bad) > 0 {
		return 0, errors.New(strings.Join(bad, "\n"))
	}
	return checked, nil
}

// near compares two of the instrument's numbers. They travel as decimal text in
// the transcript and as f64 in the recording, so an exact comparison would
// report the last bit of a decimal round trip as a difference.
func near(a, b float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

// checkScopeWrites asserts that every setting the recording says was applied is
// the transcript's, in order, and that what the recording says the scope holds
// after each is what the simulator says it holds.
//
// The second half is where clamping shows up. Asking for 0.3 V/div gets 0.2,
// and a daemon that recorded the request as the fact would pass every other
// check here.
func checkScopeWrites(r *recording, inst string, tr *transcript, report func(string, ...any)) {
	type done struct {
		t float64
		w scopeWrite
	}
	var accepted []done
	bySeq := map[uint32]ctlRec{}
	for _, c := range r.controls[inst] {
		if c.verdict == vAccepted && c.seq != 0 && c.request == rqChannelSet {
			bySeq[c.seq] = c
			accepted = append(accepted, done{c.t, scopeWrite{channel: c.channel, value: c.value}})
		}
	}
	sort.SliceStable(accepted, func(i, j int) bool { return accepted[i].t < accepted[j].t })

	for i := 0; i < max(len(accepted), len(tr.writes)); i++ {
		switch {
		case i >= len(accepted):
			report("transcript line %d: %v, which the recording does not say was asked for",
				tr.writes[i].line, tr.writes[i])
		case i >= len(tr.writes):
			report("the recording has %v accepted at %.3fs, and the transcript has no more settings",
				accepted[i].w, accepted[i].t)
		case accepted[i].w.channel != tr.writes[i].channel || !near(accepted[i].w.value, tr.writes[i].value):
			report("setting %d: the recording has %v at %.3fs, transcript line %d has %v",
				i+1, accepted[i].w, accepted[i].t, tr.writes[i].line, tr.writes[i])
		}
	}

	checkScopeHolds(r, tr, report)
}

// checkScopeHolds asserts that what the recording says the scope holds is what
// the simulator says it holds.
//
// This is where clamping shows up, and it is the check a daemon that recorded
// the request as the fact would fail and no other would: asking for 0.3 V/div
// gets 0.2, and only the applied account says so (§4).
//
// The two accounts are compared as sequences of the values each held, with
// consecutive repeats collapsed, rather than write by write. Pairing them off
// would be wrong, because the instrument's settings are coupled: switching a
// second channel on halves the memory depth, so one write produces two changes
// and the simulator says so.
func checkScopeHolds(r *recording, tr *transcript, report func(string, ...any)) {
	want := map[string][]float64{}
	for _, h := range tr.holds {
		want[h.channel] = appendChange(want[h.channel], h.value)
	}
	got := map[string][]float64{}
	for _, name := range sortedKeys(r.scopeSettings) {
		for _, st := range r.scopeSettings[name] {
			for q, v := range st.applied {
				if st.present[q] {
					ch := name + "." + q + ".set"
					got[ch] = appendChange(got[ch], v)
				}
			}
		}
	}

	for _, ch := range sortedKeys(want) {
		w, g := want[ch], got[ch]
		if len(g) == 0 {
			report("%s: the simulator held %v, and the recording has no readback of this setting at all", ch, w)
			continue
		}
		// Every value the recording claims the scope held must be one the
		// simulator says it held. A daemon that wrote the request into the
		// applied account fails here and nowhere else.
		for _, v := range g {
			if !containsNear(w, v) {
				report("%s: the recording says the scope held %g, and the simulator never held it; it held %v",
					ch, v, w)
			}
		}
		// And every value the simulator held must have reached the recording:
		// a readback the daemon did not take is a change nobody can see.
		for _, v := range w {
			if !containsNear(g, v) {
				report("%s: the simulator held %g, and the recording never says the scope did; it says %v",
					ch, v, g)
			}
		}
		if !near(w[len(w)-1], g[len(g)-1]) {
			report("%s: the run ends with the recording saying the scope holds %g and the simulator holding %g",
				ch, g[len(g)-1], w[len(w)-1])
		}
	}
	for _, ch := range sortedKeys(got) {
		if _, ok := want[ch]; !ok {
			report("%s: the recording says the scope held %v, and the simulator's account has no such setting",
				ch, got[ch])
		}
	}
}

// appendChange adds v unless it is what the sequence already ends with: a
// setting read back unchanged is not a change.
func appendChange(s []float64, v float64) []float64 {
	if n := len(s); n > 0 && near(s[n-1], v) {
		return s
	}
	return append(s, v)
}

func containsNear(s []float64, v float64) bool {
	for _, x := range s {
		if near(x, v) {
			return true
		}
	}
	return false
}

// checkScopeSegments asserts §5.2's rule where it bites: a change of sample
// interval must open a new segment, because axis_step lives in the SCHEMA frame
// and a schema is only restated at a segment boundary. A writer that changed it
// inside one would leave every sample of that frame decoding to a wrong
// position and say nothing.
func checkScopeSegments(r *recording, inst string, report func(string, ...any)) {
	for _, name := range sortedKeys(r.scopeSteps) {
		if !strings.HasPrefix(name, inst+".") && name != inst {
			continue
		}
		for _, s := range r.scopeSteps[name] {
			if len(s.steps) > 1 {
				report("%s: segment %d declares axis_step %v; a change of it must open a segment (§5.2)",
					name, s.segment, s.steps)
			}
		}
	}
}

// segSteps is the set of sample intervals one stream declared in one segment.
// More than one is §5.2's rule broken.
type segSteps struct {
	segment int
	steps   []float64
}

// noteStep records the axis_step a stream declared in a segment. A schema is
// restated at every segment boundary, so one stream normally contributes one
// step to each; two means the writer changed it without opening a segment,
// which decodes every sample of the frames after the change to a wrong axis
// position and says nothing.
func (r *recording) noteStep(name string, segment int, step float64) {
	ss := r.scopeSteps[name]
	if n := len(ss); n > 0 && ss[n-1].segment == segment {
		for _, s := range ss[n-1].steps {
			if s == step {
				return
			}
		}
		ss[n-1].steps = append(ss[n-1].steps, step)
		r.scopeSteps[name] = ss
		return
	}
	r.scopeSteps[name] = append(ss, segSteps{segment: segment, steps: []float64{step}})
}

// checkScopeControl asserts that a scope was written only as its control stream
// allowed: every setting record that answers a request answers a channel write
// the control plane accepted for that instrument, and every accepted write was
// recorded as requested.
//
// It is the scope's half of what checkSupplyWrites does for a supply. What the
// instrument then held is a different question, and is checked against the
// simulator's own transcript by -scope, because the recording cannot be its own
// witness for that.
func checkScopeControl(r *recording, report func(string, ...any)) {
	for _, inst := range sortedKeys(r.controls) {
		if !isScope(r, inst) {
			continue
		}
		accepted := map[uint32]ctlRec{}
		for _, c := range r.controls[inst] {
			if c.verdict == vAccepted && c.seq != 0 {
				accepted[c.seq] = c
			}
		}
		requested := map[uint32]bool{}
		for _, name := range sortedKeys(r.scopeSettings) {
			if instrumentOf(name) != inst {
				continue
			}
			for _, s := range r.scopeSettings[name] {
				if s.seq == 0 {
					continue // what the scope reported unasked
				}
				c, ok := accepted[s.seq]
				if !ok || c.request != rqChannelSet {
					report("%s: a setting under seq %d at %.3fs, and %s accepted no write under it",
						name, s.seq, s.t, inst)
					continue
				}
				// A readback may be in a stream the write did not name. The
				// instrument's settings are coupled: a second channel switched
				// on halves the memory depth, and a timebase change moves the
				// sample rate, so a write to one of them changes what the other
				// reports holding, and a recording that said otherwise would be
				// the one being wrong.
				base, _, _ := splitChannel(c.channel)
				if s.event == psRequested {
					if base != name {
						report("%s: seq %d at %.3fs is recorded as requested here, and it asked for %s",
							name, s.seq, s.t, c.channel)
						continue
					}
					requested[s.seq] = true
				}
			}
		}
		for _, seq := range sortedSeqs(accepted) {
			c := accepted[seq]
			if c.request == rqChannelSet && !requested[seq] {
				report("%s: seq %d, a write to %s accepted and never recorded as requested", inst, seq, c.channel)
			}
		}
	}
}

func sortedSeqs(m map[uint32]ctlRec) []uint32 {
	out := make([]uint32, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
