package secondary

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/trukeio/ktest/record"
)

// # What a DUT rack needs besides the Server
//
// Who may connect, and a certificate to serve TLS with. A rack may do either
// its own way; these do it as ktestd does, so that one tokens file, made with
// `ktestd -new-token name:role`, serves both.

// LoadTokens reads a tokens file as ktestd reads it, and returns the Auth
// it describes. A line is "name role sha256", the role view or operate, the
// hash the SHA-256 of the token in hex; # starts a comment. Only hashes are
// kept, so the file admits nobody by being read. A file anyone may write to
// is refused: it would let anyone admit themselves.
func LoadTokens(path string) (func(token string) (Caller, bool), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return nil, fmt.Errorf("%s is writable by anyone, which would let anyone admit themselves", path)
	}
	type entry struct {
		hash [sha256.Size]byte
		c    Caller
	}
	var list []entry
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(s)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want \"name role sha256\", got %q", path, line, strings.TrimSpace(s))
		}
		name, role := fields[0], fields[1]
		if err := checkName(name); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if role != "view" && role != "operate" {
			return nil, fmt.Errorf("%s:%d: role %q: a role is view or operate", path, line, role)
		}
		h, err := hex.DecodeString(fields[2])
		if err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("%s:%d: a token's hash is %d hex digits of SHA-256", path, line, 2*sha256.Size)
		}
		e := entry{c: Caller{Name: name, Operate: role == "operate"}}
		copy(e.hash[:], h)
		for _, o := range list {
			switch {
			case o.c.Name == name:
				return nil, fmt.Errorf("%s:%d: %q is named twice", path, line, name)
			case o.hash == e.hash:
				return nil, fmt.Errorf("%s:%d: %q has the same token as %q", path, line, name, o.c.Name)
			}
		}
		list = append(list, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("%s admits nobody", path)
	}
	return func(tok string) (Caller, bool) {
		sum := sha256.Sum256([]byte(tok))
		var found Caller
		ok := false
		for _, e := range list {
			if subtle.ConstantTimeCompare(e.hash[:], sum[:]) == 1 {
				found, ok = e.c, true
			}
		}
		return found, ok
	}, nil
}

// checkName refuses a name the recording could not carry: it is written
// into every decision its bearer makes.
func checkName(name string) error {
	switch {
	case len(name) > record.HolderMax:
		return fmt.Errorf("name %q is longer than %d bytes", name, record.HolderMax)
	case !utf8.ValidString(name):
		return fmt.Errorf("name %q is not UTF-8", name)
	case strings.ContainsAny(name, ":,#"):
		return fmt.Errorf("name %q: a name may not contain ':', ',' or '#'", name)
	case strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0:
		return fmt.Errorf("name %q: a name is printable and has no spaces", name)
	}
	return nil
}

// SelfSigned makes a TLS certificate valid for hosts (names or IP
// addresses) for validity, writes it as PEM to certPath for the master to
// trust with cacert=, and returns it for the rack to serve with. A rack
// with a certificate from a CA of its own uses that instead.
func SelfSigned(certPath string, validity time.Duration, hosts ...string) (tls.Certificate, error) {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "DUT rack " + hosts[0]},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
