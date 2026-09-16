//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rveen/ktest/record"
)

// role is what an authenticated name may do.
type role int

const (
	roleView    role = iota + 1 // read the streams, and disarm
	roleOperate                 // everything: lease, arm, transmit
)

func (r role) String() string {
	switch r {
	case roleView:
		return "view"
	case roleOperate:
		return "operate"
	}
	return "none"
}

func parseRole(s string) (role, error) {
	switch s {
	case "view":
		return roleView, nil
	case "operate":
		return roleOperate, nil
	}
	return 0, fmt.Errorf("role %q: a role is view or operate", s)
}

// caller is who made a request, and through which listener.
type caller struct {
	name string // the token's name; "" on the local socket
	role role
	via  record.Via
}

type callerKey struct{}

func callerOf(r *http.Request) caller {
	c, _ := r.Context().Value(callerKey{}).(caller)
	return c
}

func withCaller(r *http.Request, c caller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, c))
}

// local marks every request on the Unix socket as the local operator's. The
// socket file's owner and group are its access control, so whoever reached it
// may operate, and has no name beyond the one they give a lease.
func local(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, withCaller(r, caller{role: roleOperate, via: record.ViaUnix}))
	})
}

// authenticated admits a request over the network only with a token the
// daemon knows, and marks it with that token's name and role. Everything
// behind it — control and streams alike — sees a named caller or nothing.
func authenticated(ts *tokenSet, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearer(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ktestd"`)
			httpError(w, http.StatusUnauthorized, 0, "this listener needs a token: Authorization: Bearer <token>")
			return
		}
		t, ok := ts.lookup(tok)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ktestd", error="invalid_token"`)
			httpError(w, http.StatusUnauthorized, 0, "the token is not one this daemon admits")
			return
		}
		h.ServeHTTP(w, withCaller(r, caller{name: t.name, role: t.role, via: record.ViaTLS}))
	})
}

// bearer is the token of an Authorization: Bearer header.
func bearer(r *http.Request) (string, bool) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	tok = strings.TrimSpace(tok)
	return tok, ok && strings.EqualFold(scheme, "Bearer") && tok != ""
}

// A token is one name the daemon admits over the network.
type token struct {
	name string
	role role
	hash [sha256.Size]byte
}

// A tokenSet is the daemon's tokens file, read once at start.
//
// The file holds hashes, never tokens, so reading it admits nobody: one line
// per name, "name role sha256-of-token", with # starting a comment. A token is
// 256 random bits, so a plain SHA-256 of it is as hard to reverse as the token
// is to guess, and needs no salt or stretching the way a password would.
type tokenSet struct {
	list []token
}

// loadTokens reads a tokens file. A file anyone may write to is refused: it
// would let anyone admit themselves.
func loadTokens(path string) (*tokenSet, error) {
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
	return parseTokens(f, path)
}

func parseTokens(r io.Reader, path string) (*tokenSet, error) {
	ts := &tokenSet{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		s, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(s)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want \"name role sha256\", got %q", path, line, strings.TrimSpace(s))
		}
		t := token{name: fields[0]}
		if err := checkName(t.name); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		var err error
		if t.role, err = parseRole(fields[1]); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		h, err := hex.DecodeString(fields[2])
		if err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("%s:%d: a token's hash is %d hex digits of SHA-256", path, line, 2*sha256.Size)
		}
		copy(t.hash[:], h)
		for _, o := range ts.list {
			switch {
			case o.name == t.name:
				return nil, fmt.Errorf("%s:%d: %q is named twice", path, line, t.name)
			case o.hash == t.hash:
				return nil, fmt.Errorf("%s:%d: %q has the same token as %q", path, line, t.name, o.name)
			}
		}
		ts.list = append(ts.list, t)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(ts.list) == 0 {
		return nil, fmt.Errorf("%s admits nobody", path)
	}
	return ts, nil
}

// checkName refuses a name the recording could not carry, or could not list:
// it is written into every decision its bearer makes, and the list of names is
// written into the recording as "name:role, name:role".
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

// lookup finds the name a token belongs to. Every entry is compared, and in
// constant time, so the answer's timing says nothing about which name, if
// any, came close.
func (ts *tokenSet) lookup(tok string) (token, bool) {
	sum := sha256.Sum256([]byte(tok))
	var found token
	ok := false
	for _, t := range ts.list {
		if subtle.ConstantTimeCompare(sum[:], t.hash[:]) == 1 {
			found, ok = t, true
		}
	}
	return found, ok
}

// String is the names and their roles, for the recording: never the hashes.
func (ts *tokenSet) String() string {
	var s []string
	for _, t := range ts.list {
		s = append(s, t.name+":"+t.role.String())
	}
	slices.Sort(s)
	return strings.Join(s, ", ")
}

// printNewToken makes a token for "name:role". The token goes to errw, to be
// handed to its client and never stored; the line that admits it goes to out,
// so that `ktestd -new-token rolf:operate >> tokens` appends the hash alone.
func printNewToken(out, errw io.Writer, spec string) error {
	name, r, ok := strings.Cut(spec, ":")
	if !ok {
		return errors.New(`-new-token takes name:role, e.g. rolf:operate`)
	}
	if err := checkName(name); err != nil {
		return err
	}
	role, err := parseRole(r)
	if err != nil {
		return err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tok := hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(tok))
	fmt.Fprintf(errw, "ktestd: the token for %s (%s), shown once; the daemon keeps only its hash:\n%s\n", name, role, tok)
	_, err = fmt.Fprintf(out, "%s %s %x\n", name, role, sum)
	return err
}
