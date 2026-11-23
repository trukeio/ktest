// Package ndjson is the NDJSON encoding of the peer protocol's subscription
// stream, doc/peer-protocol.md §7.8: one JSON object per message, one message
// per line.
//
// The protocol defines the stream as messages (§7.5) — meta, schema, hold and
// record — and Logb and NDJSON as two encodings of them. This package goes
// between the two. Writer turns what a logb.Reader yields into lines, and
// Transcode does that to a live Logb stream as it arrives, which is how a
// rack that records in Logb serves NDJSON: the same frames, written again.
// Reader turns lines back into logb schemas and records, so a master reads
// either encoding into the same values (§7.9).
//
// What Logb has and the messages do not, and so NDJSON does not carry:
// bit offsets and byte order (a value is a number, wherever its bits were);
// a raw value under a numeric conversion (the converted value is carried, as
// a float); text conversions other than value-to-text (the raw number is
// carried); complex fields; runs. Variable-length strings and bytes are
// carried, without a "max"; Reader leaves them out of the schemas it builds,
// since a fixed record cannot hold them.
package ndjson

// MediaType is what an NDJSON subscription is served as (§7.1).
const MediaType = "application/x-ndjson"

// The type names of §7.6.
const (
	typeUint   = "uint"
	typeSint   = "sint"
	typeFloat  = "float"
	typeBool   = "bool"
	typeString = "string"
	typeBytes  = "bytes"
	typeEnum   = "enum"
)

// maxExact is the widest integer every JSON reader holds exactly: a binary64
// has a 53-bit significand. Wider integers travel as decimal strings (§7.8).
const maxExact = 53

// jsonSchema is a schema message.
type jsonSchema struct {
	Schema string            `json:"schema"`
	Fields []jsonField       `json:"fields"`
	Meta   map[string]string `json:"meta,omitempty"`
}

// jsonField is one field of a schema message.
type jsonField struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Bits     uint32            `json:"bits,omitempty"`
	Max      uint32            `json:"max,omitempty"`
	Unit     string            `json:"unit,omitempty"`
	Held     bool              `json:"held,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Guard    *jsonGuard        `json:"guard,omitempty"`
	Desc     string            `json:"desc,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
	Variable bool              `json:"-"` // a string or bytes field with no max
}

// jsonGuard says a field is present in a record only when another field holds
// a value: a Logb guarded field (§7.6, a field MAY be absent).
type jsonGuard struct {
	Field string `json:"field"`
	Value uint64 `json:"value"`
}
