package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/logb"

	"github.com/trukeio/ktest/ndjson"
)

// Item is one record the DUT rack streamed, with both racks' times.
type Item struct {
	// Schema is the DUT rack's stream, as its subscription stream declares it.
	Schema *logb.Schema
	// Rec is the record, in the DUT rack's layout: the axis first.
	Rec []byte
	// PeerT is the record's own time on the DUT rack's clock, Unix ns: its
	// axis plus the stream's recording.epoch.
	PeerT int64
	// Recv is when this rack read it, by its own clock.
	Recv time.Time
	// Restated is a held value the DUT rack restated rather than wrote: the
	// state of a stream when the subscription joined it, before any record of
	// it. PeerT is then when the value was last written.
	Restated bool
}

// Stream is a subscription to a DUT rack, in either encoding: r reads Logb,
// nd NDJSON, and the other is nil.
type Stream struct {
	body  io.ReadCloser
	r     *logb.Reader
	nd    *ndjson.Reader
	epoch int64
	queue []Item
	seen  map[string]bool // streams with a record already handed over
}

// Subscribe opens /stream/sub for the streams globs name. ctx bounds the
// whole subscription.
func (c *Client) Subscribe(ctx context.Context, globs []string) (*Stream, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		c.cfg.URL+"/stream/sub?ch="+url.QueryEscape(strings.Join(globs, ",")), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	nd := c.cfg.Encoding == EncodingNDJSON
	if nd {
		req.Header.Set("Accept", ndjson.MediaType)
	} else {
		req.Header.Set("Accept", "application/vnd.logb")
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		msg := strings.TrimSpace(string(b))
		if resp.StatusCode == http.StatusNotAcceptable && nd {
			msg = "the DUT rack does not serve its stream as NDJSON (§7.1); read it as Logb: " + msg
		}
		return nil, &RefusedError{Status: resp.StatusCode, Msg: msg}
	}
	s := &Stream{body: resp.Body, epoch: -1, seen: map[string]bool{}}
	if nd {
		s.nd = ndjson.NewReader(resp.Body)
		return s, nil
	}
	s.r, err = logb.NewReader(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("the subscription is not Logb: %v", err)
	}
	s.r.OnHold = s.hold
	return s, nil
}

// Close ends the subscription.
func (s *Stream) Close() error { return s.body.Close() }

// Meta is the stream's metadata as read so far: the DUT rack's provenance.
func (s *Stream) Meta() []logb.Meta {
	if s.nd != nil {
		return s.nd.Meta
	}
	return s.r.Meta
}

// Next is the next record, blocking until there is one or the stream ends.
func (s *Stream) Next() (Item, error) {
	if s.nd != nil {
		return s.nextNDJSON()
	}
	for len(s.queue) == 0 {
		b, err := s.r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // a subscription never ends of its own accord
			}
			return Item{}, err
		}
		recv := time.Now()
		if err := s.needEpoch(); err != nil {
			return Item{}, err
		}
		for i := 0; i < int(b.Count); i++ {
			rec, err := b.Record(i)
			if err != nil {
				return Item{}, err
			}
			t, err := s.peerTime(b, i)
			if err != nil {
				return Item{}, err
			}
			s.queue = append(s.queue, Item{Schema: b.Schema, Rec: append([]byte(nil), rec...), PeerT: t, Recv: recv})
			s.seen[b.Schema.Name] = true
		}
	}
	it := s.queue[0]
	s.queue = s.queue[1:]
	return it, nil
}

// nextNDJSON is Next for an NDJSON subscription: the same Items, built from
// lines. A hold is handed over as a Logb restatement is, once per stream and
// only before its first record.
func (s *Stream) nextNDJSON() (Item, error) {
	for {
		m, err := s.nd.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF // a subscription never ends of its own accord
			}
			return Item{}, err
		}
		if m.Kind != ndjson.KindHold && m.Kind != ndjson.KindRecord {
			continue
		}
		if err := s.needEpoch(); err != nil {
			return Item{}, err
		}
		name := m.Schema.Name
		if m.Kind == ndjson.KindHold && s.seen[name] {
			continue
		}
		s.seen[name] = true
		return Item{Schema: m.Schema, Rec: m.Rec, PeerT: s.epoch + m.T, Recv: time.Now(),
			Restated: m.Kind == ndjson.KindHold}, nil
	}
}

