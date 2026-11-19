package procedure

import (
	"fmt"
	"os"
	"strings"
	"unicode"
)

// Pos is where something was written.
type Pos struct {
	File string
	Line int
}

func (p Pos) String() string { return fmt.Sprintf("%s:%d", p.File, p.Line) }

// Error is a refusal, with where it was written. A procedure is refused whole,
// before anything runs, and every refusal names the construct and its line.
type Error struct {
	Pos Pos
	Msg string
}

func (e *Error) Error() string { return e.Pos.String() + ": " + e.Msg }

func errAt(p Pos, format string, a ...any) *Error {
	return &Error{Pos: p, Msg: fmt.Sprintf(format, a...)}
}

// File is one parsed procedure or resource file.
type File struct {
	Path      string
	Doc       string
	Resources []Setting // Resource <file>
	VarFiles  []Setting // Variables <file.json>
	Vars      []*VarDef
	Tests     []*Block
	Keywords  []*Block
}

// Setting is a Settings line that names a file.
type Setting struct {
	Pos  Pos
	Path string
}

// VarDef is one line of a Variables section: ${x}, @{l} or &{d}.
type VarDef struct {
	Pos    Pos
	Sigil  byte   // '$', '@' or '&'
	Name   string // as written, without sigil and braces
	Values []string
}

// Block is a test case or a user keyword.
type Block struct {
	Pos      Pos
	Name     string
	Keyword  bool // a keyword, not a test
	Doc      string
	Tags     []string
	Args     []ArgDef
	Setup    *Call
	Teardown *Call
	Timeout  string
	TimeoutP Pos
	Body     []Stmt
}

// ArgDef is one of a keyword's [Arguments].
type ArgDef struct {
	Name    string // normalised
	Written string
	Default *string
}

// Stmt is one statement of a body.
type Stmt interface{ pos() Pos }

// Call is a keyword call, with what it assigns.
type Call struct {
	Pos    Pos
	Assign []string // variable names, normalised, without sigil and =
	Name   string
	Args   []string // cells as written, escapes and variables unresolved
}

// For is FOR ... IN ... or FOR ... IN RANGE ...
type For struct {
	Pos   Pos
	Vars  []string
	Range bool
	Items []string
	Body  []Stmt
}

// If is IF / ELSE IF / ELSE / END.
type If struct {
	Pos      Pos
	Branches []Branch
}

// Branch is one arm of an If; Cond is "" for ELSE.
type Branch struct {
	Pos  Pos
	Cond string
	Body []Stmt
}

// While is WHILE cond limit=...
type While struct {
	Pos   Pos
	Cond  string
	Limit string
	Body  []Stmt
}

type Break struct{ Pos Pos }
type Continue struct{ Pos Pos }

// Return is RETURN, with what it returns.
type Return struct {
	Pos    Pos
	Values []string
}

func (s *Call) pos() Pos     { return s.Pos }
func (s *For) pos() Pos      { return s.Pos }
func (s *If) pos() Pos       { return s.Pos }
func (s *While) pos() Pos    { return s.Pos }
func (s *Break) pos() Pos    { return s.Pos }
func (s *Continue) pos() Pos { return s.Pos }
func (s *Return) pos() Pos   { return s.Pos }

// line is one logical line: its cells, continuation lines folded in, and
// whether it was indented (a body line) or not (a name or a setting).
type line struct {
	pos      Pos
	indented bool
	cells    []string
}

// ParseFile reads and parses one file.
func ParseFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, string(b))
}

