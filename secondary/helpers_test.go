package secondary

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A tokens file as ktestd -new-token writes it: hashes only.
func TestLoadTokens(t *testing.T) {
	dir := t.TempDir()
	h := func(tok string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(tok))) }
	p := filepath.Join(dir, "tokens")
	must(t, os.WriteFile(p, []byte("# the bench\nalice operate "+h("a-token")+"\nviewer view "+h("v-token")+"  # read only\n"), 0o600))
	auth, err := LoadTokens(p)
	must(t, err)
	for tok, want := range map[string]Caller{"a-token": {"alice", true}, "v-token": {"viewer", false}} {
		if c, ok := auth(tok); !ok || c != want {
			t.Errorf("%s: %+v %v", tok, c, ok)
		}
	}
	if _, ok := auth("nobody"); ok {
		t.Error("an unknown token admitted")
	}
	for body, want := range map[string]string{
		"alice operate " + h("a") + "\nalice view " + h("b") + "\n": "named twice",
		"alice admin " + h("a") + "\n":                                "role",
		"alice operate deadbeef\n":                                     "hex digits",
		"# nobody\n":                                                   "admits nobody",
	} {
		must(t, os.WriteFile(p, []byte(body), 0o600))
		if _, err := LoadTokens(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
	must(t, os.WriteFile(p, []byte("alice operate "+h("a")+"\n"), 0o600))
	must(t, os.Chmod(p, 0o666))
	if _, err := LoadTokens(p); err == nil || !strings.Contains(err.Error(), "writable by anyone") {
		t.Errorf("a world-writable file: %v", err)
	}
}

// A certificate a master trusts by the PEM written beside it.
func TestSelfSigned(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cert.pem")
	cert, err := SelfSigned(p, time.Hour, "rack.local", "127.0.0.1")
	must(t, err)
	pem, err := os.ReadFile(p)
	must(t, err)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("no certificate in the PEM")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	must(t, err)
	for _, host := range []string{"rack.local", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "elsewhere"}); err == nil {
		t.Error("valid for a host not asked for")
	}
	_ = tls.Certificate(cert)
}
