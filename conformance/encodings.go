package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/ndjson"
	"github.com/trukeio/ktest/peer"
)

const logbType = "application/vnd.logb"

// encodings checks §7.1's choice of encoding, and, when the rack serves
// NDJSON, the encoding itself (§7.5, §7.8) and its equivalence with Logb
// (§7.9): a Logb and an NDJSON subscription read side by side must carry
// the same metadata, schemas and records.
func (c *secChecker) encodings(ctx context.Context) {
	for _, x := range []struct{ accept, title string }{
		{"", "a subscription with no Accept is Logb"},
		{logbType, "a subscription that asks for Logb gets it"},
	} {
		code, ct, err := c.streamHead(ctx, x.accept)
		if err == nil && (code != http.StatusOK || ct != logbType) {
			err = fmt.Errorf("answered %d, %q", code, ct)
		}
		c.r.check("§7.1", x.title, err)
	}
	code, ct, err := c.streamHead(ctx, "text/html")
	if err == nil && code != http.StatusNotAcceptable {
		err = fmt.Errorf("answered %d, %q", code, ct)
	}
	c.r.check("§7.1", "a subscription in an encoding it does not serve is refused 406", err)

	const served = "the stream is served as NDJSON (SHOULD)"
	code, ct, err = c.streamHead(ctx, ndjson.MediaType)
	switch {
	case err != nil:
		c.r.fail("§7.1", served, "%v", err)
		return
	case code == http.StatusNotAcceptable:
		c.r.info("§7.1", served, "not served: refused 406, as it must be when it is not")
		for _, t := range ndjsonTitles {
			c.r.skip(t[0], t[1], "the rack does not serve NDJSON")
		}
		return
	case code != http.StatusOK || ct != ndjson.MediaType:
		c.r.fail("§7.1", served, "answered %d, %q", code, ct)
		for _, t := range ndjsonTitles {
			c.r.skip(t[0], t[1], "the rack did not answer NDJSON when asked for it")
		}
		return
	}
	c.r.pass("§7.1", served, "")

	lines, items, lbMeta, err := c.sideBySide(ctx)
	if err != nil {
		c.r.fail("§7.9", "Logb and NDJSON subscriptions read side by side", "%v", err)
		for _, t := range ndjsonTitles {
			c.r.skip(t[0], t[1], "no subscriptions to compare")
		}
		return
	}
	c.ndjsonLines(lines, items, lbMeta)
}

// The checks of an NDJSON stream, as the report names them.
var ndjsonTitles = [][2]string{
	{"§7.8", "every NDJSON line is one JSON object"},
	{"§7.5", "the NDJSON stream opens with the metadata, every value a string"},
	{"§7.5", "a stream's schema comes before its holds and records"},
	{"§7.8", "integers wider than 53 bits, t among them, are decimal strings; labelled enums are labels"},
	{"§7.9", "the metadata is the same in both encodings"},
	{"§7.9", "the schemas are the same in both encodings"},
	{"§7.9", "the records are the same in both encodings, to the nanosecond"},
}

// streamHead asks for a subscription with accept and returns its status
// and media type, without reading the stream.
func (c *secChecker) streamHead(ctx context.Context, accept string) (int, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+"/stream/sub?ch=mon.watermark", nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.raw.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return resp.StatusCode, mt, nil
}

// sideBySide reads an NDJSON subscription, line by line as it was sent,
// and a Logb one through package peer, for the same streams at the same
// time, for a second and a half.
func (c *secChecker) sideBySide(ctx context.Context) ([][]byte, []peer.Item, []logb.Meta, error) {
	globs := "mon.*,dut.action.*"
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	lb, err := c.op.Subscribe(ctx, strings.Split(globs, ","))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the Logb subscription: %v", err)
	}
	defer lb.Close()
	req, err := http.NewRequestWithContext(ctx, "GET", c.cfg.URL+"/stream/sub?ch="+globs, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Accept", ndjson.MediaType)
	resp, err := c.raw.Do(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the NDJSON subscription: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, fmt.Errorf("the NDJSON subscription: answered %d", resp.StatusCode)
	}

	var (
		mu    sync.Mutex
		items []peer.Item
		lines [][]byte
		wg    sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			it, err := lb.Next()
			if err != nil {
				return
			}
			mu.Lock()
			items = append(items, it)
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		br := bufio.NewReaderSize(resp.Body, 64*1024)
		for {
			l, err := br.ReadBytes('\n')
			if err != nil {
				return // a line cut by the end of the read is not one
			}
			mu.Lock()
			lines = append(lines, l)
			mu.Unlock()
		}
	}()
	<-ctx.Done()
	lb.Close()
	resp.Body.Close()
	wg.Wait()
	if len(lines) == 0 {
		return nil, nil, nil, errors.New("the NDJSON subscription sent no line in 1.5 s")
	}
	return lines, items, lb.Meta(), nil
}

