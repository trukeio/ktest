//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckPanel holds a save from the rack to the rules a file is held to at
// start, one broken rule at a time. The good panel's widgets touch on every
// side, so touching is not taken for overlapping.
func TestCheckPanel(t *testing.T) {
	good := `{"title": "bench", "columns": 6, "widgets": [
		{"type": "readout", "channel": "psu1.ch1.meas.v", "x": 0, "y": 0, "w": 3, "h": 2},
		{"type": "setpoint", "channel": "psu1.ch1.v.set", "step": 0.1, "x": 3, "y": 0, "w": 3, "h": 2},
		{"type": "plot", "channels": ["psu1.ch1.meas.v"], "x": 0, "y": 2, "w": 6, "h": 4}
	]}`
	if err := checkPanel([]byte(good)); err != nil {
		t.Fatalf("a good panel: %v", err)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"a misspelt key", `"step": 0.1`, `"stp": 0.1`, "unknown field"},
		{"a widget past the grid's edge", `"x": 3, "y": 0, "w": 3`, `"x": 4, "y": 0, "w": 3`, "columns 4 to 6 of a grid of 6"},
		{"widgets on top of each other", `"x": 0, "y": 2`, `"x": 0, "y": 1`, "overlaps widget 0"},
		{"a toggle on a voltage", `"type": "setpoint"`, `"type": "toggle"`, "a toggle switches something on or off"},
		{"a step on a readout", `"h": 2},
		{"type": "setpoint"`, `"h": 2, "step": 1},
		{"type": "setpoint"`, "only a setpoint"},
		{"a second value after the panel", `]}`, `]} {}`, "follows the panel"},
		{"too many columns", `"columns": 6`, `"columns": 49`, "49 columns"},
		{"widgets and groups at once", `"widgets": [`, `"groups": [{"widgets": []}], "widgets": [`, `either "widgets" or "groups"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Replace(good, tc.from, tc.to, 1)
			if bad == good {
				t.Fatalf("the case changed nothing")
			}
			err := checkPanel([]byte(bad))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestCheckPanelGroups holds a panel of instrument boxes to the rules a box
// adds: its own grid, its own overlaps, and no widget belonging to another
// instrument. The good panel's two boxes place widgets at the same
// coordinates, since a box's grid is its own and not the panel's.
func TestCheckPanelGroups(t *testing.T) {
	good := `{"title": "bench", "columns": 6, "groups": [
		{"instrument": "psu1", "label": "Power supply", "widgets": [
			{"type": "readout", "channel": "psu1.ch1.meas.v", "x": 0, "y": 0, "w": 3, "h": 2},
			{"type": "setpoint", "channel": "psu1.ch1.v.set", "x": 3, "y": 0, "w": 3, "h": 2}
		]},
		{"instrument": "scope1", "columns": 4, "widgets": [
			{"type": "scope", "channel": "scope1.ch1", "x": 0, "y": 0, "w": 4, "h": 4}
		]},
		{"label": "Anything", "widgets": [
			{"type": "readout", "channel": "psu1.ch2.meas.v", "x": 0, "y": 0, "w": 3, "h": 2}
		]}
	]}`
	if err := checkPanel([]byte(good)); err != nil {
		t.Fatalf("a good panel: %v", err)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"a widget past its box's own width", `"x": 0, "y": 0, "w": 4, "h": 4`, `"x": 0, "y": 0, "w": 6, "h": 4`,
			"group 1 (scope1): widget 0 (scope): columns 0 to 5 of a grid of 4"},
		{"widgets on top of each other in a box", `"x": 3, "y": 0, "w": 3, "h": 2`, `"x": 2, "y": 0, "w": 3, "h": 2`,
			"group 0 (psu1): widget 1 (setpoint) overlaps widget 0 (readout)"},
		{"another instrument's widget in a box", `"channel": "psu1.ch1.v.set"`, `"channel": "scope1.timebase.tdiv.set"`,
			"scope1.timebase.tdiv.set is scope1's, and the box is psu1's"},
		{"a misspelt key on a box", `"label": "Power supply"`, `"labl": "Power supply"`, "unknown field"},
		{"too many columns on a box", `"columns": 4`, `"columns": 49`, "group 1 (scope1): 49 columns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := strings.Replace(good, tc.from, tc.to, 1)
			if bad == good {
				t.Fatalf("the case changed nothing")
			}
			err := checkPanel([]byte(bad))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestExamplePanels holds the panels the examples ship to the same rules as
// any other, since a demo whose panel the daemon refuses is a demo that does
// not start.
func TestExamplePanels(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "panel.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no example panels found: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkPanel(raw); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		})
	}
}

// TestReplaceFile: a save replaces the file whole and keeps its permissions,
// and leaves nothing beside it.
func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bench.json")
	if err := os.WriteFile(path, []byte(`{"widgets":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(path, []byte(`{"title":"new","widgets":[]}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"title":"new","widgets":[]}` {
		t.Fatalf("read back %q, %v", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want the file's own 0600", fi.Mode().Perm())
	}
	if es, _ := os.ReadDir(dir); len(es) != 1 {
		t.Fatalf("%d files in the directory, want only the panel", len(es))
	}
}
