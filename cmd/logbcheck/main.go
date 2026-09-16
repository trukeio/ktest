// Command logbcheck asserts a Logb recording against the candump log it was
// made from.
//
//	logbcheck [flags] recording.logb reference.log
//
// This is milestone 1's acceptance test, and the choice of reference is the
// point of it. The tempting design — have the injector record what it sent in
// Logb, then compare two Logb files — is weaker rather than stronger: both
// sides are written and read by the same library, so a fault in the writer or
// in the reader cancels out and the comparison still passes. A candump log is
// foreign to the format, which is exactly what makes it able to falsify it.
//
// So the harness is a generator, canplayer, canlogb, and this:
//
//	logbcheck -gen 2000 > gen.log
//	canlogb -d 6s -o run.logb vcan0 &
//	canplayer -I gen.log
//	logbcheck run.logb gen.log
//
// ktestd's transmit streams are checked the same way, with the log of what was
// sent as the reference for the requests and a candump log for the bus:
//
//	logbcheck -gen 300 > sent.log
//	candump -L vcan0 > ref.log &
//	ktestd -sock k.sock -d 8s -o run.logb vcan0 &
//	(take the lease, arm, then POST each line of sent.log to
//	 http://ktest/bus/vcan0/send over k.sock with the lease's token)
//	logbcheck -tx sent.log run.logb ref.log
//
// The control plane is checked with -control, against the limits the
// recording itself declares: nothing accepted unarmed or without the lease, no
// cyclic period below the floor, no refusal its state does not justify, every
// disarm stopping every running task, and every lapsed lease expiring on time.
// Every instrument's control stream is checked so, a supply's too, and on a
// supply also: no write accepted above the channel's stated limit, every
// output switched off by a disarm or a stop, and nothing written to it that
// its control stream did not accept. The rack's panel saves in rack.panel are
// checked with them: a revision for each save and none for a refusal, saves
// only from names that may make them, and the panel the recording ends with
// the one its last record names.
//
// A supply is checked against its own account with -psu, the transcript
// psusim keeps of what it was told, which nothing that writes Logb wrote:
//
//	psusim -transcript psu.log &
//	ktestd -psu psu1=127.0.0.1:5025,2 -limits limits.json -sock k.sock -d 8s -o run.logb vcan0 &
//	(lease psu1, arm it, PUT /channels/psu1.ch1.v.set, ...)
//	logbcheck -control -psu psu1=psu.log run.logb ref.log
//
// A scope is checked against its own account with -scope, the transcript
// scopesim keeps of what it was told and what it handed over — every setting,
// what it holds after each, and a sha256 of the bytes of every acquisition:
//
//	scopesim -transcript scope.log -channels 'sine:amp=2,freq=1000' &
//	ktestd -scope scope1=127.0.0.1:5026,2,full=ch1 -sock k.sock -d 8s -o run.logb vcan0 &
//	(lease scope1, arm it, PUT /channels/scope1.ch1.vert.vdiv.set, ...)
//	logbcheck -control -scope scope1=scope.log run.logb ref.log
//
// Every acquisition the recording carries must be the transcript's, at the same
// position, with the same settings, and its samples must hash to what the
// simulator handed over. A sample is stored as f32 volts (§13), so the hash is
// taken over the codes recovered from the volts by the recording's own stated
// conversion: a daemon that converted with a different volts/div, a different
// offset or the other sign rule, or that stated a conversion it did not use,
// produces different bytes and the hash says so. Each acquisition's axis is
// computed from the transcript's numbers by the dialect's rule — step 1/sara,
// sample 0 at -(tdiv x 14 / 2) — and not from the daemon's arithmetic. Every
// setting the recording says was applied must be the transcript's, and what the
// recording says the scope holds must be what the simulator holds, which is
// where clamping to the instrument's 1-2-5 steps shows up. And a change of
// sample interval must have opened a new segment (§5.2).
//
// Cyclic tasks are checked with -cyclic, against the same candump log: each
// change's request must agree with the kernel's TX_READ, every frame a task
// put on the bus must fall inside a time it was running and carry the data
// then in force, and a counted run must put exactly its count on the bus.
//
// What is checked is what replay preserves: every frame, in order, with its
// identifier, its length, its flags and its payload bytes. What is not checked
// is absolute time, because canplayer replays on its own clock — the spacing
// between frames is compared instead, and only loosely, since §12 records that
// vcan replay jitters at p99 976 µs against a 1 ms spacing.
package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rveen/logb"
)