// Parse parses the text of one file. It refuses what is outside the subset
// as soon as it meets it, with a sentence naming the construct and its line.
func Parse(path, src string) (*File, error) {
	f := &File{Path: path}
	section := ""
	var cur *Block
	var body *bodyParser
	finish := func() error {
		if cur == nil {
			return nil
		}
		if err := body.finish(); err != nil {
			return err
		}
		cur.Body = body.stmts
		cur, body = nil, nil
		return nil
	}

	lines, err := split(path, src)
	if err != nil {
		return nil, err
	}
	for _, ln := range lines {
		c := ln.cells
		if !ln.indented && strings.HasPrefix(c[0], "*") {
			if err := finish(); err != nil {
				return nil, err
			}
			name := strings.ToLower(strings.Join(strings.Fields(strings.Trim(strings.Join(c, " "), "* ")), " "))
			switch name {
			case "settings", "setting":
				section = "settings"
			case "variables", "variable":
				section = "variables"
			case "test cases", "test case":
				section = "tests"
			case "keywords", "keyword":
				section = "keywords"
			case "comments", "comment":
				section = "comments"
			case "tasks", "task":
				return nil, errAt(ln.pos, "*** Tasks ***: a procedure's section is *** Test Cases ***")
			default:
				return nil, errAt(ln.pos, "%s is not a section; the sections are Settings, Variables, Test Cases and Keywords",
					strings.Join(c, " "))
			}
			continue
		}
		switch section {
		case "":
			return nil, errAt(ln.pos, "this line comes before any section header, such as *** Test Cases ***")
		case "comments":
		case "settings":
			if ln.indented {
				return nil, errAt(ln.pos, "a setting starts at the beginning of its line")
			}
			if err := f.setting(ln); err != nil {
				return nil, err
			}
		case "variables":
			if ln.indented {
				return nil, errAt(ln.pos, "a variable starts at the beginning of its line")
			}
			v, err := varDef(ln)
			if err != nil {
				return nil, err
			}
			f.Vars = append(f.Vars, v)
		case "tests", "keywords":
			if !ln.indented {
				if err := finish(); err != nil {
					return nil, err
				}
				cur = &Block{Pos: ln.pos, Name: c[0], Keyword: section == "keywords"}
				if strings.Contains(c[0], "${") {
					return nil, errAt(ln.pos, "%q: keywords with embedded arguments are not in the subset; use [Arguments]", c[0])
				}
				if section == "tests" {
					f.Tests = append(f.Tests, cur)
				} else {
					f.Keywords = append(f.Keywords, cur)
				}
				body = &bodyParser{block: cur}
				if len(c) > 1 {
					// Robot allows the first step on the name's line.
					if err := body.line(line{pos: ln.pos, indented: true, cells: c[1:]}); err != nil {
						return nil, err
					}
				}
				continue
			}
			if cur == nil {
				return nil, errAt(ln.pos, "an indented line before any test or keyword name")
			}
			if err := body.line(ln); err != nil {
				return nil, err
			}
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	return f, nil
}

// split turns the text into logical lines: cells split on tabs or two or more
// spaces, comments dropped, continuation lines folded into the line they
// continue.
func split(path, src string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		p := Pos{path, i + 1}
		if strings.HasPrefix(raw, "| ") || raw == "|" {
			return nil, errAt(p, "the pipe-separated format is not in the subset; separate cells with two or more spaces")
		}
		indented := raw != "" && (raw[0] == ' ' || raw[0] == '\t')
		cells := cellsOf(raw)
		// A comment runs from a cell starting with # to the end of the line.
		for j, c := range cells {
			if strings.HasPrefix(c, "#") {
				cells = cells[:j]
				break
			}
		}
		// Trailing empty cells mean nothing.
		for len(cells) > 0 && cells[len(cells)-1] == "" {
			cells = cells[:len(cells)-1]
		}
		if len(cells) == 0 {
			continue
		}
		if cells[0] == "..." {
			if len(out) == 0 {
				return nil, errAt(p, "... continues a line, and there is none before it")
			}
			out[len(out)-1].cells = append(out[len(out)-1].cells, cells[1:]...)
			continue
		}
		out = append(out, line{pos: p, indented: indented, cells: cells})
	}
	return out, nil
}

// cellsOf splits one physical line. Leading indentation is not a cell. A
// backslash keeps the space after it, so "a\  b" is one cell.
func cellsOf(raw string) []string {
	s := strings.TrimLeft(raw, " \t")
	s = strings.TrimRight(s, " \t")
	var cells []string
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '\\' && i+1 < len(rs) {
			b.WriteRune(r)
			b.WriteRune(rs[i+1])
			i++
			continue
		}
		if r == '\t' || r == ' ' && i+1 < len(rs) && rs[i+1] == ' ' {
			cells = append(cells, b.String())
			b.Reset()
			for i+1 < len(rs) && (rs[i+1] == ' ' || rs[i+1] == '\t') {
				i++
			}
			continue
		}
		b.WriteRune(r)
	}
	cells = append(cells, b.String())
	return cells
}

