package stream

import (
	"testing"

	"github.com/trukeio/ktest/ndjson"
)

func TestNegotiate(t *testing.T) {
	const lb, nd = ContentType, ndjson.MediaType
	for _, c := range []struct {
		accept []string
		want   string
	}{
		{nil, lb}, // every client written before NDJSON
		{[]string{"*/*"}, lb},
		{[]string{lb}, lb},
		{[]string{nd}, nd},
		{[]string{"application/x-ndjson; charset=utf-8"}, nd},
		{[]string{"Application/X-NDJSON"}, nd},
		{[]string{nd + ", */*"}, nd},   // named beats a wildcard at equal q
		{[]string{"*/*, " + nd}, nd},   // whatever the order
		{[]string{nd + ", " + lb}, lb}, // a tie between two named: the default
		{[]string{nd + ";q=0.5, " + lb + ";q=0.4"}, nd},
		{[]string{lb + ";q=0, */*"}, nd}, // Logb refused outright
		{[]string{"application/*;q=0.2, " + nd + ";q=0.1"}, lb},
		{[]string{"text/html"}, ""},
		{[]string{nd + ";q=0"}, ""},
		{[]string{"text/plain", nd}, nd}, // several Accept lines
	} {
		if got := Negotiate(c.accept); got != c.want {
			t.Errorf("Accept %q: %q, want %q", c.accept, got, c.want)
		}
	}
}
