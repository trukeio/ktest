package expr

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type tokKind uint8

const (
	tEOF tokKind = iota
	tNum
	tStr
	tName   // a.b.c
	tVar    // $a.b or ${a.b}, name without the dollar
	tOp     // == != < <= > >= + - * /
	tLParen // (
	tRParen // )
	tComma
	tAnd
	tOr
	tNot
	tTrue
	tFalse
)

type token struct {
	kind tokKind
	col  int // 1-based, in runes, for the error sentence
	text string
	num  float64
	unit string // the suffix of a number as written, "" for a bare one
}

// Error is a refusal, with the column of what was refused. A caller that
// knows the file and line, a contract loader or a procedure, puts them in
// front of it.
type Error struct {
	Col int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("column %d: %s", e.Col, e.Msg) }

func errAt(col int, format string, a ...any) *Error {
	return &Error{Col: col, Msg: fmt.Sprintf(format, a...)}
}

func isUnitRune(r rune) bool {
	return unicode.IsLetter(r) || r == '°' || r == '%' || r == 'Ω' || r == 'µ'
}

func isNameStart(r rune) bool { return r == '_' || r < 128 && unicode.IsLetter(r) }
func isNameRune(r rune) bool {
	return r == '_' || r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

// lex splits an expression into tokens. The operators other languages spell
// differently are refused here, by name, since they are the mistakes people
// coming from C, JavaScript or the outline's first draft will make.
func lex(src string) ([]token, error) {
	rs := []rune(src)
	var out []token
	i := 0
	for i < len(rs) {
		r := rs[i]
		col := i + 1
		switch {
		case unicode.IsSpace(r):
			i++

		case unicode.IsDigit(r) || r == '.' && i+1 < len(rs) && unicode.IsDigit(rs[i+1]):
			j := i
			for j < len(rs) && unicode.IsDigit(rs[j]) {
				j++
			}
			if j < len(rs) && rs[j] == '.' {
				j++
				for j < len(rs) && unicode.IsDigit(rs[j]) {
					j++
				}
			}
			// An exponent only when digits follow, so that a unit beginning
			// with e, were there one, could not be eaten.
			if j < len(rs) && (rs[j] == 'e' || rs[j] == 'E') {
				k := j + 1
				if k < len(rs) && (rs[k] == '+' || rs[k] == '-') {
					k++
				}
				if k < len(rs) && unicode.IsDigit(rs[k]) {
					for k < len(rs) && unicode.IsDigit(rs[k]) {
						k++
					}
					j = k
				}
			}
			v, err := strconv.ParseFloat(string(rs[i:j]), 64)
			if err != nil {
				return nil, errAt(col, "%q is not a number", string(rs[i:j]))
			}
			u := j
			for u < len(rs) && isUnitRune(rs[u]) {
				u++
			}
			out = append(out, token{kind: tNum, col: col, text: string(rs[i:u]), num: v, unit: string(rs[j:u])})
			i = u

		case r == '"' || r == '\'':
			var b strings.Builder
			j := i + 1
			for ; j < len(rs) && rs[j] != r; j++ {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				b.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, errAt(col, "the string that starts here is not closed")
			}
			out = append(out, token{kind: tStr, col: col, text: b.String()})
			i = j + 1

		case r == '$':
			j := i + 1
			braced := j < len(rs) && rs[j] == '{'
			if braced {
				j++
			}
			name, k := lexName(rs, j)
			if name == "" {
				return nil, errAt(col, "a $ must be followed by a variable name, as in $x or ${x}")
			}
			if braced {
				if k >= len(rs) || rs[k] != '}' {
					return nil, errAt(col, "${%s is not closed with }", name)
				}
				k++
			}
			out = append(out, token{kind: tVar, col: col, text: name})
			i = k

		case isNameStart(r):
			name, k := lexName(rs, i)
			t := token{kind: tName, col: col, text: name}
			switch name {
			case "and":
				t.kind = tAnd
			case "or":
				t.kind = tOr
			case "not":
				t.kind = tNot
			case "True":
				t.kind = tTrue
			case "False":
				t.kind = tFalse
			case "true", "false":
				return nil, errAt(col, "%s: write %s, with a capital, as Robot and Python do",
					name, strings.ToUpper(name[:1])+name[1:])
			case "AND", "OR", "NOT":
				return nil, errAt(col, "%s: the logic operators are written and, or, not", name)
			}
			out = append(out, t)
			i = k

		default:
			two := ""
			if i+1 < len(rs) {
				two = string(rs[i : i+2])
			}
			switch two {
			case "==", "!=", "<=", ">=":
				out = append(out, token{kind: tOp, col: col, text: two})
				i += 2
				continue
			case "&&":
				return nil, errAt(col, "&&: write and")
			case "||":
				return nil, errAt(col, "||: write or")
			case "**":
				return nil, errAt(col, "**: there is no power operator")
			case "//":
				return nil, errAt(col, "//: there is no integer division; write /")
			}
			switch r {
			case '<', '>', '+', '-', '*', '/':
				out = append(out, token{kind: tOp, col: col, text: string(r)})
			case '(':
				out = append(out, token{kind: tLParen, col: col, text: "("})
			case ')':
				out = append(out, token{kind: tRParen, col: col, text: ")"})
			case ',':
				out = append(out, token{kind: tComma, col: col, text: ","})
			case '!':
				return nil, errAt(col, "!: write not")
			case '=':
				return nil, errAt(col, "=: there is no assignment; to compare, write ==")
			case '%':
				return nil, errAt(col, "%%: there is no remainder operator; %% is a unit, written straight after a number")
			default:
				return nil, errAt(col, "%q is not part of an expression", string(r))
			}
			i++
		}
	}
	out = append(out, token{kind: tEOF, col: len(rs) + 1})
	return out, nil
}

// lexName reads a dotted name from rs[i:]. A part after a dot may begin with
// a digit, since channel names do: can0.cyclic.123.
func lexName(rs []rune, i int) (string, int) {
	if i >= len(rs) || !isNameStart(rs[i]) {
		return "", i
	}
	j := i
	for j < len(rs) && isNameRune(rs[j]) {
		j++
	}
	for j+1 < len(rs) && rs[j] == '.' && isNameRune(rs[j+1]) {
		j++
		for j < len(rs) && isNameRune(rs[j]) {
			j++
		}
	}
	return string(rs[i:j]), j
}
