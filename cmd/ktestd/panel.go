//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rveen/logb"

	"github.com/rveen/ktest/record"
)

// A panel is a rack layout: widgets placed on a grid, each bound to channels
// by name (§10.1). It is data, not code — the browser builds the rack from it
// — which is what lets a recording carry the rack it was made with.
//
//	{
//	  "title": "Engine bench",
//	  "widgets": [
//	    {"type": "readout", "channel": "EngineData.EngineSpeed", "x": 0, "y": 0, "w": 3, "h": 2},
//	    {"type": "plot", "channels": ["EngineData.EngineSpeed"], "window": 10,
//	     "x": 3, "y": 0, "w": 9, "h": 4}
//	  ]
//	}
//
// A channel is a stream's name and a field's, as the recording has them. The
// grid is 12 columns unless "columns" says otherwise; x, y, w and h are in grid
// cells, the shape gridstack serialises, and every widget lies inside the grid
// and clear of every other.
//
// The rack edits its panel too. A save, PUT /panel, is checked exactly as the
// file is at start, written back to that file, embedded in the recording again
// and recorded in rack.panel; see savePanel.
type panel struct {
	Title   string   `json:"title"`
	Columns int      `json:"columns"`
	Widgets []widget `json:"widgets"`
}

type widget struct {
	Type     string   `json:"type"`
	Label    string   `json:"label"`
	Channel  string   `json:"channel"`
	Channels []string `json:"channels"`
	Window   float64  `json:"window"` // plot: seconds shown
	Step     float64  `json:"step"`   // setpoint: the input's step; 0 for any
	X        int      `json:"x"`
	Y        int      `json:"y"`
	W        int      `json:"w"`
	H        int      `json:"h"`
}

// loadPanel reads and checks a panel file, and returns it as given: the bytes
// the browser is served and the recording embeds are the file, not a
// re-encoding of it.
func loadPanel(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := checkPanel(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return raw, nil
}

// checkPanel is what the daemon can know of a panel before it is used — shape,
// types and placement; whether a channel exists is known only once its stream
// does, and the rack says so beside the widget. A file at start and a save
// from the rack are held to the same rules, by this.
func checkPanel(raw []byte) error {
	var p panel
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A misspelt key is refused rather than ignored: a widget that silently
	// lost its binding would show nothing, and look like a quiet channel.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("something follows the panel's closing brace")
	}
	if p.Columns < 0 || p.Columns > 48 {
		return fmt.Errorf("%d columns; a panel has 1 to 48, or 12 when unsaid", p.Columns)
	}
	cols := p.Columns
	if cols == 0 {
		cols = 12
	}
	for i, w := range p.Widgets {
		where := fmt.Sprintf("widget %d (%s)", i, w.Type)
		switch w.Type {
		case "readout":
			if w.Channel == "" || len(w.Channels) > 0 {
				return fmt.Errorf(`%s: a readout shows one "channel"`, where)
			}
		case "plot":
			if len(w.Channels) == 0 || w.Channel != "" {
				return fmt.Errorf(`%s: a plot draws "channels", a list`, where)
			}
			if w.Window < 0 {
				return fmt.Errorf("%s: a window of %g s", where, w.Window)
			}
		case "scope":
			// A scope draws one channel's acquisitions, so its channel is a
			// scope channel and not one of the accounts around it: scope1.ch1,
			// never scope1.ch1.env or scope1.ch1.vert.vdiv.set.
			if w.Channel == "" || len(w.Channels) > 0 || !scopeChannel.MatchString(w.Channel) {
				return fmt.Errorf(`%s: a scope draws one "channel", a scope channel such as scope1.ch1`, where)
			}
		case "setpoint", "toggle":
			// These write, so their channel must be one that can be written,
			// and of the right kind: a toggle switches something on or off, a
			// setpoint sets a number.
			cn, err := parseChannel(w.Channel)
			if err != nil || len(w.Channels) > 0 {
				return fmt.Errorf(`%s: a %s writes one "channel" that can be set, such as psu1.ch1.v.set`, where, w.Type)
			}
			if (w.Type == "toggle") != isSwitch(cn.what) {
				return fmt.Errorf("%s: a toggle switches something on or off, such as out.set or on.set, and a "+
					"setpoint sets a number; %s is neither", where, w.Channel)
			}
			if w.Step < 0 {
				return fmt.Errorf("%s: a step of %g", where, w.Step)
			}
		case "lamp":
			if w.Channel == "" || len(w.Channels) > 0 {
				return fmt.Errorf(`%s: a lamp shows one "channel"`, where)
			}
		default:
			return fmt.Errorf(`%s: no widget type %q; there are "readout", "plot", "scope", "setpoint", "toggle" and "lamp"`,
				where, w.Type)
		}
		if w.Step != 0 && w.Type != "setpoint" {
			return fmt.Errorf(`%s: only a setpoint has a "step"`, where)
		}
		if w.X < 0 || w.Y < 0 || w.W < 1 || w.H < 1 {
			return fmt.Errorf("%s: placed at %d,%d, %d by %d cells", where, w.X, w.Y, w.W, w.H)
		}
		// A widget off the grid or on top of another would be moved by the
		// layout to somewhere the file does not say, and a rack that is not
		// the panel it was given is the thing a check is for.
		if w.X+w.W > cols {
			return fmt.Errorf("%s: columns %d to %d of a grid of %d", where, w.X, w.X+w.W-1, cols)
		}
		for j, o := range p.Widgets[:i] {
			if w.X < o.X+o.W && o.X < w.X+w.W && w.Y < o.Y+o.H && o.Y < w.Y+w.H {
				return fmt.Errorf("%s overlaps widget %d (%s)", where, j, o.Type)
			}
		}
	}
	return nil
}