// field is a schema message's field, as §7.8 writes it.
type field struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Bits   uint32            `json:"bits,omitempty"`
	Max    uint32            `json:"max,omitempty"`
	Unit   string            `json:"unit,omitempty"`
	Held   bool              `json:"held,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Guard  *struct {
		Field string `json:"field"`
		Value uint64 `json:"value"`
	} `json:"guard,omitempty"`
}

type schemaMsg struct {
	Schema string  `json:"schema"`
	Fields []field `json:"fields"`
}

func (c *secChecker) ndjsonLines(lines [][]byte, items []peer.Item, lbMeta []logb.Meta) {
	// §7.8: one object a line.
	objs := make([]map[string]json.RawMessage, 0, len(lines))
	objLines := make([][]byte, 0, len(lines))
	var bad string
	for i, l := range lines {
		var o map[string]json.RawMessage
		if err := json.Unmarshal(l, &o); err != nil {
			if bad == "" {
				bad = fmt.Sprintf("line %d: %v: %.80s", i+1, err, l)
			}
			continue
		}
		objs = append(objs, o)
		objLines = append(objLines, l)
	}
	if bad != "" {
		c.r.fail("§7.8", ndjsonTitles[0][1], "%s", bad)
	} else {
		c.r.pass("§7.8", ndjsonTitles[0][1], "%d lines", len(lines))
	}

	// §7.5: meta first, its values strings, the keys §7.3 requires.
	meta := map[string]string{}
	func() {
		title := ndjsonTitles[1][1]
		if len(objs) == 0 || objs[0]["meta"] == nil || isValues(objs[0]) {
			c.r.fail("§7.5", title, "the first line is not a meta message")
			return
		}
		for i, o := range objs {
			if o["meta"] == nil || isValues(o) || o["schema"] != nil {
				continue
			}
			var kv map[string]string
			if err := json.Unmarshal(o["meta"], &kv); err != nil {
				c.r.fail("§7.5", title, "meta message %d: %v", i+1, err)
				return
			}
			for k, v := range kv {
				if _, ok := meta[k]; !ok {
					meta[k] = v
				}
			}
		}
		if meta["recording.epoch"] == "" || meta["peer.protocol"] != strconv.Itoa(peer.Protocol) {
			c.r.fail("§7.5", title, "recording.epoch %q, peer.protocol %q", meta["recording.epoch"], meta["peer.protocol"])
			return
		}
		c.r.pass("§7.5", title, "")
	}()

	// §7.5: schemas first; §7.8: wide integers as strings, enums as labels.
	schemas := map[string]schemaMsg{}
	var order, wide string
	n := 0
	for i, o := range objs {
		switch {
		case o["schema"] != nil && !isValues(o):
			var s schemaMsg
			if err := json.Unmarshal(objLines[i], &s); err == nil {
				schemas[s.Schema] = s
			}
		case isValues(o):
			kind, name := "rec", ""
			if o["hold"] != nil {
				kind = "hold"
			}
			json.Unmarshal(o[kind], &name)
			s, ok := schemas[name]
			if !ok {
				if order == "" {
					order = fmt.Sprintf("a %s of %s before its schema", kind, name)
				}
				continue
			}
			n++
			if wide == "" {
				wide = wideInts(name, s, o)
			}
		}
	}
	if order != "" {
		c.r.fail("§7.5", ndjsonTitles[2][1], "%s", order)
	} else {
		c.r.pass("§7.5", ndjsonTitles[2][1], "%d streams", len(schemas))
	}
	if wide != "" {
		c.r.fail("§7.8", ndjsonTitles[3][1], "%s", wide)
	} else {
		c.r.pass("§7.8", ndjsonTitles[3][1], "%d holds and records", n)
	}

	c.equivalent(lines, items, lbMeta, meta, schemas)
}

// isValues says whether an object is a hold or a record: a schema message
// has a "meta" of its own, and is not one.
func isValues(o map[string]json.RawMessage) bool { return o["rec"] != nil || o["hold"] != nil }

// wideInts is what is wrong with how a hold or record writes its numbers,
// or "".
func wideInts(name string, s schemaMsg, o map[string]json.RawMessage) string {
	if !isDecimalString(o["t"]) {
		return fmt.Sprintf("%s: t is %s, not a decimal string", name, o["t"])
	}
	var v map[string]json.RawMessage
	json.Unmarshal(o["v"], &v)
	for _, f := range s.Fields {
		raw, ok := v[f.Name]
		if !ok {
			continue
		}
		switch f.Type {
		case "uint", "sint":
			if f.Bits > 53 && !isDecimalString(raw) {
				return fmt.Sprintf("%s.%s, %d bits: %s, not a decimal string", name, f.Name, f.Bits, raw)
			}
			if f.Bits <= 53 && (len(raw) == 0 || raw[0] == '"') {
				return fmt.Sprintf("%s.%s, %d bits: %s, not a number", name, f.Name, f.Bits, raw)
			}
		case "enum":
			if len(raw) > 0 && raw[0] != '"' {
				if _, labelled := f.Labels[string(raw)]; labelled {
					return fmt.Sprintf("%s.%s: code %s, which has the label %q", name, f.Name, raw, f.Labels[string(raw)])
				}
			}
		}
	}
	return ""
}

func isDecimalString(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return false
	}
	_, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		_, err = strconv.ParseUint(s, 10, 64)
	}
	return err == nil
}

// equivalent holds the NDJSON subscription to the Logb one read beside it
// (§7.9).
func (c *secChecker) equivalent(lines [][]byte, items []peer.Item, lbKV []logb.Meta, meta map[string]string,
	schemas map[string]schemaMsg) {
	lbSchemas := map[string]*logb.Schema{}
	for _, it := range items {
		lbSchemas[it.Schema.Name] = it.Schema
	}
	// The metadata: every key the Logb states, with the value it first had.
	lbMeta := map[string]string{}
	for _, kv := range lbKV {
		if _, ok := lbMeta[kv.Key]; !ok {
			lbMeta[kv.Key] = kv.Value
		}
	}
	var diff []string
	for _, k := range slices.Sorted(mapsKeys(lbMeta)) {
		if meta[k] != lbMeta[k] {
			diff = append(diff, fmt.Sprintf("%s: %q in NDJSON, %q in Logb", k, meta[k], lbMeta[k]))
		}
	}
	if len(diff) > 0 {
		c.r.fail("§7.9", ndjsonTitles[4][1], "%s", strings.Join(diff, "; "))
	} else {
		c.r.pass("§7.9", ndjsonTitles[4][1], "%d keys", len(lbMeta))
	}

	// The schemas: what each Logb schema is in NDJSON, field by field.
	diff = nil
	compared := 0
	for _, name := range slices.Sorted(mapsKeys(lbSchemas)) {
		got, ok := schemas[name]
		if !ok {
			diff = append(diff, name+": no NDJSON schema")
			continue
		}
		var buf bytes.Buffer
		if err := ndjson.NewWriter(&buf).Schema(lbSchemas[name]); err != nil {
			diff = append(diff, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		var want schemaMsg
		json.Unmarshal(buf.Bytes(), &want)
		compared++
		if d := sameFields(want.Fields, got.Fields); d != "" {
			diff = append(diff, name+": "+d)
		}
	}
	if len(diff) > 0 {
		c.r.fail("§7.9", ndjsonTitles[5][1], "%s", strings.Join(diff, "; "))
	} else {
		c.r.pass("§7.9", ndjsonTitles[5][1], "%d streams", compared)
	}

	c.sameRecords(lines, items)
}

func sameFields(want, got []field) string {
	byName := map[string]field{}
	for _, f := range got {
		byName[f.Name] = f
	}
	for _, w := range want {
		g, ok := byName[w.Name]
		if !ok {
			return "no field " + w.Name
		}
		wj, _ := json.Marshal(w)
		gj, _ := json.Marshal(g)
		if !bytes.Equal(wj, gj) {
			return fmt.Sprintf("%s is %s, want %s", w.Name, gj, wj)
		}
	}
	if len(got) != len(want) {
		return fmt.Sprintf("%d fields, want %d", len(got), len(want))
	}
	return ""
}

// sameRecords compares the records both subscriptions carried while both
// were open: from the later of their first watermarks to the earlier of
// their last, every record of one must be in the other, at the same time
// and with the same values.
func (c *secChecker) sameRecords(lines [][]byte, items []peer.Item) {
	title := ndjsonTitles[6][1]
	var lb, nd []rec
	for _, it := range items {
		if it.Restated {
			continue
		}
		lb = append(lb, recOf(it.Schema, it.Rec, it.PeerT, nil))
	}
	r := ndjson.NewReader(bytes.NewReader(bytes.Join(lines, nil)))
	epoch := int64(-1)
	for {
		m, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.r.fail("§7.9", title, "the NDJSON does not read back: %v", err)
			return
		}
		if epoch < 0 {
			for _, kv := range r.Meta {
				if kv.Key == "recording.epoch" {
					epoch, _ = strconv.ParseInt(kv.Value, 10, 64)
				}
			}
		}
		if m.Kind == ndjson.KindRecord {
			nd = append(nd, recOf(m.Schema, m.Rec, epoch+m.T, m.Present))
		}
	}
	lo, hi, ok := overlap(lb, nd)
	if !ok {
		c.r.fail("§7.9", title, "the two subscriptions have no watermarks in common to compare between")
		return
	}
	count := func(rs []rec) map[string]int {
		m := map[string]int{}
		for _, x := range rs {
			if x.t >= lo && x.t <= hi {
				m[x.key]++
			}
		}
		return m
	}
	a, b := count(lb), count(nd)
	var diff []string
	for _, k := range slices.Sorted(mapsKeys(a)) {
		if b[k] < a[k] {
			diff = append(diff, "only in Logb: "+k)
		}
	}
	for _, k := range slices.Sorted(mapsKeys(b)) {
		if a[k] < b[k] {
			diff = append(diff, "only in NDJSON: "+k)
		}
	}
	n := 0
	for _, v := range a {
		n += v
	}
	if len(diff) > 0 {
		if len(diff) > 3 {
			diff = append(diff[:3], fmt.Sprintf("and %d more", len(diff)-3))
		}
		c.r.fail("§7.9", title, "%s", strings.Join(diff, "; "))
		return
	}
	c.r.pass("§7.9", title, "%d records over %v", n, time.Duration(hi-lo).Round(time.Millisecond))
}

// rec is a record reduced to what both encodings must agree on: its stream,
// its time and its values by name, as one comparable key.
type rec struct {
	stream string
	t      int64
	key    string
}

func recOf(s *logb.Schema, data []byte, t int64, present []bool) rec {
	b := &logb.Batch{Schema: s, Count: 1, Data: data}
	var parts []string
	for f := range s.Fields {
		fd := &s.Fields[f]
		if f == int(s.AxisField) || fd.Type == logb.TypeComplex || fd.Variable {
			continue
		}
		if present != nil && !present[f] {
			continue
		}
		v, err := b.Raw(0, f)
		if errors.Is(err, logb.ErrFieldAbsent) {
			continue
		}
		if err != nil {
			continue
		}
		// What the messages carry (§7.7): a converted float for a field
		// under a numeric conversion.
		if _, numeric := fd.Conv.(logb.Linear); numeric {
			v, _ = b.Value(0, f)
		}
		switch x := v.(type) {
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				v = "null"
			}
		case []byte:
			v = fmt.Sprintf("%x", x)
		}
		parts = append(parts, fmt.Sprintf("%s=%v", fd.Name, v))
	}
	slices.Sort(parts)
	return rec{stream: s.Name, t: t, key: fmt.Sprintf("%s@%d{%s}", s.Name, t, strings.Join(parts, " "))}
}

// overlap is the span both subscriptions covered, by their watermarks.
func overlap(a, b []rec) (lo, hi int64, ok bool) {
	span := func(rs []rec) (int64, int64, bool) {
		first, last, any := int64(0), int64(0), false
		for _, r := range rs {
			if r.stream != "mon.watermark" {
				continue
			}
			if !any {
				first = r.t
			}
			last, any = r.t, true
		}
		return first, last, any
	}
	a0, a1, ok1 := span(a)
	b0, b1, ok2 := span(b)
	lo, hi = max(a0, b0), min(a1, b1)
	return lo, hi, ok1 && ok2 && lo < hi
}