func (f *File) setting(ln line) error {
	c := ln.cells
	name := norm(c[0])
	switch name {
	case "resource", "variables":
		if len(c) != 2 {
			return errAt(ln.pos, "%s takes one file", c[0])
		}
		s := Setting{Pos: ln.pos, Path: c[1]}
		if name == "resource" {
			f.Resources = append(f.Resources, s)
		} else {
			f.VarFiles = append(f.VarFiles, s)
		}
	case "documentation":
		f.Doc = strings.Join(c[1:], " ")
	case "library":
		return errAt(ln.pos, "Library is not in the subset: the keywords are Ktest's own, and need no import")
	case "testtemplate", "tasktemplate":
		return errAt(ln.pos, "%s: templates are not in the subset; write each step out", c[0])
	case "suitesetup", "suiteteardown", "testsetup", "testteardown", "tasksetup", "taskteardown":
		return errAt(ln.pos, "%s is not in the subset; give each test its own [Setup] or [Teardown]", c[0])
	default:
		return errAt(ln.pos, "%s is not a setting of the subset; the settings are Resource, Variables and Documentation", c[0])
	}
	return nil
}

func varDef(ln line) (*VarDef, error) {
	c := ln.cells
	name := strings.TrimSuffix(strings.TrimSpace(c[0]), "=")
	name = strings.TrimSpace(name)
	if len(name) < 4 || !strings.ContainsRune("$@&", rune(name[0])) || name[1] != '{' || name[len(name)-1] != '}' {
		return nil, errAt(ln.pos, "%q is not a variable; write ${name}, @{name} or &{name}", c[0])
	}
	v := &VarDef{Pos: ln.pos, Sigil: name[0], Name: name[2 : len(name)-1], Values: c[1:]}
	if strings.HasPrefix(v.Name, "{") {
		return nil, errAt(ln.pos, "${{...}}: inline Python is not in the subset")
	}
	if v.Sigil == '$' && len(v.Values) > 1 {
		return nil, errAt(ln.pos, "${%s} has %d values; a scalar has one, a list is written @{%s}", v.Name, len(v.Values), v.Name)
	}
	if v.Sigil == '&' {
		for _, kv := range v.Values {
			if k, _, ok := strings.Cut(kv, "="); !ok || k == "" {
				return nil, errAt(ln.pos, "&{%s}: %q is not key=value", v.Name, kv)
			}
		}
	}
	return v, nil
}

// bodyParser assembles a block's statements, with a stack for the control
// structures that are open.
type bodyParser struct {
	block *Block
	stmts []Stmt
	open  []openStmt
}

type openStmt struct {
	s    Stmt
	body *[]Stmt
}

func (b *bodyParser) add(s Stmt) {
	if n := len(b.open); n > 0 {
		*b.open[n-1].body = append(*b.open[n-1].body, s)
		return
	}
	b.stmts = append(b.stmts, s)
}

func (b *bodyParser) finish() error {
	if n := len(b.open); n > 0 {
		s := b.open[n-1].s
		return errAt(s.pos(), "%s has no END", keywordOf(s))
	}
	return nil
}

func keywordOf(s Stmt) string {
	switch s.(type) {
	case *For:
		return "FOR"
	case *If:
		return "IF"
	case *While:
		return "WHILE"
	}
	return "this"
}

// refusedControl names the Robot control words outside the subset, with
// what to do instead.
var refusedControl = map[string]string{
	"TRY":     "TRY/EXCEPT is not in the subset: a failure fails the test, and [Teardown] runs",
	"EXCEPT":  "EXCEPT is not in the subset: a failure fails the test, and [Teardown] runs",
	"FINALLY": "FINALLY is not in the subset; use [Teardown]",
	"GROUP":   "GROUP is not in the subset",
	"VAR":     "VAR is not in the subset; write ${x}=    Set Variable    value",
	":FOR":    ":FOR is old syntax; write FOR ... IN ... END",
}

