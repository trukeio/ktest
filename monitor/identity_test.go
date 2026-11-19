package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	// The same strictness as Load: a misspelt key refuses it.
	if err := os.WriteFile(cp, []byte(strings.Replace(example, `"modes"`, `"moods"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Identify(cp); err == nil || !strings.Contains(err.Error(), "moods") {
		t.Errorf("a misspelt key: %v", err)
	}
}
