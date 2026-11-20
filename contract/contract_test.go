package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The master's view of a contract: its names, and the hashes of the contract
// and its resource, read as strictly as Load reads it.
func TestIdentify(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "dut-interface.json")
	if err := os.WriteFile(cp, []byte(example), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "actions.robot"), []byte(actions), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := Identify(cp)
	if err != nil {
		t.Fatal(err)
	}
	h := func(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
	if id.Name != "acme-bcm-v3" || id.Hash != h(example) || id.ResourceHash != h(actions) {
		t.Errorf("%s %s %s", id.Name, id.Hash, id.ResourceHash)
	}
	if strings.Join(id.Monitors, ",") != "abort,alive,ready,speed,vx" || strings.Join(id.Modes, ",") != "active" ||
		strings.Join(id.Actions, ",") != "enter_active,recover" {
		t.Errorf("%v %v %v", id.Monitors, id.Modes, id.Actions)
	}
	if id.Settle["active"] != 500*time.Millisecond {
		t.Errorf("settle %v", id.Settle)
	}
	// The same strictness as Load: a misspelt key refuses it.
	if err := os.WriteFile(cp, []byte(strings.Replace(example, `"modes"`, `"moods"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Identify(cp); err == nil || !strings.Contains(err.Error(), "moods") {
		t.Errorf("a misspelt key: %v", err)
	}
}

func TestHas(t *testing.T) {
	id := &Identity{Name: "c", Monitors: []string{"abort", "alive"}, Modes: []string{"active"}}
	if err := id.Has("monitor", "alive"); err != nil {
		t.Error(err)
	}
	if err := id.Has("mode", "sleep"); err == nil || !strings.Contains(err.Error(), "active") {
		t.Errorf("an undeclared mode: %v", err)
	}
	if err := id.Has("action", "x"); err == nil || !strings.Contains(err.Error(), "none") {
		t.Errorf("a contract with no actions: %v", err)
	}
}

const example = `{
  "contract": "acme-bcm-v3",
  "schema": 1,
  "inputs":   {"Vx": "psu1.ch1.meas.v", "Speed": "EngineData.EngineSpeed", "Alive": "DutStatus"},
  "monitors": {
    "vx":    {"ok": "Vx > 1.0V"},
    "speed": {"ok": "abs(Speed - 3000) <= 30", "debounce": "20ms", "modes": ["active"]},
    "alive": {"ok": "age(Alive) < 30ms and delta(Alive.counter, 16) == 1"},
    "abort": {"ok": "Vx < 16V"},
    "ready": {"ok": "vx and speed and alive"}
  },
  "modes":    {"active": {"action": "enter_active", "settle": "500ms"}},
  "resource": "actions.robot",
  "actions":  {"enter_active": "Enter Active", "recover": "Recover"},
  "enums":    {"class": ["A", "B", "C", "D", "E"]},
  "timeouts": {"ready": "10s", "recover": "5s"}
}`

const actions = `*** Keywords ***
Enter Active
    Log    active
Recover
    Log    recover
`
