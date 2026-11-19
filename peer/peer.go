// Package peer is the master's driver for a DUT rack (handoff §4.6): a
// conversation with another ktestd over its REST interface and its
// subscription stream, as package psu is a conversation with a supply.
//
// It holds no policy. It says what the DUT rack's contract is, takes and
// renews a lease, estimates the offset between the two clocks, and hands
// over each record the DUT rack streams with both racks' times. What a stale
// watermark or a monitor in dev means is the daemon's to decide.
package peer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config is one -peer flag:
//
//	dut=https://host:8443,token=dut.token,cacert=dut.pem,contract=dut-interface.json
type Config struct {
	Name     string // the peer's name, which its mirrored streams are named under: dut.mon.alive
	URL      string // the DUT rack's https listener
	Token    string // a file holding the token the master presents
	CACert   string // the DUT rack's certificate, PEM
	Contract string // the master's copy of the contract
}

var peerName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// ParseConfig reads a -peer flag.
func ParseConfig(s string) (Config, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok || !peerName.MatchString(name) {
		return Config{}, errors.New("want name=https://host:port,token=file,cacert=file,contract=file; " +
			"the name is letters, digits, _ and -, with no dots, since streams are named under it")
	}
	c := Config{Name: name}
	parts := strings.Split(rest, ",")
	c.URL = parts[0]
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return Config{}, fmt.Errorf("%q: a peer is an https listener, https://host:port", c.URL)
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	for _, kv := range parts[1:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return Config{}, fmt.Errorf("%q: want key=value", kv)
		}
		switch k {
		case "token":
			c.Token = v
		case "cacert":
			c.CACert = v
		case "contract":
			c.Contract = v
		default:
			return Config{}, fmt.Errorf("%q is not a peer setting: token, cacert, contract", k)
		}
	}
	switch {
	case c.Token == "":
		return Config{}, errors.New("token= is required: a peer is reached over TLS with a named token")
	case c.Contract == "":
		return Config{}, errors.New("contract= is required: the master's copy, compared with the DUT rack's")
	}
	return c, nil
}

// Client is one DUT rack.
type Client struct {
	cfg   Config
	token string
	http  *http.Client // requests: short timeouts
	long  *http.Client // the subscription: none
}

// New makes a client: the token read, the certificate trusted.
func New(cfg Config) (*Client, error) {
	tok, err := os.ReadFile(cfg.Token)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", cfg.CACert)
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true}
	return &Client{cfg: cfg, token: strings.TrimSpace(string(tok)),
		http: &http.Client{Transport: tr, Timeout: 5 * time.Second},
		long: &http.Client{Transport: tr}}, nil
}

// Name is the peer's name.
func (c *Client) Name() string { return c.cfg.Name }

// Info is what a DUT rack says of itself.
type Info struct {
	Contract     string            `json:"contract"`
	Hash         string            `json:"sha256"`
	ResourceHash string            `json:"resource_sha256"`
	Provenance   map[string]string `json:"provenance"`
}

// Info asks the DUT rack which contract it runs.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var in Info
	err := c.do(ctx, "GET", "/dut/contract", nil, "", &in)
	return in, err
}

// ClockSample is one round trip to the DUT rack's clock: when it was asked
// (Sent) and answered (Recv) by this clock, and what it said, by its own.
type ClockSample struct {
	Sent, Recv time.Time
	Peer       int64 // Unix ns, the DUT rack's clock
}

// RTT is the round trip.
func (s ClockSample) RTT() time.Duration { return s.Recv.Sub(s.Sent) }

// Offset is the DUT rack's clock less this one, assuming the answer was
// read halfway through the round trip; it is right to within half the round
// trip, whatever the two halves were.
func (s ClockSample) Offset() time.Duration {
	mid := s.Sent.UnixNano() + s.Recv.Sub(s.Sent).Nanoseconds()/2
	return time.Duration(s.Peer - mid)
}

// Clock takes one sample of the DUT rack's clock.
func (c *Client) Clock(ctx context.Context) (ClockSample, error) {
	var body struct {
		T int64 `json:"t"`
	}
	s := ClockSample{Sent: time.Now()}
	err := c.do(ctx, "GET", "/clock", nil, "", &body)
	s.Recv = time.Now()
	s.Peer = body.T
	if err == nil && body.T == 0 {
		err = errors.New("the peer's clock answered no time")
	}
	return s, err
}

// Lease takes the DUT's lease for holder, for ttl, and returns its token.
func (c *Client) Lease(ctx context.Context, holder string, ttl time.Duration) (string, error) {
	var body struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, "POST", "/dut/lease", map[string]string{"holder": holder, "ttl": ttl.String()}, "", &body)
	return body.Token, err
}

// Renew renews the DUT's lease.
func (c *Client) Renew(ctx context.Context, lease string) error {
	return c.do(ctx, "PUT", "/dut/lease", nil, lease, nil)
}

// Release gives the DUT's lease up.
func (c *Client) Release(ctx context.Context, lease string) error {
	return c.do(ctx, "DELETE", "/dut/lease", nil, lease, nil)
}

// RefusedError is a request the DUT rack answered with a refusal: its
// sentence and, when the refusal was recorded, its number.
type RefusedError struct {
	Status int
	Seq    uint32
	Msg    string
}

func (e *RefusedError) Error() string {
	if e.Seq != 0 {
		return fmt.Sprintf("refused, seq %d: %s", e.Seq, e.Msg)
	}
	return "refused: " + e.Msg
}

func (c *Client) do(ctx context.Context, method, path string, body any, lease string, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if lease != "" {
		req.Header.Set("Ktest-Lease", lease)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
			Seq   uint32 `json:"seq"`
		}
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return &RefusedError{Status: resp.StatusCode, Seq: e.Seq, Msg: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("%s %s answered %q: %v", method, path, b, err)
		}
	}
	return nil
}