// panelState is the panel in force. The loop replaces it whole, and the
// handlers that serve it read it without a lock.
type panelState struct {
	raw []byte // nil when there is none
	sha string // hex sha256 of raw, "" when there is none
	rev uint32 // 1 for the file the daemon started with, one more for each save; 0 for none
	by  string // the name that saved it; "" for the file, or a save on the local socket
}

func newPanelState(raw []byte, rev uint32, by string) *panelState {
	p := &panelState{raw: raw, rev: rev, by: by}
	if raw != nil {
		sum := sha256.Sum256(raw)
		p.sha = hex.EncodeToString(sum[:])
	}
	return p
}

// servePanel is GET /panel: the panel in force, with its sha256 as its ETag,
// or 404 without one.
func (d *daemon) servePanel(w http.ResponseWriter, r *http.Request) {
	p := d.panel.Load()
	if p.raw == nil {
		httpError(w, http.StatusNotFound, 0, "no panel: start the daemon with -panel")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", strconv.Quote(p.sha))
	w.Write(p.raw)
}

// panelMax is the largest panel a save may send. A panel of a hundred widgets
// is some ten kilobytes.
const panelMax = 1 << 20

// putPanel is PUT /panel: a panel, to be the one in force.
//
// The save names the panel it was edited from, as If-Match with the sha256
// GET /panel gave as its ETag, so that of two people editing at once the
// second is told rather than silently overwriting the first. The answer is a
// sequence number or a refusal, as for any other request (§6); the panel then
// arrives on the stream like everything else.
func (d *daemon) putPanel(w http.ResponseWriter, r *http.Request) {
	if d.panelPath == "" {
		// Nothing to save to is not a decision; it is answered without a number.
		httpError(w, http.StatusConflict, 0, "this daemon has no panel file to save to: start it with -panel FILE, "+
			`where FILE may hold no more than {"widgets":[]}`)
		return
	}
	match := r.Header.Get("If-Match")
	if match == "" {
		httpError(w, http.StatusPreconditionRequired, 0,
			`a save names the panel it was edited from: If-Match: "SHA256", the ETag GET /panel answers with`)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, panelMax))
	if err != nil {
		httpError(w, http.StatusBadRequest, 0, "%v", err)
		return
	}
	if err := checkPanel(raw); err != nil {
		httpError(w, http.StatusBadRequest, 0, "panel: %v", err)
		return
	}
	d.submit(w, r, &request{kind: reqPanelSave, panel: raw, match: strings.Trim(match, `"`)})
}

var (
	errStale     = errors.New("the panel has changed")
	errPanelFile = errors.New("the panel file could not be written, and the panel in force is unchanged")
)

// A panel save's verdict, as rack.panel records it. Where a refusal has a
// number in the control streams it has the same one here.
const (
	panelSaved     = 0
	panelForbidden = uint8(record.VerdictForbidden)
	panelStale     = 7
	panelUnwritten = 8
)

