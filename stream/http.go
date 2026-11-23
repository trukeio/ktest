package stream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/trukeio/ktest/ndjson"
)

// ContentType is what a Logb stream is served as. It is not registered with
// IANA; nothing dispatches on it but us, and it exists so that a consumer can
// tell it apart from an error page.
const ContentType = "application/vnd.logb"

// ServeHTTP streams Logb to one consumer, for as long as the recording lasts
// or the consumer keeps up.
//
// The response is an ordinary chunked HTTP body, which is the whole of §6.1's
// argument: control goes in over REST and results come back only on the stream,
// so the data connection is one-way and a one-way byte stream is a response
// body. What that buys over a WebSocket is that
//
//	curl http://rack/stream/live > run.logb
//
// is a recording rather than a transcript of one.
//
// Backpressure is why every chunk is flushed rather than buffered: a slow
// client stalls the write, TCP says so, and the stall reaches the Hub as a
// consumer that stops calling Done. The Hub then disconnects it rather than
// letting it hold the recording up, which is the policy §10.5 requires and the
// reason this handler does not need a queue of its own.
//
// # HTTP/2
//
// Over ktestd's TLS listener this handler is served as HTTP/2, since Go
// negotiates it by default, and it is left that way on a measurement rather
// than the documentation: with vcan flooded by cangen, three consumers at once
// — HTTP/2 and HTTP/1.1 over TLS, and plain HTTP/1.1 — each received the whole
// 51.6 MB stream at the 6.45 MB/s the recorder produced, and the Hub dropped
// none. That is the producer's ceiling, not the transport's, and the clients
// were curl, whose receive windows are large; a client with a small h2 window
// is the case that would bite, and the one to measure if a consumer is ever
// seen falling behind.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.ServeStreams(w, r, nil) }

// ServeStreams is ServeHTTP for a consumer sent only the streams keep accepts,
// by name: see SubscribeStreams.
//
// The consumer's Accept header chooses the encoding (doc/peer-protocol.md
// §7.1): Logb, the frames as they are, or NDJSON, the same frames transcoded
// message by message as they arrive. A consumer that accepts neither is
// refused 406.
func (h *Hub) ServeStreams(w http.ResponseWriter, r *http.Request, keep func(stream string) bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "the server cannot stream to this connection", http.StatusInternalServerError)
		return
	}
	enc := Negotiate(r.Header.Values("Accept"))
	if enc == "" {
		http.Error(w, "this stream is served as "+ContentType+" or "+ndjson.MediaType, http.StatusNotAcceptable)
		return
	}

	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		http.Error(w, "the recording has ended", http.StatusGone)
		return
	}

	// No Content-Length, so the response is chunked and open-ended.
	w.Header().Set("Content-Type", enc)
	w.Header().Add("Vary", "Accept")
	w.Header().Set("Cache-Control", "no-store")
	// A reverse proxy will buffer a chunked response into uselessness unless
	// told not to (§6.1). This is the header nginx honours; others need their
	// own configuration, which is a deployment matter rather than a code one.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	sub := h.SubscribeStreams(0, keep)
	defer sub.Cancel()

	// For NDJSON the chunks go through a pipe to a transcoder, which writes
	// and flushes a line per message. The pipe has no buffer, so a slow
	// consumer still stalls the write below, and the Hub still sees it.
	out := io.Writer(w)
	if enc == ndjson.MediaType {
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			nw := ndjson.NewWriter(w)
			nw.Flush = fl.Flush
			pr.CloseWithError(ndjson.Transcode(nw, pr))
		}()
		defer func() { pw.Close(); <-done }()
		out = pw
	}

	ctx := r.Context()
	for {
		select {
		case b, ok := <-sub.Chunks():
			if !ok {
				return
			}
			_, err := out.Write(b)
			// Released whether or not the write succeeded: on failure this
			// consumer is going away, and holding its allowance would only
			// make the Hub disconnect it for a reason that is not true.
			sub.Done(len(b))
			if err != nil {
				return
			}
			if out == w {
				fl.Flush()
			}
		case <-ctx.Done():
			return
		}
	}
}

// Negotiate is the encoding an Accept header asks for, among Logb and NDJSON
// (doc/peer-protocol.md §7.1), or "" if it accepts neither.
//
// The higher q wins. At equal q a type named outright beats one a wildcard
// matches, so "application/x-ndjson, */*" is NDJSON; then Logb, the default.
// No Accept at all is Logb, so every client written before NDJSON existed
// gets what it always got.
func Negotiate(accept []string) string {
	offered := []string{ContentType, ndjson.MediaType}
	type score struct {
		q    float64
		spec int // 0 none, 1 */*, 2 type/*, 3 the type itself
	}
	best := map[string]score{}
	named := false
	for _, line := range accept {
		for _, part := range strings.Split(line, ",") {
			mt, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			mt = strings.ToLower(strings.TrimSpace(mt))
			if mt == "" {
				continue
			}
			named = true
			q := 1.0
			for _, p := range strings.Split(params, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = f
					}
				}
			}
			for _, o := range offered {
				spec := 0
				switch {
				case mt == o:
					spec = 3
				case mt == "*/*":
					spec = 1
				case strings.HasSuffix(mt, "/*") && strings.HasPrefix(o, strings.TrimSuffix(mt, "*")):
					spec = 2
				}
				// The most specific range that matches a type sets its q.
				if spec > 0 && spec > best[o].spec {
					best[o] = score{q, spec}
				}
			}
		}
	}
	if !named {
		return ContentType
	}
	pick, top := "", score{}
	for _, o := range offered {
		s, ok := best[o]
		if !ok || s.q <= 0 {
			continue
		}
		if pick == "" || s.q > top.q || (s.q == top.q && s.spec > top.spec) {
			pick, top = o, s
		}
	}
	return pick
}

// StatusHandler reports the fan-out's accounting as JSON.
//
// It exists because the two things this package can do wrong are invisible from
// outside: a consumer silently disconnected for falling behind, and a stream
// nobody is reading. Both are counted, so both can be looked at.
func (h *Hub) StatusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := h.Stats()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"frames":      st.Frames,
			"bytes":       st.Bytes,
			"subscribers": st.Subscribers,
			"joined":      st.Joined,
			"dropped":     st.Dropped,
		})
	})
}

// Mux returns the endpoints of §6.1 that exist today.
//
// /stream/replay/{id} is not among them: replay is serving a finished file,
// and there is no run registry to name one by yet. It arrives with the control
// plane, which is milestone 2.
func (h *Hub) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /stream/live", h)
	mux.Handle("GET /stream/status", h.StatusHandler())
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "ktest\n\n  GET /stream/live     the recording, live, as Logb\n"+
			"  GET /stream/status   what the fan-out is doing\n\n"+
			"  curl -N %s/stream/live > run.logb\n", "http://"+r.Host)
	})
	return mux
}
