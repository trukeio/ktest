// Command sublat measures how late a DUT rack's monitors reach a subscriber.
//
//	sublat [-cacert pem] [-token file] [-d 10s] https://host:8443/stream/sub?ch=mon.*
//
// It subscribes to /stream/sub, and for every record that arrives notes the
// time it arrived and the time it is about. A monitor transition is about the
// sample or the deadline that caused it (handoff §4.3), so its lateness is
// the whole path: the reorder window, the evaluator, the loop, the writer,
// the hub, TLS and the socket. The watermark's lateness is how far behind
// the daemon's clock the claim "everything up to here has been sent" runs.
//
// The handoff's target (§4.4) is a transition on the wire within 5 ms of its
// sample at the 99th percentile on loopback. This measures it rather than
// assuming it. Both clocks are this machine's, so on loopback the numbers
// are the path's own; across machines they would include the clocks' offset.
//
// A record's time is its axis plus the stream's epoch, recording.epoch, which
// the rack states in the stream's metadata.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trukeio/logb"
)

func main() {
	cacert := flag.String("cacert", "", "the rack's certificate, PEM, for https")
	token := flag.String("token", "", "a file holding a token with the view role")
	dur := flag.Duration("d", 10*time.Second, "how long to measure")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: sublat [flags] https://host:8443/stream/sub?ch=mon.*\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	tr := &http.Transport{ForceAttemptHTTP2: true}
	if *cacert != "" {
		pem, err := os.ReadFile(*cacert)
		if err != nil {
			fatalf("%v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			fatalf("%s holds no certificate", *cacert)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	req, err := http.NewRequest("GET", flag.Arg(0), nil)
	if err != nil {
		fatalf("%v", err)
	}
	if *token != "" {
		b, err := os.ReadFile(*token)
		if err != nil {
			fatalf("%v", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		fatalf("%v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fatalf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	time.AfterFunc(*dur, func() { resp.Body.Close() })

	m, err := measure(resp.Body)
	if err != nil && !errors.Is(err, io.EOF) && m.records == 0 {
		fatalf("%v", err)
	}
	m.report(os.Stdout)
}

type measurement struct {
	records     int
	transitions []time.Duration // per monitor transition: arrival less its time
	watermarks  []time.Duration // per watermark: arrival less the time it vouches for
	byMonitor   map[string]int
}

func measure(r io.Reader) (*measurement, error) {
	m := &measurement{byMonitor: map[string]int{}}
	rd, err := logb.NewReader(r)
	if err != nil {
		return m, err
	}
	epoch := int64(-1)
	for {
		b, err := rd.Next()
		arrived := time.Now().UnixNano()
		if err != nil {
			return m, err
		}
		if epoch < 0 {
			for _, kv := range rd.Meta {
				if kv.Key == "recording.epoch" {
					epoch, _ = strconv.ParseInt(kv.Value, 10, 64)
				}
			}
			if epoch < 0 {
				return m, errors.New("the stream does not state recording.epoch: is the rack older than this tool?")
			}
		}
		name := b.Schema.Name
		if !strings.HasPrefix(name, "mon.") || name == "mon.gap" {
			continue
		}
		for i := 0; i < int(b.Count); i++ {
			ax, err := b.Axis(i)
			if err != nil {
				return m, err
			}
			d, err := ax.Duration(b.Schema.AxisExp)
			if err != nil {
				return m, err
			}
			late := time.Duration(arrived - (epoch + int64(d)))
			m.records++
			if name == "mon.watermark" {
				m.watermarks = append(m.watermarks, late)
				continue
			}
			// The first record of each monitor is its state at the start,
			// written when the stream was declared, and the preamble restates
			// held states at every join: neither is a transition.
			if from, to := field(b, i, "from"), field(b, i, "state"); from == to {
				continue
			}
			m.transitions = append(m.transitions, late)
			m.byMonitor[name]++
		}
	}
}

func field(b *logb.Batch, i int, name string) any {
	for k := range b.Schema.Fields {
		if b.Schema.Fields[k].Name == name {
			v, _ := b.Value(i, k)
			return v
		}
	}
	return nil
}

func (m *measurement) report(w io.Writer) {
	fmt.Fprintf(w, "%d records\n", m.records)
	line := func(what string, ds []time.Duration) {
		if len(ds) == 0 {
			fmt.Fprintf(w, "%-12s none\n", what)
			return
		}
		s := slices.Clone(ds)
		slices.Sort(s)
		q := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s))))] }
		fmt.Fprintf(w, "%-12s n=%-6d min %-10v p50 %-10v p90 %-10v p99 %-10v max %v\n",
			what, len(s), s[0], q(0.5), q(0.9), q(0.99), s[len(s)-1])
	}
	line("transitions", m.transitions)
	line("watermarks", m.watermarks)
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m.byMonitor {
			if !yield(k) {
				return
			}
		}
	}) {
		fmt.Fprintf(w, "  %s: %d transitions\n", k, m.byMonitor[k])
	}
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "sublat: %s\n", fmt.Sprintf(format, a...))
	os.Exit(1)
}
