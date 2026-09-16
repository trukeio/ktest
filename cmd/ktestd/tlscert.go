//go:build linux

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// serverCert is the certificate the network listener presents, and the two
// ways a client can pin it.
type serverCert struct {
	tls         tls.Certificate
	fingerprint string // SHA-256 of the certificate, hex: what the recording names
	pin         string // for curl's --pinnedpubkey: sha256// and the public key's SHA-256, base64
}

// defaultStateDir is where the daemon keeps what it makes, following the XDG
// convention for state: data that should survive a restart but is not
// configuration.
func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "ktestd")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "ktestd")
	}
	return "ktestd-state"
}

// loadOrMakeCert is the listener's certificate: the operator's files if given,
// otherwise the one the daemon made on an earlier start, otherwise a new one.
// It returns where the certificate came from, for the recording.
//
// A made certificate is kept, not remade on every start, because a client
// that has pinned it or trusted it would otherwise be broken by a restart. It
// is never overwritten: if either half exists, both must load.
func loadOrMakeCert(certFile, keyFile, stateDir string) (*serverCert, string, error) {
	switch {
	case certFile != "" && keyFile != "":
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, "", err
		}
		sc, err := describe(c)
		return sc, certFile, err
	case certFile != "" || keyFile != "":
		return nil, "", errors.New("-tls-cert and -tls-key go together")
	}

	cf, kf := filepath.Join(stateDir, "tls-cert.pem"), filepath.Join(stateDir, "tls-key.pem")
	source := cf + " (self-signed)"
	if !exists(cf) && !exists(kf) {
		if err := makeSelfSigned(cf, kf); err != nil {
			return nil, "", err
		}
		source = cf + " (self-signed, made now)"
	}
	c, err := tls.LoadX509KeyPair(cf, kf)
	if err != nil {
		return nil, "", err
	}
	sc, err := describe(c)
	return sc, source, err
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// makeSelfSigned makes a certificate that is its own authority, so that a
// client can trust it as it would a lab CA — curl --cacert tls-cert.pem —
// instead of turning verification off. It names localhost, the loopback
// addresses and whatever addresses the host has as it is made; a lab that
// renames or readdresses the host, or has its own CA, supplies -tls-cert.
func makeSelfSigned(certFile, keyFile string) error {
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ktestd " + host},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else if host != "" && host != "localhost" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
				continue
			}
			if !containsIP(tmpl.IPAddresses, n.IP) {
				tmpl.IPAddresses = append(tmpl.IPAddresses, n.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key first and exclusively, owner-only: a key readable by others,
	// even for a moment, is a key that has to be replaced.
	if err := writePEM(keyFile, "PRIVATE KEY", kder, 0o600); err != nil {
		return err
	}
	return writePEM(certFile, "CERTIFICATE", der, 0o644)
}

func containsIP(list []net.IP, ip net.IP) bool {
	for _, l := range list {
		if l.Equal(ip) {
			return true
		}
	}
	return false
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: kind, Bytes: der}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func describe(c tls.Certificate) (*serverCert, error) {
	if len(c.Certificate) == 0 {
		return nil, errors.New("tls: no certificate")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	fp := sha256.Sum256(c.Certificate[0])
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return &serverCert{
		tls:         c,
		fingerprint: hex.EncodeToString(fp[:]),
		pin:         "sha256//" + base64.StdEncoding.EncodeToString(spki[:]),
	}, nil
}