// savePanel is the loop's decision on a save, and the save itself.
//
// The file is written first and the recording told after, so that a save the
// disk refused leaves the recording saying so rather than naming a panel that
// no restart would bring back. The file is replaced whole, by a rename, so a
// daemon that dies mid-save leaves the old panel or the new one and never
// half of each.
func (l *loop) savePanel(r *request, t time.Time) reply {
	cur := l.d.panel.Load()
	var v uint8
	var err error
	switch {
	case r.who.role != roleOperate:
		v, err = panelForbidden, fmt.Errorf("%w: %q has the view role, which may read the streams and disarm, and nothing more",
			errForbidden, r.who.name)
	case r.match != cur.sha:
		v, err = panelStale, fmt.Errorf("%w: it was edited from %.12s…, and revision %d, %.12s…, has been saved since",
			errStale, r.match, cur.rev, cur.sha)
	default:
		if werr := replaceFile(l.d.panelPath, r.panel); werr != nil {
			v, err = panelUnwritten, fmt.Errorf("%w: %v", errPanelFile, werr)
		}
	}
	if err == nil {
		cur = newPanelState(r.panel, cur.rev+1, r.who.name)
		// The same name as at start: a reader keeps the last attachment of a
		// name, so a recording ends with the panel that was in force at its
		// end, and a stream joined late ends its preamble with the one in
		// force now (stream.Hub replays every attachment, in order).
		if err := l.w.Attach("panel.json", cur.raw); err != nil {
			fatalf("write: %v", err)
		}
		l.d.panel.Store(cur)
	}
	l.panelRecord(t, l.seq, v, r, cur)
	return reply{seq: l.seq, err: err}
}

// replaceFile writes data to path by way of a file beside it, synced, and a
// rename, keeping the permissions path had.
func replaceFile(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// The panel stream's record layout, in bytes: t 0–7, seq 8–11, verdict 12,
// via 13, revision 16–19, sha256 20–83, saved_by and by a name each.
const (
	pnSeq     = 8
	pnVerdict = 12
	pnVia     = 13
	pnRev     = 16
	pnSHA     = 20
	pnSavedBy = pnSHA + 64
	pnBy      = pnSavedBy + record.HolderMax
	pnBytes   = pnBy + record.HolderMax
)

// panelSchema is rack.panel: every save of the rack's panel, refusals
// included, with the panel in force after it. The panel itself is the
// recording's panel.json attachment; the stream says which one was in force
// when, and its held fields restate that at every segment, so a reader
// joining anywhere knows which rack it is looking at.
func panelSchema() *logb.Schema {
	str := func(name string, at, n uint32, held bool, desc string) logb.Field {
		return logb.Field{Name: name, BitOffset: 8 * at, BitWidth: 8 * n, Type: logb.TypeString, Hold: held, Desc: desc}
	}
	return record.Schema("rack.panel", 8*pnBytes, []logb.Field{
		{Name: "seq", BitOffset: 8 * pnSeq, BitWidth: 32, Type: logb.TypeUint,
			Desc: "the save this record answers; 0 for the panel the daemon started with"},
		{Name: "verdict", BitOffset: 8 * pnVerdict, BitWidth: 8, Type: logb.TypeUint,
			Desc: "0 saved; refused: 5 the caller's role does not permit it, " +
				"7 it was edited from a panel no longer in force, 8 the panel file could not be written"},
		{Name: "via", BitOffset: 8 * pnVia, BitWidth: 8, Type: logb.TypeUint,
			Desc: "0 the daemon, at start; 1 the local socket; 2 the network listener, TLS with a named token"},
		{Name: "revision", BitOffset: 8 * pnRev, BitWidth: 32, Type: logb.TypeUint, Hold: true,
			Desc: "the panel in force: 1 the file the daemon started with, one more for each save; 0 for none"},
		str("sha256", pnSHA, 64, true, "the sha256 of the panel in force, the panel.json attachment; empty for none"),
		str("saved_by", pnSavedBy, record.HolderMax, true,
			"the name that saved the panel in force; empty for the file the daemon started with, and for a save on the local socket"),
		str("by", pnBy, record.HolderMax, false, "the name that asked; empty on the local socket and at start"),
	}, map[string]string{
		"source":     "the ktestd control plane: each save of the rack's panel, refusals included",
		"clock":      "userspace: the daemon's clock when it decided",
		"attachment": "panel.json",
	})
}

// panelRecord writes one save, or the panel the daemon started with when r is
// nil, with p the panel in force after it.
func (l *loop) panelRecord(t time.Time, seq uint32, v uint8, r *request, p *panelState) {
	err := l.w.Write(t, l.panel, func(rec []byte) {
		le := binary.LittleEndian
		le.PutUint32(rec[pnSeq:], seq)
		rec[pnVerdict] = v
		if r != nil {
			rec[pnVia] = byte(r.who.via)
			copy(rec[pnBy:pnBy+record.HolderMax], r.who.name)
		}
		le.PutUint32(rec[pnRev:], p.rev)
		copy(rec[pnSHA:pnSHA+64], p.sha)
		copy(rec[pnSavedBy:pnSavedBy+record.HolderMax], p.by)
	})
	if err != nil {
		fatalf("write: %v", err)
	}
}