func main() {
	var (
		gen     = flag.Int("gen", 0, "instead of checking, write this many frames of a candump log to stdout")
		seed    = flag.Int64("seed", 7, "-gen: seed, so the same log can be regenerated")
		spacing = flag.Float64("spacing", 0.001, "-gen: seconds between frames")
		iface   = flag.String("iface", "vcan0", "-gen: the interface name to write into the log")
		jitter  = flag.Float64("jitter", 0.05, "largest tolerated error in a frame-to-frame interval, in seconds")
		verbose = flag.Bool("v", false, "report every difference rather than the first ten")
		tx      = flag.String("tx", "", "also check the transmit streams against this candump log of the frames sent")
		cyc     = flag.Bool("cyclic", false, "also check every cyclic task's stream against the frames it put on the bus")
		ctl     = flag.Bool("control", false, "also check the control streams against the limits the recording declares")
		psuT    = flag.String("psu", "", "also check a supply against the transcript of what it was told: name=transcript.log")
		scopeT  = flag.String("scope", "", "also check a scope against the simulator's own account of the run: name=transcript.log")
	)
	flag.Usage = usage
	flag.Parse()

	if *gen > 0 {
		if err := generate(os.Stdout, *gen, *seed, *spacing, *iface); err != nil {
			fatal(err)
		}
		return
	}
	if flag.NArg() != 2 {
		flag.Usage()
	}

	want, err := readLog(flag.Arg(1))
	if err != nil {
		fatal(err)
	}
	rec, err := readLogb(flag.Arg(0))
	if err != nil {
		fatal(err)
	}
	if err := compare(rec.raw, want, *jitter, *verbose); err != nil {
		fail(err)
	}
	fmt.Printf("logbcheck: %d frames match %s byte for byte, in order\n", len(want), flag.Arg(1))

	if *tx != "" {
		sent, err := readLog(*tx)
		if err != nil {
			fatal(err)
		}
		if err := compareTX(rec.set, rec.applied, sent, *verbose); err != nil {
			fail(err)
		}
		fmt.Printf("logbcheck: %d requests match %s in order, each applied exactly once and after it was requested\n",
			len(sent), *tx)
	}
	if *cyc {
		n, err := compareCyclic(rec, *verbose)
		if err != nil {
			fail(err)
		}
		fmt.Printf("logbcheck: %d cyclic tasks agree with the kernel's account, and every frame they put on the bus "+
			"carried the data in force, inside a time the task was running, once a period\n", n)
	}
	if *ctl {
		n, floor, err := compareControl(rec, *verbose)
		if err != nil {
			fail(err)
		}
		fmt.Printf("logbcheck: %d control decisions keep to the limits the recording declares (cyclic floor %v): "+
			"nothing accepted unarmed or without the lease, no refusal its state does not justify, "+
			"every disarm stopped every running task, and every lapsed lease expired on time\n", n, floor)
	}
	if *psuT != "" {
		name, path, ok := strings.Cut(*psuT, "=")
		if !ok {
			fatal(fmt.Errorf("-psu %q: want name=transcript.log", *psuT))
		}
		tr, err := readTranscript(path)
		if err != nil {
			fatal(err)
		}
		n, err := compareSupply(rec, name, tr, *verbose)
		if err != nil {
			fail(err)
		}
		fmt.Printf("logbcheck: %d writes %s carried out match its transcript %s, in order, channel and value\n",
			n, name, path)
	}
	if *scopeT != "" {
		name, path, ok := strings.Cut(*scopeT, "=")
		if !ok {
			fatal(fmt.Errorf("-scope %q: want name=transcript.log", *scopeT))
		}
		tr, err := readScopeTranscript(path, name)
		if err != nil {
			fatal(err)
		}
		n, err := compareScope(rec, name, tr, *verbose)
		if err != nil {
			fail(err)
		}
		fmt.Printf("logbcheck: %d acquisitions of %s are the ones %s says it handed over, in order, "+
			"under the settings in force, with the axis the dialect's rule gives and the samples recovering "+
			"byte for byte to the sha256 it recorded; every setting applied is the transcript's, and what "+
			"the recording says the scope holds is what the simulator holds\n", n, name, path)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "logbcheck: FAIL\n%v\n", err)
	os.Exit(1)
}

// frame is the part of a CAN frame that survives a round trip through a
// candump log, and therefore the whole of what can be compared.
type frame struct {
	t        float64 // seconds; absolute in the log, segment-relative in the recording
	id       uint32
	extended bool
	data     []byte
	fd, brs  bool // CAN FD, and its bit rate switch

	// local is the recording's word that the frame was sent from its own
	// host. A recording made before the bit existed says false throughout.
	local bool
}

func (f frame) String() string {
	id := fmt.Sprintf("%03X", f.id)
	if f.extended {
		id = fmt.Sprintf("%08X", f.id)
	}
	sep := "#"
	if f.fd {
		flags := 0
		if f.brs {
			flags = 1
		}
		sep = fmt.Sprintf("##%X", flags)
	}
	return fmt.Sprintf("%s%s%s", id, sep, strings.ToUpper(hex.EncodeToString(f.data)))
}

// readLog parses a candump log: "(1773480413.001000) vcan0 100#14055A28".
//
// An identifier of more than three hex digits is extended, which is candump's
// own convention and the reason a generator must pad one to eight.
func readLog(path string) ([]frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []frame
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" {
			continue
		}
		fields := strings.Fields(s)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want (time) iface id#data, got %q", path, line, s)
		}
		t, err := strconv.ParseFloat(strings.Trim(fields[0], "()"), 64)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		id, data, ok := strings.Cut(fields[2], "#")
		if !ok {
			return nil, fmt.Errorf("%s:%d: no # in %q", path, line, fields[2])
		}
		n, err := strconv.ParseUint(id, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		// A remote frame writes its length after the R; nothing here sends
		// one, so it is refused rather than half-handled.
		if strings.HasPrefix(data, "R") {
			return nil, fmt.Errorf("%s:%d: remote frames are not compared", path, line)
		}
		// A CAN FD frame is written ID##F<data>, F being one hex digit of
		// canfd_frame.flags: 1 is the bit rate switch, 2 the error state
		// indicator.
		var fd, brs bool
		if rest, ok := strings.CutPrefix(data, "#"); ok {
			if rest == "" {
				return nil, fmt.Errorf("%s:%d: a CAN FD frame without its flags digit", path, line)
			}
			flags, err := strconv.ParseUint(rest[:1], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: CAN FD flags: %w", path, line, err)
			}
			fd, brs, data = true, flags&1 != 0, rest[1:]
		}
		b, err := hex.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out = append(out, frame{t: t, id: uint32(n), extended: len(id) > 3, data: b, fd: fd, brs: brs})
	}
	return out, sc.Err()
}

// txRec is one record of a transmit stream: the frame, the request number it
// belongs to, and on the set stream the kernel's answer to the request.
type txRec struct {
	frame
	seq   uint32
	errno uint64
}

// recording is what logbcheck reads out of a Logb file. A recording made by a
// program that does not transmit has no transmit or cyclic streams, and those
// parts come back empty.
type recording struct {
	raw          []frame
	set, applied []txRec
	cyclic       map[string][]cycRec // by task name
	tasks        []string            // task names, in the order first seen
	control      []ctlRec            // the bus's control stream
	controls     map[string][]ctlRec // every other instrument's, by name
	psu          map[string][]psuRec // supply channel streams, by stream name: psu1.ch1

	// The scope's, by instrument and by stream name. A waveform is one batch
	// per acquisition, so its samples are held by run rather than appended.
	scopeAcqs     map[string][]scopeAcq
	scopeSamples  map[string]map[uint32]scopeSamples
	scopeSettings map[string][]scopeSetting
	scopeSteps    map[string][]segSteps
	panel         []panelRec // rack.panel: the rack's panel saves
	panelAttach   []byte     // the panel.json the recording ends with; nil for none
	meta          map[string]string
}