func (b *bodyParser) line(ln line) error {
	c := ln.cells
	if len(c) > 0 && c[0] == "" {
		c = c[1:] // Robot's "\" or an empty first cell
	}
	if len(c) == 0 {
		return nil
	}
	p := ln.pos
	head := c[0]

	if strings.HasPrefix(head, "[") && strings.HasSuffix(head, "]") {
		if len(b.open) > 0 {
			return errAt(p, "%s inside %s: settings come before the steps", head, keywordOf(b.open[len(b.open)-1].s))
		}
		return b.setting(p, head, c[1:])
	}
	if msg, ok := refusedControl[head]; ok {
		return errAt(p, "%s", msg)
	}
	switch head {
	case "FOR":
		f := &For{Pos: p}
		i := 1
		for ; i < len(c) && isVar(c[i]); i++ {
			f.Vars = append(f.Vars, varName(c[i]))
		}
		if len(f.Vars) == 0 {
			return errAt(p, "FOR needs a loop variable, as in FOR    ${i}    IN RANGE    10")
		}
		if i >= len(c) {
			return errAt(p, "FOR needs IN or IN RANGE after its variables")
		}
		switch c[i] {
		case "IN":
		case "IN RANGE":
			f.Range = true
		case "IN ENUMERATE", "IN ZIP":
			return errAt(p, "FOR ... %s is not in the subset; use IN or IN RANGE", c[i])
		default:
			return errAt(p, "FOR needs IN or IN RANGE after its variables, not %q", c[i])
		}
		f.Items = c[i+1:]
		if f.Range && (len(f.Vars) != 1 || len(f.Items) < 1 || len(f.Items) > 3) {
			return errAt(p, "FOR ... IN RANGE takes one variable and one to three numbers: end, start end, or start end step")
		}
		b.add(f)
		b.open = append(b.open, openStmt{f, &f.Body})
		return nil

	case "IF":
		if len(c) != 2 {
			if len(c) > 2 {
				return errAt(p, "inline IF is not in the subset; write IF, the step on its own line, and END")
			}
			return errAt(p, "IF needs a condition")
		}
		s := &If{Pos: p, Branches: []Branch{{Pos: p, Cond: c[1]}}}
		b.add(s)
		b.open = append(b.open, openStmt{s, &s.Branches[0].Body})
		return nil

	case "ELSE IF", "ELSE":
		n := len(b.open)
		var s *If
		if n > 0 {
			s, _ = b.open[n-1].s.(*If)
		}
		if s == nil {
			return errAt(p, "%s without IF", head)
		}
		if last := s.Branches[len(s.Branches)-1]; last.Cond == "" {
			return errAt(p, "%s after ELSE", head)
		}
		br := Branch{Pos: p}
		if head == "ELSE IF" {
			if len(c) != 2 {
				return errAt(p, "ELSE IF needs one condition")
			}
			br.Cond = c[1]
		} else if len(c) != 1 {
			return errAt(p, "ELSE takes nothing on its line; the steps go below it")
		}
		s.Branches = append(s.Branches, br)
		b.open[n-1].body = &s.Branches[len(s.Branches)-1].Body
		return nil

	case "WHILE":
		w := &While{Pos: p}
		if len(c) < 2 {
			return errAt(p, "WHILE needs a condition")
		}
		w.Cond = c[1]
		for _, opt := range c[2:] {
			k, v, ok := strings.Cut(opt, "=")
			switch {
			case ok && k == "limit":
				w.Limit = v
			case ok && (k == "on_limit" || k == "on_limit_message"):
				return errAt(p, "WHILE %s is not in the subset: reaching the limit fails the test", k)
			default:
				return errAt(p, "WHILE takes a condition and limit=, not %q", opt)
			}
		}
		if w.Limit == "" {
			return errAt(p, "WHILE needs limit=, a count or a duration: a procedure's loops end")
		}
		if strings.EqualFold(w.Limit, "NONE") {
			return errAt(p, "WHILE limit=NONE is not in the subset: a procedure's loops end")
		}
		b.add(w)
		b.open = append(b.open, openStmt{w, &w.Body})
		return nil

	case "END":
		if len(c) != 1 {
			return errAt(p, "END takes nothing else on its line")
		}
		if len(b.open) == 0 {
			return errAt(p, "END without FOR, IF or WHILE")
		}
		b.open = b.open[:len(b.open)-1]
		return nil

	case "BREAK", "CONTINUE":
		if len(c) != 1 {
			return errAt(p, "%s takes nothing else on its line", head)
		}
		if head == "BREAK" {
			b.add(&Break{Pos: p})
		} else {
			b.add(&Continue{Pos: p})
		}
		return nil

	case "RETURN":
		b.add(&Return{Pos: p, Values: c[1:]})
		return nil
	}

	call := &Call{Pos: p}
	i := 0
	for ; i < len(c) && isAssign(c[i]); i++ {
		if !strings.HasPrefix(c[i], "$") {
			return errAt(p, "%s: only ${scalar} variables are assigned in the subset", c[i])
		}
		call.Assign = append(call.Assign, varName(strings.TrimSuffix(strings.TrimSpace(c[i]), "=")))
	}
	if i >= len(c) {
		return errAt(p, "%s is assigned, and no keyword follows", c[0])
	}
	call.Name, call.Args = c[i], c[i+1:]
	b.add(call)
	return nil
}

