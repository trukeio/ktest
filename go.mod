module github.com/rveen/ktest

go 1.25.6

require (
	github.com/google/uuid v1.6.0
	github.com/rveen/logb v0.0.0
	github.com/rveen/logb/viewer v0.0.0-00010101000000-000000000000
	golang.org/x/sys v0.46.0
)

require github.com/klauspost/compress v1.19.0 // indirect

// logb has no tagged release yet, and it is developed alongside this repo.
// Same reasoning as logb's own viewer/go.mod: drop this and pin a version once
// the parent is tagged.
replace github.com/rveen/logb => ../logb

// The index recorder lives in the viewer module (SPEC rule 4 keeps it out of
// the core). Same local-development replace as logb itself.
replace github.com/rveen/logb/viewer => ../logb/viewer