func (s *Stream) needEpoch() error {
	if s.epoch >= 0 {
		return nil
	}
	for _, kv := range s.Meta() {
		if kv.Key == "peer.protocol" {
			v, err := strconv.Atoi(kv.Value)
			if err != nil {
				return fmt.Errorf("peer.protocol %q: %v", kv.Value, err)
			}
			if err := supported(v); err != nil {
				return err
			}
		}
	}
	for _, kv := range s.Meta() {
		if kv.Key == "recording.epoch" {
			e, err := strconv.ParseInt(kv.Value, 10, 64)
			if err != nil {
				return fmt.Errorf("recording.epoch %q: %v", kv.Value, err)
			}
			s.epoch = e
			return nil
		}
	}
	return errors.New("the DUT rack's stream does not state recording.epoch, so its times cannot be read")
}

// peerTime is record i's time on the DUT rack's clock: its axis, base and
// offset, in the stream's ticks, from the epoch.
func (s *Stream) peerTime(b *logb.Batch, i int) (int64, error) {
	if b.Schema.AxisKind != logb.AxisTime {
		return 0, fmt.Errorf("%s: no time axis", b.Schema.Name)
	}
	ax, err := b.Axis(i)
	if err != nil {
		return 0, err
	}
	d, err := ax.Duration(b.Schema.AxisExp)
	if err != nil {
		return 0, err
	}
	return s.epoch + int64(d), nil
}

// hold turns the held values restated for a stream nothing has been read of
// yet into a record of it: the state the stream was in when the subscription
// joined. Later restatements say nothing new, and are passed over.
//
// A stream restates its held values when a segment begins, so a subscription
// that joins mid-segment learns a held stream's state at the next boundary,
// or from its next record if that comes first.
func (s *Stream) hold(h *logb.Hold) {
	if s.seen[h.Schema.Name] || s.needEpoch() != nil {
		return
	}
	s.seen[h.Schema.Name] = true
	rec := make([]byte, h.Schema.RecordBytes())
	for f := range h.Schema.Fields {
		if f == int(h.Schema.AxisField) || !h.Has(f) {
			continue
		}
		v, err := h.Raw(f)
		if err != nil {
			continue
		}
		put(rec, &h.Schema.Fields[f], v)
	}
	d, err := h.AxisBase.Duration(h.Schema.AxisExp)
	if err != nil {
		return
	}
	putBits(rec, 0, 64, uint64(int64(d)))
	s.queue = append(s.queue, Item{Schema: h.Schema, Rec: rec, PeerT: s.epoch + int64(d), Recv: time.Now(), Restated: true})
}

// put writes a field's raw value into a record, little-endian as the DUT
// rack's own streams are laid out. A big-endian field is left as zero: no
// held stream of a rack has one.
func put(rec []byte, f *logb.Field, v any) {
	if f.BigEndian {
		return
	}
	switch x := v.(type) {
	case string:
		copy(rec[f.BitOffset/8:(f.BitOffset+f.BitWidth)/8], x)
	case []byte:
		copy(rec[f.BitOffset/8:(f.BitOffset+f.BitWidth)/8], x)
	case bool:
		if x {
			putBits(rec, f.BitOffset, 1, 1)
		}
	case uint64:
		putBits(rec, f.BitOffset, f.BitWidth, x)
	case int64:
		putBits(rec, f.BitOffset, f.BitWidth, uint64(x))
	case float64:
		if f.BitWidth == 32 {
			putBits(rec, f.BitOffset, 32, uint64(math.Float32bits(float32(x))))
		} else {
			putBits(rec, f.BitOffset, 64, math.Float64bits(x))
		}
	}
}

func putBits(rec []byte, off, width uint32, v uint64) {
	for i := uint32(0); i < width; i++ {
		bit := off + i
		if int(bit/8) >= len(rec) {
			return
		}
		if v>>i&1 != 0 {
			rec[bit/8] |= 1 << (bit % 8)
		} else {
			rec[bit/8] &^= 1 << (bit % 8)
		}
	}
}
