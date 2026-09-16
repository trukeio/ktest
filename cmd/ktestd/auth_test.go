//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

func TestTokens(t *testing.T) {
	file := "# names this daemon admits\n" +
		"rolf operate " + hashOf("rolf-secret") + "\n" +
		"\n" +
		"dash view " + hashOf("dash-secret") + "  # a dashboard\n"
	ts, err := parseTokens(strings.NewReader(file), "tokens")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ts.String(), "dash:view, rolf:operate"; got != want {
		t.Errorf("names %q, want %q", got, want)
	}
	if tk, ok := ts.lookup("rolf-secret"); !ok || tk.name != "rolf" || tk.role != roleOperate {
		t.Errorf("rolf's token: %+v, %v", tk, ok)
	}
	if _, ok := ts.lookup("guess"); ok {
		t.Error("a token the file does not hold was admitted")
	}
	// Someone who reads the file learns hashes, and a hash must not admit them.
	if _, ok := ts.lookup(hashOf("rolf-secret")); ok {
		t.Error("the hash in the file admitted its reader")
	}

	for _, bad := range []struct{ name, file, want string }{
		{"a role that does not exist", "rolf admin " + hashOf("x"), "role"},
		{"a name given twice", "rolf view " + hashOf("a") + "\nrolf operate " + hashOf("b"), "twice"},
		{"one token for two names", "rolf view " + hashOf("a") + "\neve view " + hashOf("a"), "same token"},
		{"a hash of the wrong length", "rolf view abcd", "hex digits"},
		{"a name too long for the recording", strings.Repeat("n", 33) + " view " + hashOf("a"), "longer than"},
		{"a name that would break the list", "ro:lf view " + hashOf("a"), "may not contain"},
		{"a line of the wrong shape", "rolf view", "want"},
		{"a file that admits nobody", "# nobody\n", "admits nobody"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := parseTokens(strings.NewReader(bad.file), "tokens")
			if err == nil || !strings.Contains(err.Error(), bad.want) {
				t.Errorf("got %v, want an error mentioning %q", err, bad.want)
			}
		})
	}
}

func TestTokensFileWritableByAnyoneIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(p, []byte("rolf operate "+hashOf("x")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTokens(p); err != nil {
		t.Fatalf("an owner-only file must load: %v", err)
	}
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTokens(p); err == nil {
		t.Fatal("a file anyone may write to was accepted, and anyone could admit themselves")
	}
}

// TestNewTokenAdmitsItself: the line -new-token prints for the file admits
// the token it prints for the client, and does not carry that token.
func TestNewTokenAdmitsItself(t *testing.T) {
	var out, errw bytes.Buffer
	if err := printNewToken(&out, &errw, "rolf:operate"); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	lines := strings.Split(strings.TrimSpace(errw.String()), "\n")
	tok := lines[len(lines)-1]
	if strings.Contains(line, tok) {
		t.Error("the line for the file carries the token itself")
	}
	ts, err := parseTokens(strings.NewReader(line), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if tk, ok := ts.lookup(tok); !ok || tk.name != "rolf" || tk.role != roleOperate {
		t.Errorf("the printed line does not admit the printed token: %+v, %v", tk, ok)
	}
	for _, spec := range []string{"rolf", "rolf:admin", "ro,lf:view"} {
		if err := printNewToken(&out, &errw, spec); err == nil {
			t.Errorf("-new-token %s was accepted", spec)
		}
	}
}

func TestBearer(t *testing.T) {
	for _, c := range []struct {
		header, tok string
		ok          bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"Basic abc", "", false},
		{"Bearer ", "", false},
		{"", "", false},
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		tok, ok := bearer(r)
		if ok != c.ok || (ok && tok != c.tok) {
			t.Errorf("Authorization %q: got %q, %v", c.header, tok, ok)
		}
	}
}

// TestSelfSignedCertIsKept: the certificate made on a first start is the one
// every later start presents, since a client that pinned or trusted it would
// otherwise be broken by a restart; and a client that trusts it can reach the
// daemon by the names it was made for.
func TestSelfSignedCertIsKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	a, src, err := loadOrMakeCert("", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "made now") {
		t.Errorf("first start: source %q, want a certificate made now", src)
	}
	b, src, err := loadOrMakeCert("", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, "made now") || a.fingerprint != b.fingerprint {
		t.Errorf("the second start made a new certificate (%s)", src)
	}
	if fi, err := os.Stat(filepath.Join(dir, "tls-key.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file: %v, %v; want mode 0600", fi, err)
	}

	leaf, err := x509.ParseCertificate(a.tls.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	for _, name := range []string{"localhost", "127.0.0.1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: name}); err != nil {
			t.Errorf("a client trusting the certificate cannot reach %s: %v", name, err)
		}
	}
	if _, _, err := loadOrMakeCert(filepath.Join(dir, "tls-cert.pem"), "", dir); err == nil {
		t.Error("-tls-cert without -tls-key was accepted")
	}
}