// cycRec is one change to a cyclic task, as the task's stream records it.
type cycRec struct {
	t        float64
	seq      uint32
	id       uint32
	extended bool
	event    uint64
	errno    uint64
	active   bool
	data     []byte
	period   float64 // seconds
	count    uint64

	// The kernel's account, when applied says there is one.
	applied bool
	aData   []byte
	aPeriod float64
	aCount  uint64
}

// The event field of a cyclic stream, as its schema describes it.
const (
	evStart    = 1
	evUpdate   = 2
	evStop     = 3
	evExpired  = 4
	evEnded    = 5
	evDisarmed = 6
)

// ctlRec is one decision of the control plane, as its stream records it.
type ctlRec struct {
	t       float64
	seq     uint32
	request uint64
	verdict uint64
	period  float64 // seconds
	armed   bool
	leased  bool
	ttl     float64 // seconds
	holder  string

	// Who asked, and through which listener. A recording made before TLS
	// says neither, and reads as the local socket.
	by  string
	via uint64

	// What a channel write named, and the value it asked for.
	channel string
	value   float64
}

// The request and verdict fields of the control stream, as its schema
// describes them.
const (
	rqSend         = 1
	rqCyclicSet    = 2
	rqCyclicDelete = 3
	rqArm          = 4
	rqDisarm       = 5
	rqLeaseTake    = 6
	rqLeaseRenew   = 7
	rqLeaseRelease = 8
	rqLeaseExpired = 9
	rqShutdown     = 10
	rqChannelSet   = 11
	rqStop         = 12

	vAccepted   = 0
	vNotArmed   = 1
	vNoLease    = 2
	vLeaseHeld  = 3
	vBelowFloor = 4
	vForbidden  = 5
	vAboveLimit = 6

	viaTLS = 2
)

// needsLease is the requests the control plane admits only from the lease's
// holder.
var needsLease = map[uint64]bool{
	rqSend: true, rqCyclicSet: true, rqCyclicDelete: true, rqArm: true, rqLeaseRenew: true, rqLeaseRelease: true,
	rqChannelSet: true,
}

var requestNames = map[uint64]string{
	rqSend: "send", rqCyclicSet: "cyclic set", rqCyclicDelete: "cyclic delete", rqArm: "arm",
	rqDisarm: "disarm", rqLeaseTake: "lease take", rqLeaseRenew: "lease renewal",
	rqLeaseRelease: "lease release", rqLeaseExpired: "lease expiry", rqShutdown: "shutdown",
	rqChannelSet: "channel write", rqStop: "stop",
}

