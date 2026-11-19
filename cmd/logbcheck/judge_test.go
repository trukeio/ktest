package main

import "testing"

// A judgement is recomputed from the mirrored states alone: the window's
// start state and every state inside it, and for all-ok the states at the
// watermark. One fault planted at a time must show.
func TestReplayJudgement(t *testing.T) {
	hist := map[string][]mstamp{
		"alive": {{0, "unknown"}, {100, "ok"}, {500, "dev"}, {900, "ok"}},
		"speed": {{0, "inactive"}, {200, "settling"}, {400, "ok"}},
	}
	for _, c := range []struct {
		name string
		j    judged
		want string
	}{
		{"a quiet window", judged{kind: 1, from: 400, to: 499, monitors: []string{"alive", "speed"}}, ""},
		{"a deviation inside", judged{kind: 1, from: 400, to: 600, monitors: []string{"alive", "speed"}}, "alive=dev"},
		{"a deviation at the start", judged{kind: 1, from: 600, to: 700, monitors: []string{"alive"}}, "alive=dev"},
		{"a deviation just after the end", judged{kind: 1, from: 400, to: 500 - 1, monitors: []string{"alive"}}, ""},
		{"a deviation at the end", judged{kind: 1, from: 400, to: 500, monitors: []string{"alive"}}, "alive=dev"},
		{"unknown before anything", judged{kind: 1, from: 50, to: 60, monitors: []string{"alive"}}, "alive=unknown"},
		{"a monitor never mirrored", judged{kind: 1, from: 400, to: 450, monitors: []string{"abort"}}, "abort=unknown"},
		{"settling is not a deviation", judged{kind: 1, from: 250, to: 300, monitors: []string{"speed"}}, ""},
		{"all ok", judged{kind: 2, to: 950, monitors: []string{"alive", "speed"}}, "ok"},
		{"not all ok", judged{kind: 2, to: 600, monitors: []string{"alive", "speed"}}, "not ok"},
	} {
		if got := replay(c.j, hist); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