func (b *bodyParser) setting(p Pos, head string, rest []string) error {
	blk := b.block
	switch norm(head[1 : len(head)-1]) {
	case "documentation":
		blk.Doc = strings.Join(rest, " ")
	case "tags":
		blk.Tags = rest
	case "arguments":
		if !blk.Keyword {
			return errAt(p, "[Arguments] belongs to a keyword, not a test")
		}
		for _, a := range rest {
			name, def, hasDef := strings.Cut(a, "=")
			if !strings.HasPrefix(name, "${") || !strings.HasSuffix(name, "}") {
				if strings.HasPrefix(name, "@{") || strings.HasPrefix(name, "&{") {
					return errAt(p, "%s: variable arguments are not in the subset; name each argument", name)
				}
				return errAt(p, "%q is not an argument; write ${name} or ${name}=default", a)
			}
			ad := ArgDef{Name: varName(name), Written: name}
			if hasDef {
				ad.Default = &def
			}
			blk.Args = append(blk.Args, ad)
		}
	case "setup", "teardown":
		if len(rest) == 0 {
			return nil
		}
		c := &Call{Pos: p, Name: rest[0], Args: rest[1:]}
		if norm(head) == "[setup]" {
			blk.Setup = c
		} else {
			blk.Teardown = c
		}
	case "timeout":
		if len(rest) != 1 {
			return errAt(p, "[Timeout] takes one duration")
		}
		blk.Timeout, blk.TimeoutP = rest[0], p
	case "template":
		return errAt(p, "[Template]: templates are not in the subset; write each step out")
	case "return":
		return errAt(p, "[Return] is old syntax; write RETURN")
	default:
		return errAt(p, "%s is not a setting of the subset; the settings are [Arguments] [Setup] [Teardown] "+
			"[Documentation] [Tags] [Timeout]", head)
	}
	return nil
}

// isVar says whether a cell is exactly one ${name}.
func isVar(c string) bool {
	return len(c) > 3 && c[0] == '$' && c[1] == '{' && c[len(c)-1] == '}' && !strings.ContainsAny(c[2:len(c)-1], "{}")
}

func isAssign(c string) bool {
	c = strings.TrimSpace(c)
	c = strings.TrimSuffix(c, "=")
	c = strings.TrimSpace(c)
	return len(c) > 3 && strings.ContainsRune("$@&", rune(c[0])) && c[1] == '{' && c[len(c)-1] == '}' &&
		!strings.ContainsAny(c[2:len(c)-1], "{}")
}

// varName is a variable's name from ${name}, normalised.
func varName(c string) string { return norm(c[2 : len(c)-1]) }

// norm is how Robot compares names: case, spaces and underscores do not
// count.
func norm(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == ' ' || r == '_' || r == '\t' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