// readLogb reads the bus stream, the transmit streams and the cyclic task
// streams back out of a recording.
//
// It reads the fields by name rather than by offset, so that it is checking
// what the schema in the file says and not what this program assumes the
// writer did. A recording whose schema drifted would fail here rather than be
// silently reinterpreted.
func readLogb(path string) (*recording, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r, err := logb.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, err
	}
	out := &recording{cyclic: map[string][]cycRec{}, controls: map[string][]ctlRec{}, psu: map[string][]psuRec{},
		scopeAcqs: map[string][]scopeAcq{}, scopeSamples: map[string]map[uint32]scopeSamples{},
		scopeSettings: map[string][]scopeSetting{}, scopeSteps: map[string][]segSteps{}}
	// Which segment each batch fell in, so that §5.2's rule — a change of
	// axis_step opens a segment — can be checked against the file rather than
	// taken on trust. The reader rebinds every stream id at a SYNC, so a schema
	// frame is how a segment announces itself here.
	segment := 0
	r.OnSchema = func(sc *logb.Schema, id uint16) {
		if id == 0 {
			segment++
		}
		if sc.AxisMode != logb.AxisImplicit {
			return
		}
		out.noteStep(sc.Name, segment, float64(int64(sc.AxisStep))*math.Pow10(int(sc.AxisExp)))
	}
	for {
		b, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := b.Schema.Name
		if name == "rack.panel" {
			ps, err := readPanel(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out.panel = append(out.panel, ps...)
			continue
		}
		if inst, ok := strings.CutSuffix(name, ".control"); ok {
			cs, err := readControl(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out.controls[inst] = append(out.controls[inst], cs...)
			continue
		}
		if inst, ok := strings.CutSuffix(name, ".acq"); ok {
			as, err := readScopeAcq(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out.scopeAcqs[inst] = append(out.scopeAcqs[inst], as...)
			continue
		}
		if strings.HasSuffix(name, ".vert") || strings.HasSuffix(name, ".timebase") {
			ss, err := readScopeSetting(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out.scopeSettings[name] = append(out.scopeSettings[name], ss...)
			continue
		}
		if b.Schema.AxisMode == logb.AxisImplicit && scopeStream.MatchString(name) {
			sm, err := readScopeSamples(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if out.scopeSamples[name] == nil {
				out.scopeSamples[name] = map[uint32]scopeSamples{}
			}
			out.scopeSamples[name][sm.run] = sm
			continue
		}
		if psuChannel.MatchString(name) {
			ps, err := readPsu(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			out.psu[name] = append(out.psu[name], ps...)
			continue
		}
		if i := strings.Index(name, ".cyclic."); i >= 0 {
			task := name[i+len(".cyclic."):]
			rs, err := readCyclic(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if _, seen := out.cyclic[task]; !seen {
				out.tasks = append(out.tasks, task)
			}
			out.cyclic[task] = append(out.cyclic[task], rs...)
			continue
		}
		isRaw := strings.HasSuffix(name, ".raw")
		isSet := strings.HasSuffix(name, ".tx.set")
		if !isRaw && !isSet && !strings.HasSuffix(name, ".tx.applied") {
			continue
		}
		names := []string{"can_id", "len", "extended", "payload"}
		if !isRaw {
			names = append(names, "seq")
		}
		if isSet {
			names = append(names, "errno")
		}
		idx, err := fieldIndex(b.Schema, names...)
		if err != nil {
			return nil, err
		}
		// Fields a raw stream carries now and an older recording may not.
		if isRaw {
			for _, opt := range []string{"local", "fd", "brs"} {
				if p := fieldPos(b.Schema, opt); p >= 0 {
					idx[opt] = p
				}
			}
		}
		for i := 0; i < int(b.Count); i++ {
			fr, err := frameAt(b, i, idx)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if isRaw {
				out.raw = append(out.raw, fr)
				continue
			}
			seq, err := uintField(b, i, idx["seq"])
			if err != nil {
				return nil, err
			}
			rec := txRec{frame: fr, seq: uint32(seq)}
			if !isSet {
				out.applied = append(out.applied, rec)
				continue
			}
			if rec.errno, err = uintField(b, i, idx["errno"]); err != nil {
				return nil, err
			}
			out.set = append(out.set, rec)
		}
	}
	if r.Truncated {
		return nil, fmt.Errorf("the recording is truncated: it ended at damage, not at an END frame")
	}
	out.meta = map[string]string{}
	for _, m := range r.Meta {
		out.meta[m.Key] = m.Value
	}
	out.panelAttach = r.Attachments["panel.json"]
	// The bus's control stream is told from the instruments' by name: the
	// recording says which interface it recorded (§11). Guessing instead —
	// "anything that is not a supply is the bus" — was right while a supply was
	// the only other instrument, and quietly threw one of them away as soon as
	// there were two kinds.
	bus := out.meta["bus.interface"]
	if cs, ok := out.controls[bus]; ok {
		out.control = cs
		delete(out.controls, bus)
	} else {
		// A recording from before bus.interface was written, or one whose bus
		// stream is named something else: fall back to the old rule, which is
		// still right when there is exactly one instrument-less candidate.
		for inst, cs := range out.controls {
			_, isSupply := out.meta["psu."+inst+".channels"]
			_, isScope := out.meta["scope."+inst+".channels"]
			if !isSupply && !isScope {
				out.control = cs
				delete(out.controls, inst)
			}
		}
	}
	return out, nil
}

// readControl reads one batch of the control stream.
func readControl(b *logb.Batch) ([]ctlRec, error) {
	idx, err := fieldIndex(b.Schema, "seq", "request", "verdict", "armed", "leased", "period", "ttl", "holder")
	if err != nil {
		return nil, err
	}
	byI, viaI := fieldPos(b.Schema, "by"), fieldPos(b.Schema, "via")
	chI, valI := fieldPos(b.Schema, "channel"), fieldPos(b.Schema, "value")
	var out []ctlRec
	for i := 0; i < int(b.Count); i++ {
		var first error
		num := func(name string) uint64 {
			v, err := uintField(b, i, idx[name])
			if err != nil && first == nil {
				first = err
			}
			return v
		}
		flag := func(name string) bool {
			v, err := b.Value(i, idx[name])
			if err != nil && first == nil {
				first = err
			}
			return v == true
		}
		axis, err := b.Axis(i)
		if err != nil {
			return nil, err
		}
		c := ctlRec{
			t:       axis.Seconds(b.Schema.AxisExp),
			seq:     uint32(num("seq")),
			request: num("request"),
			verdict: num("verdict"),
			period:  float64(num("period")) / 1e9,
			armed:   flag("armed"),
			leased:  flag("leased"),
			ttl:     float64(num("ttl")) / 1e9,
		}
		text := func(f int) string {
			v, err := b.Value(i, f)
			if err != nil && first == nil {
				first = err
			}
			s, _ := v.(string)
			return s
		}
		c.holder = text(idx["holder"])
		if byI >= 0 {
			c.by = text(byI)
		}
		if viaI >= 0 {
			v, err := uintField(b, i, viaI)
			if err != nil && first == nil {
				first = err
			}
			c.via = v
		}
		if chI >= 0 {
			c.channel = text(chI)
		}
		if valI >= 0 {
			v, err := b.Value(i, valI)
			if err != nil && first == nil {
				first = err
			}
			c.value, _ = v.(float64)
		}
		if first != nil {
			return nil, fmt.Errorf("record %d: %w", i, first)
		}
		out = append(out, c)
	}
	return out, nil
}

// frameAt reads record i of a batch as a frame.
func frameAt(b *logb.Batch, i int, idx map[string]int) (frame, error) {
	rec, err := b.Record(i)
	if err != nil {
		return frame{}, err
	}
	id, err := uintField(b, i, idx["can_id"])
	if err != nil {
		return frame{}, err
	}
	n, err := uintField(b, i, idx["len"])
	if err != nil {
		return frame{}, err
	}
	ext, err := b.Value(i, idx["extended"])
	if err != nil {
		return frame{}, err
	}
	data, err := blob(b, rec, idx["payload"], n)
	if err != nil {
		return frame{}, fmt.Errorf("record %d: %w", i, err)
	}
	axis, err := b.Axis(i)
	if err != nil {
		return frame{}, err
	}
	fr := frame{
		t:        axis.Seconds(b.Schema.AxisExp),
		id:       uint32(id),
		extended: ext == true,
		data:     data,
	}
	for _, opt := range []struct {
		name string
		dst  *bool
	}{{"local", &fr.local}, {"fd", &fr.fd}, {"brs", &fr.brs}} {
		if p, ok := idx[opt.name]; ok {
			v, err := b.Value(i, p)
			if err != nil {
				return frame{}, err
			}
			*opt.dst = v == true
		}
	}
	return fr, nil
}

// readCyclic reads one batch of a cyclic task's stream.
func readCyclic(b *logb.Batch) ([]cycRec, error) {
	idx, err := fieldIndex(b.Schema, "seq", "can_id", "extended", "event", "errno", "active", "applied",
		"len", "period", "count", "payload", "applied_len", "applied_period", "applied_count", "applied_payload")
	if err != nil {
		return nil, err
	}
	var out []cycRec
	for i := 0; i < int(b.Count); i++ {
		rec, err := b.Record(i)
		if err != nil {
			return nil, err
		}
		var first error
		num := func(name string) uint64 {
			v, err := uintField(b, i, idx[name])
			if err != nil && first == nil {
				first = err
			}
			return v
		}
		flag := func(name string) bool {
			v, err := b.Value(i, idx[name])
			if err != nil && first == nil {
				first = err
			}
			return v == true
		}
		axis, err := b.Axis(i)
		if err != nil {
			return nil, err
		}
		c := cycRec{
			t:        axis.Seconds(b.Schema.AxisExp),
			seq:      uint32(num("seq")),
			id:       uint32(num("can_id")),
			extended: flag("extended"),
			event:    num("event"),
			errno:    num("errno"),
			active:   flag("active"),
			applied:  flag("applied"),
			period:   float64(num("period")) / 1e9,
			count:    num("count"),
		}
		c.data, err = blob(b, rec, idx["payload"], num("len"))
		if err == nil && c.applied {
			c.aPeriod, c.aCount = float64(num("applied_period"))/1e9, num("applied_count")
			c.aData, err = blob(b, rec, idx["applied_payload"], num("applied_len"))
		}
		if err == nil {
			err = first
		}
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		out = append(out, c)
	}
	return out, nil
}

// blob is the first n bytes of a bytes field, refusing a length the field
// cannot hold.
func blob(b *logb.Batch, rec []byte, f int, n uint64) ([]byte, error) {
	pf := b.Schema.Fields[f]
	if n > uint64(pf.BitWidth/8) {
		return nil, fmt.Errorf("%d bytes claimed in the %d-byte field %s", n, pf.BitWidth/8, pf.Name)
	}
	off := pf.BitOffset / 8
	return append([]byte(nil), rec[off:off+uint32(n)]...), nil
}

func fieldPos(s *logb.Schema, name string) int {
	for i := range s.Fields {
		if s.Fields[i].Name == name {
			return i
		}
	}
	return -1
}

func fieldIndex(s *logb.Schema, names ...string) (map[string]int, error) {
	idx := map[string]int{}
	for _, n := range names {
		found := -1
		for i := range s.Fields {
			if s.Fields[i].Name == n {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("stream %q has no field %q", s.Name, n)
		}
		idx[n] = found
	}
	return idx, nil
}

func uintField(b *logb.Batch, i, f int) (uint64, error) {
	v, err := b.Raw(i, f)
	if err != nil {
		return 0, err
	}
	n, ok := v.(uint64)
	if !ok {
		return 0, fmt.Errorf("field %d of record %d is %T, not an integer", f, i, v)
	}
	return n, nil
}

// compare is the assertion. Count first, then frame by frame in order, then
// the intervals.
func compare(got, want []frame, jitter float64, verbose bool) error {
	var bad []string
	report := func(format string, a ...any) {
		if len(bad) < 10 || verbose {
			bad = append(bad, fmt.Sprintf(format, a...))
		}
	}

	if len(got) != len(want) {
		// Not fatal on its own: showing which frames differ says far more
		// about why than the count does.
		report("frame count: recorded %d, replayed %d", len(got), len(want))
	}
	n := min(len(got), len(want))
	for i := 0; i < n; i++ {
		g, w := got[i], want[i]
		if g.id != w.id || g.extended != w.extended {
			report("frame %d: identifier %s, want %s", i, g, w)
			continue
		}
		if g.fd != w.fd || g.brs != w.brs {
			report("frame %d: %s, want %s", i, g, w)
			continue
		}
		if len(g.data) != len(w.data) {
			report("frame %d: %d payload bytes, want %d (%s against %s)",
				i, len(g.data), len(w.data), g, w)
			continue
		}
		for j := range g.data {
			if g.data[j] != w.data[j] {
				report("frame %d: payload byte %d is %#02x, want %#02x (%s against %s)",
					i, j, g.data[j], w.data[j], g, w)
				break
			}
		}
	}

	// Timing is compared as intervals, never as absolute times: canplayer
	// replays on its own clock, and the recording's axis is relative to its
	// own start. §12 measures that replay's jitter at p99 976 µs against a
	// 1 ms spacing, so the tolerance here is loose on purpose — this catches a
	// frame recorded out of order or an axis that does not advance, not the
	// jitter of a virtual bus.
	var worst float64
	var worstAt = -1
	for i := 1; i < n; i++ {
		// A step back is a failure only if the reference did not take it too.
		// Kernel receive stamps are not monotonic in delivery order when two
		// senders on the host race on different CPUs: measured on vcan with
		// CAN_BCM tasks on six cores, one frame in a thousand arrived stamped
		// 2.9 µs before the frame ahead of it, and candump saw exactly the same
		// reversal. A recording that reproduces the kernel's order and stamps is
		// right; one that reverses what the reference did not is not.
		if got[i].t < got[i-1].t && want[i].t >= want[i-1].t {
			report("frame %d: the axis went backwards, %.6fs after %.6fs",
				i, got[i].t, got[i-1].t)
			continue
		}
		d := math.Abs((got[i].t - got[i-1].t) - (want[i].t - want[i-1].t))
		if d > worst {
			worst, worstAt = d, i
		}
	}
	if worst > jitter {
		report("frame %d: the gap before it differs from the log by %.1f ms, more than the %.1f ms tolerated",
			worstAt, worst*1000, jitter*1000)
	}

	if len(bad) > 0 {
		if !verbose && len(bad) == 10 {
			bad = append(bad, "(more differences suppressed; -v shows them all)")
		}
		return errors.New(strings.Join(bad, "\n"))
	}
	return nil
}

// compareTX asserts the transmit streams: the requests against the log of what
// was sent, then each request against its echo.
//
// The log is the reference for the requests for the same reason a candump log
// is the reference for the bus: it was written by the client, outside the
// format. The echoes are checked against the requests rather than against the
// log, because what they establish is the pairing — every frame the kernel took
// came back once, as itself, and no sooner than it was asked for.
func compareTX(set, applied []txRec, sent []frame, verbose bool) error {
	var bad []string
	report := func(format string, a ...any) {
		if len(bad) < 10 || verbose {
			bad = append(bad, fmt.Sprintf(format, a...))
		}
	}

	// Request timing is the client's, not the log's, so intervals are not
	// compared: an infinite tolerance keeps the order and content checks.
	reqs := make([]frame, len(set))
	for i := range set {
		reqs[i] = set[i].frame
	}
	if err := compare(reqs, sent, math.Inf(1), verbose); err != nil {
		report("requests against the log:\n%v", err)
	}

	bySeq := map[uint32]txRec{}
	for i, s := range set {
		if s.errno != 0 {
			report("request %d (seq %d, %s): the kernel refused it, errno %d", i, s.seq, s.frame, s.errno)
		}
		if i > 0 && s.seq <= set[i-1].seq {
			report("request %d: seq %d does not follow %d", i, s.seq, set[i-1].seq)
		}
		bySeq[s.seq] = s
	}
	seen := map[uint32]bool{}
	for i, a := range applied {
		s, ok := bySeq[a.seq]
		switch {
		case a.seq == 0:
			report("echo %d (%s) answers no request", i, a.frame)
		case !ok:
			report("echo %d (%s) names seq %d, which was never requested", i, a.frame, a.seq)
		case seen[a.seq]:
			report("seq %d was applied twice", a.seq)
		default:
			seen[a.seq] = true
			if !sameFrame(a.frame, s.frame) {
				report("seq %d: requested %s, the kernel handed back %s", a.seq, s.frame, a.frame)
			}
			if a.t < s.t {
				report("seq %d: applied %.6fs before it was requested", a.seq, s.t-a.t)
			}
		}
	}
	for _, s := range set {
		if s.errno == 0 && !seen[s.seq] {
			report("seq %d (%s): the kernel took it and never handed it back", s.seq, s.frame)
		}
	}

	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "\n"))
	}
	return nil
}

// compareCyclic asserts each cyclic task's stream against the frames on the
// bus. The bus side is the raw stream, which has by now been checked against
// the candump log, so the reference is still foreign to the format.
//
// A task's frames are the raw frames with its identifier that were sent from
// the recording host, less the daemon's own single sends, which tx.applied
// identifies. That attribution holds only while nothing else on the host sends
// the same identifier, which a test has to arrange.
func compareCyclic(r *recording, verbose bool) (int, error) {
	var bad []string
	report := func(format string, a ...any) {
		if len(bad) < 10 || verbose {
			bad = append(bad, fmt.Sprintf(format, a...))
		}
	}
	// grace is how late a frame may still belong to the state before a change:
	// the daemon takes a request's time before it calls the kernel.
	const grace = 0.005

	// A single send and a cyclic task can carry the same message, sent once
	// and sent every period, so the single sends are set aside first.
	single := singleSends(r)
	for _, name := range r.tasks {
		evs := r.cyclic[name]
		id, ext := evs[0].id, evs[0].extended
		var bus []frame
		for i, f := range r.raw {
			if f.local && f.id == id && f.extended == ext && !single[i] {
				bus = append(bus, f)
			}
		}
		inForce := func(e cycRec) ([]byte, float64) {
			if e.applied {
				return e.aData, e.aPeriod
			}
			return e.data, e.period
		}

		// The two accounts of each change agree.
		for _, e := range evs {
			switch {
			case e.errno != 0:
			case e.event == evStart || e.event == evUpdate:
				if !e.applied {
					report("%s seq %d: the kernel took the task and gave no account of it", name, e.seq)
					continue
				}
				if !bytes.Equal(e.data, e.aData) || e.period != e.aPeriod {
					report("%s seq %d: asked for %X every %gs, the kernel holds %X every %gs",
						name, e.seq, e.data, e.period, e.aData, e.aPeriod)
				}
				// The first frame of a counted run goes out as it starts, so
				// the kernel may already be one short.
				if e.count > 0 && (e.aCount > e.count || e.aCount+1 < e.count) {
					report("%s seq %d: asked for %d frames, the kernel has %d to send", name, e.seq, e.count, e.aCount)
				}
			case e.active:
				report("%s: still active after event %d", name, e.event)
			}
		}

		// Every frame falls in a stretch where the task ran, and carries the
		// data in force then.
		j := -1
		for _, f := range bus {
			for j+1 < len(evs) && evs[j+1].t <= f.t {
				j++
			}
			if j < 0 {
				report("%s: %s at %.6fs, before the task began", name, f, f.t)
				continue
			}
			e := evs[j]
			d, _ := inForce(e)
			ok := e.active && bytes.Equal(f.data, d)
			if !ok && f.t-e.t < grace && j > 0 && evs[j-1].active {
				pd, _ := inForce(evs[j-1])
				ok = bytes.Equal(f.data, pd)
			}
			switch {
			case ok:
			case e.active:
				report("%s: %s at %.6fs carries data that was not in force", name, f, f.t)
			default:
				report("%s: %s at %.6fs, while the task was not running", name, f, f.t)
			}
		}

		// The rhythm fits. A counted run that expired put exactly its count on
		// the bus. One that runs until stopped put a frame down once a period,
		// never skipping one and never doubling up, from the change that
		// started the stretch to the change that ended it.
		//
		// That is asserted interval by interval rather than on the count,
		// because CAN_BCM keeps no absolute timetable: it re-arms from each
		// expiry, and measured on vcan every interval ran some tens of
		// microseconds long, so over seconds the count falls measurably short
		// of the stretch over the period while no frame is missing.
		for k, e := range evs {
			if !e.active {
				continue
			}
			if k+1 == len(evs) {
				report("%s: still running when the recording ended", name)
				continue
			}
			next := evs[k+1]
			var in []frame
			for _, f := range bus {
				if f.t >= e.t && f.t < next.t {
					in = append(in, f)
				}
			}
			_, p := inForce(e)
			switch {
			case e.count > 0 && next.event == evExpired:
				if len(in) != int(e.count) {
					report("%s: a run of %d frames put %d on the bus", name, e.count, len(in))
				}
			case e.count == 0 && p > 0:
				prev := e.t
				for i, f := range in {
					gap := f.t - prev
					if gap > 1.5*p+grace {
						report("%s: %.3f ms without a frame before %.6fs, at one per %g ms", name, gap*1000, f.t, p*1000)
					}
					// The first frame of a restarted task goes out at once, so
					// only the gaps between frames have a floor.
					if i > 0 && gap < 0.5*p {
						report("%s: frames %.3f ms apart at %.6fs, at one per %g ms", name, gap*1000, f.t, p*1000)
					}
					prev = f.t
				}
				if gap := next.t - prev; gap > 1.5*p+grace {
					report("%s: %.3f ms without a frame before the change at %.6fs, at one per %g ms",
						name, gap*1000, next.t, p*1000)
				}
			}
		}
	}

	if len(bad) > 0 {
		if !verbose && len(bad) == 10 {
			bad = append(bad, "(more differences suppressed; -v shows them all)")
		}
		return 0, errors.New(strings.Join(bad, "\n"))
	}
	return len(r.tasks), nil
}

// singleSends finds the raw frames that are the daemon's single sends: for
// each echo in tx.applied, the local raw frame that carries the same frame
// nearest the echo's time, within a millisecond. The echo and the monitor's
// copy are the kernel's stamps on one delivery — identical to the nanosecond,
// measured on vcan — so the tolerance is generous; each raw frame answers one
// echo at most.
func singleSends(r *recording) map[int]bool {
	const tol = 0.001
	taken := map[int]bool{}
	for _, a := range r.applied {
		best, bestD := -1, tol
		for i, f := range r.raw {
			if taken[i] || !f.local || !sameFrame(f, a.frame) {
				continue
			}
			if d := math.Abs(f.t - a.t); d <= bestD {
				best, bestD = i, d
			}
		}
		if best >= 0 {
			taken[best] = true
		}
	}
	return taken
}

// compareControl asserts the control stream against the limits the recording
// declares and against itself: what was accepted, what was refused and why,
// what a disarm did to the tasks that were running, and when leases ended.
//
// It returns the number of decisions and the cyclic floor it held them to.
func compareControl(r *recording, verbose bool) (int, time.Duration, error) {
	floor, err := time.ParseDuration(r.meta["limits.cyclic.min_period"])
	if err != nil {
		return 0, 0, fmt.Errorf("the recording does not state its cyclic period floor (limits.cyclic.min_period): %v", err)
	}
	if len(r.control) == 0 {
		return 0, floor, errors.New("the recording has no control stream")
	}
	var bad []string
	report := func(format string, a ...any) {
		if len(bad) < 10 || verbose {
			bad = append(bad, fmt.Sprintf(format, a...))
		}
	}
	// grace is how long a disarm may take to stop every running task, and lag
	// how late the daemon may notice a lapsed lease: its loop looks five times
	// a second, and before every request.
	const grace, lag = 0.05, 0.25
	fl := floor.Seconds()

	// stateAt is the control plane's state at t: that of its last decision at
	// or before t.
	stateAt := func(t float64) (ctlRec, bool) {
		i := sort.Search(len(r.control), func(i int) bool { return r.control[i].t > t }) - 1
		if i < 0 {
			return ctlRec{}, false
		}
		return r.control[i], true
	}
	// runningBefore is whether a task was running just before t.
	runningBefore := func(evs []cycRec, t float64) bool {
		on := false
		for _, e := range evs {
			if e.t >= t {
				break
			}
			on = e.active
		}
		return on
	}
	state := func(c ctlRec) string {
		switch {
		case !c.leased:
			return "without a lease"
		case !c.armed:
			return "unarmed"
		}
		return "armed"
	}

	// The names the recording says could act over the network, and their roles.
	roles := map[string]string{}
	for _, nr := range strings.Split(r.meta["auth.identities"], ",") {
		if n, role, ok := strings.Cut(strings.TrimSpace(nr), ":"); ok {
			roles[n] = role
		}
	}

	// Every instrument is checked on its own stream, the same way: its own
	// lease, its own arming. What follows from a decision differs — tasks on
	// the bus, outputs on a supply — and is checked beside it.
	outer := report
	n := 0
	check := func(inst string, cs []ctlRec) {
		bus := inst == ""
		what := "the bus"
		report := outer
		if !bus {
			what = inst
			report = func(format string, a ...any) { outer(inst+": "+format, a...) }
		}
		n += len(cs)
		leaseUntil := -1.0 // when the current lease's term runs out; negative for none
		prevArmed := false
		prevHolder := ""
		for _, c := range cs {
			name := requestNames[c.request]

			// A lease past its term is the next thing the daemon records, and
			// promptly.
			if leaseUntil >= 0 && c.t >= leaseUntil {
				if c.request != rqLeaseExpired {
					report("a lease ran out at %.3fs, and the next decision, seq %d, a %s at %.3fs, is not its expiry",
						leaseUntil, c.seq, name, c.t)
				} else if c.t > leaseUntil+lag {
					report("a lease ran out at %.3fs and expired only at %.3fs", leaseUntil, c.t)
				}
			}
			if c.request == rqLeaseExpired && (leaseUntil < 0 || c.t < leaseUntil) {
				report("a lease expired at %.3fs, before any term had run out", c.t)
			}

			if c.verdict == vAccepted {
				switch c.request {
				case rqSend, rqCyclicSet, rqChannelSet:
					if !c.armed || !c.leased {
						report("seq %d: a %s accepted %s", c.seq, name, state(c))
					}
					if c.request == rqChannelSet {
						limit, has, stated := limitOf(r.meta, c.channel)
						switch {
						case !stated:
							report("seq %d: a write to %s, a channel the recording states no limit for", c.seq, c.channel)
						case has && c.value > limit:
							report("seq %d: %s set to %g, accepted above its limit of %g", c.seq, c.channel, c.value, limit)
						}
					}
					if c.request == rqCyclicSet && c.period < fl {
						report("seq %d: a cyclic period of %gs accepted under a floor of %v", c.seq, c.period, floor)
					}
				case rqCyclicDelete, rqArm, rqLeaseTake, rqLeaseRenew:
					if !c.leased {
						report("seq %d: a %s accepted, and nobody holds the lease after it", c.seq, name)
					}
				case rqDisarm, rqLeaseRelease, rqLeaseExpired, rqShutdown, rqStop:
					if c.armed {
						report("seq %d: %s is still armed after a %s", c.seq, what, name)
					}
				}
			} else {
				switch c.verdict {
				case vNotArmed:
					if c.armed {
						report("seq %d: a %s refused as not armed while the bus was armed", c.seq, name)
					}
				case vBelowFloor:
					if c.period >= fl {
						report("seq %d: a period of %gs refused under a floor of %v", c.seq, c.period, floor)
					}
				case vLeaseHeld:
					if !c.leased {
						report("seq %d: a lease refused as held by another while nobody held it", c.seq)
					}
				case vAboveLimit:
					if limit, has, _ := limitOf(r.meta, c.channel); !has || c.value <= limit {
						report("seq %d: %s set to %g, refused as above a limit the recording does not state", c.seq, c.channel, c.value)
					}
				}
			}

			// Over the network every request is by a name the recording lists,
			// does only what that name's role allows, and — when it needs the
			// lease — is made by the name that holds it.
			if c.via == viaTLS {
				role, known := roles[c.by]
				switch {
				case !known:
					report("seq %d: a %s over TLS by %q, a name the recording does not list", c.seq, name, c.by)
				case c.verdict == vAccepted && c.request != rqDisarm && c.request != rqStop && role != "operate":
					report("seq %d: a %s accepted from %q, whose role is %s", c.seq, name, c.by, role)
				case c.verdict == vForbidden && role == "operate":
					report("seq %d: a %s refused as not permitted to %q, whose role is operate", c.seq, name, c.by)
				}
				if c.verdict == vAccepted && needsLease[c.request] && c.by != prevHolder {
					report("seq %d: a %s over TLS by %q, under a lease held by %q", c.seq, name, c.by, prevHolder)
				}
			} else if c.verdict == vForbidden {
				report("seq %d: a %s refused as not permitted, through a listener that has no roles", c.seq, name)
			}

			// Everything running stops when the bus is disarmed, under the
			// disarm's own number.
			if bus && prevArmed && !c.armed {
				for _, task := range r.tasks {
					evs := r.cyclic[task]
					if !runningBefore(evs, c.t) {
						continue
					}
					stopped := false
					for _, e := range evs {
						if e.t >= c.t && e.t <= c.t+grace && !e.active && e.seq == c.seq &&
							(e.event == evDisarmed || e.event == evEnded) {
							stopped = true
							break
						}
					}
					if !stopped {
						report("%s: running when a %s disarmed the bus at %.3fs, and not stopped within %.0f ms",
							task, name, c.t, grace*1000)
					}
				}
			}

			switch {
			case c.verdict != vAccepted:
			case c.request == rqLeaseTake || c.request == rqLeaseRenew:
				leaseUntil = c.t + c.ttl
			case c.request == rqLeaseRelease || c.request == rqLeaseExpired || c.request == rqShutdown:
				leaseUntil = -1
			}
			if !bus && c.verdict == vAccepted && (c.request == rqDisarm || c.request == rqStop) {
				checkOff(r, inst, c, report)
			}
			prevArmed, prevHolder = c.armed, c.holder
		}
	}
	check("", r.control)
	for _, inst := range slices.Sorted(maps.Keys(r.controls)) {
		check(inst, r.controls[inst])
	}
	checkStops(r, report)
	checkSupplyWrites(r, report)
	checkScopeControl(r, report)
	checkPanelSaves(r, roles, report)
	n += len(r.panel)

	// And nothing runs while the bus is disarmed.
	for _, task := range r.tasks {
		for _, e := range r.cyclic[task] {
			if !e.active {
				continue
			}
			if s, ok := stateAt(e.t); !ok || !s.armed || !s.leased {
				report("%s: running at %.3fs while the bus was %s", task, e.t, state(s))
			}
		}
	}

	if len(bad) > 0 {
		if !verbose && len(bad) == 10 {
			bad = append(bad, "(more differences suppressed; -v shows them all)")
		}
		return 0, floor, errors.New(strings.Join(bad, "\n"))
	}
	return n, floor, nil
}

func sameFrame(a, b frame) bool {
	return a.id == b.id && a.extended == b.extended && bytes.Equal(a.data, b.data)
}

// generate writes a candump log with no daemon and no hardware involved.
//
// §12 is explicit that signal generation needs no injector: a ramp, a channel
// held for minutes and a burst are all offline text generation, and canplayer
// replays them. So this is a few lines rather than a tool to maintain.
func generate(w io.Writer, n int, seed int64, spacing float64, iface string) error {
	rng := rand.New(rand.NewSource(seed))
	bw := bufio.NewWriter(w)
	t := 1773480413.0 // the same constant epoch logb's example fixture uses
	for i := 0; i < n; i++ {
		t += spacing
		var id, data string
		switch {
		case i%2 == 0:
			// A ramp on EngineSpeed, so a decoded plot has something to show
			// and a signal has a shape a human can check by eye.
			rpm := 1300 + 500*math.Sin(float64(i)/200)
			v := uint16(rpm * 4) // the database's 0.25 rpm per bit
			b := []byte{byte(v), byte(v >> 8), 90, 40, 0, 0x20, 0, 0}
			id, data = "100", hex.EncodeToString(b)
		case i%5 == 1:
			b := make([]byte, 8)
			rng.Read(b)
			id, data = "200", hex.EncodeToString(b)
		case i%7 == 3:
			// An extended identifier, padded to eight digits because that is
			// how candump says extended and canplayer refuses anything else.
			b := make([]byte, 3)
			rng.Read(b)
			id, data = "01ABCDEF", hex.EncodeToString(b)
		default:
			// A one-byte payload, so the length field is exercised rather
			// than every frame being eight bytes.
			id, data = "300", hex.EncodeToString([]byte{byte(i)})
		}
		if _, err := fmt.Fprintf(bw, "(%.6f) %s %s#%s\n",
			t, iface, id, strings.ToUpper(data)); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "logbcheck:", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, strings.TrimLeft(`
logbcheck asserts a Logb recording against the candump log it was made from.

    logbcheck -gen 2000 > gen.log        write a log to replay
    logbcheck run.logb gen.log           check a recording against it

The whole loop, against a virtual bus:

    logbcheck -gen 2000 > gen.log
    canlogb -d 6s -o run.logb vcan0 &
    canplayer -I gen.log
    logbcheck run.logb gen.log

ktestd's transmit streams, with the log of what was sent as the reference:

    logbcheck -gen 300 > sent.log
    candump -L vcan0 > ref.log &
    ktestd -sock k.sock -d 8s -o run.logb vcan0 &
    # take the lease, arm, then POST each line of sent.log to
    # http://ktest/bus/vcan0/send over k.sock with the lease's token
    logbcheck -tx sent.log run.logb ref.log

its cyclic tasks, against the bus as candump saw it:

    logbcheck -cyclic run.logb ref.log

and its control plane, against the limits the recording declares — the
rack's panel saves among its decisions:

    logbcheck -control run.logb ref.log

Flags:
`, "\n"))
	flag.PrintDefaults()
	os.Exit(2)
}
