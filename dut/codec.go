package dut

import (
	"bytes"
	"fmt"

	"github.com/rveen/logb/dbc"
)

// Codec is the DUT's frames, as DBC lays them out: the two it sends and the
// reset it listens for. It encodes with the database rather than by hand, so
// that what ktestd decodes with the same file is what was meant.
type Codec struct {
	DB                       *dbc.File
	Status, Engine, ResetMsg *dbc.Message
	reset                    []byte // DutReset with Request = 1
}

// NewCodec reads DBC and checks that it has the messages and signals the DUT
// uses.
func NewCodec() (*Codec, error) {
	db, err := dbc.Parse(bytes.NewReader(DBC))
	if err != nil {
		return nil, fmt.Errorf("dutsim.dbc: %w", err)
	}
	c := &Codec{DB: db}
	for _, m := range []struct {
		p    **dbc.Message
		name string
	}{{&c.Status, "DutStatus"}, {&c.Engine, "EngineData"}, {&c.ResetMsg, "DutReset"}} {
		if *m.p = db.Message(m.name); *m.p == nil {
			return nil, fmt.Errorf("dutsim.dbc has no message %s", m.name)
		}
	}
	if c.reset, err = c.ResetMsg.Encode(map[string]any{"Request": 1}); err != nil {
		return nil, fmt.Errorf("dutsim.dbc: %w", err)
	}
	return c, nil
}

// Alive and Speed encode the DUT's two frames' payloads.
func (c *Codec) Alive(counter uint8) ([]byte, error) {
	return c.Status.Encode(map[string]any{"counter": int(counter)})
}

func (c *Codec) Speed(rpm float64) ([]byte, error) {
	return c.Engine.Encode(map[string]any{"EngineSpeed": rpm})
}

// IsReset says whether a frame is a reset: DutReset's identifier, standard,
// and exactly the payload the database encodes for Request = 1. Any other
// DutReset is not a reset, which is what the database says it is.
func (c *Codec) IsReset(id uint32, extended bool, payload []byte) bool {
	return id == c.ResetMsg.ID && extended == c.ResetMsg.Extended && bytes.Equal(payload, c.reset)
}
