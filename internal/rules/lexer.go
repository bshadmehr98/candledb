package rules

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Pos is a position in the rules source, for error messages.
type Pos struct{ Line, Col int }

func (p Pos) String() string { return fmt.Sprintf("%d:%d", p.Line, p.Col) }

// SyntaxError is returned by Parse when the source is not valid.
type SyntaxError struct {
	Pos Pos
	Msg string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("rules:%s: %s", e.Pos, e.Msg) }

type tokenKind int

const (
	tokEOF    tokenKind = iota
	tokIdent            // request, allow, uid
	tokInt              // 42
	tokFloat            // 1.5
	tokString           // "text" or 'text'
	tokPath             // /users/{uid} (only directly after the keyword match)
	tokPunct            // operators and brackets
)

type token struct {
	kind tokenKind
	text string // for tokString this is the decoded value
	pos  Pos
}

func (t token) String() string {
	if t.kind == tokEOF {
		return "end of file"
	}
	return fmt.Sprintf("%q", t.text)
}

// punctuation lists the operators, longest first so that "==" is tried
// before "=".
var punctuation = []string{
	"==", "!=", "<=", ">=", "&&", "||",
	"{", "}", "(", ")", "[", "]", ",", ";", ":", ".",
	"<", ">", "!", "+", "-", "*", "/", "%", "=",
}

// lexer turns source text into tokens. It walks the input once, keeping
// only its current offset and the line and column that offset corresponds to.
type lexer struct {
	src  string
	off  int
	line int
	col  int
}

// lex splits the whole source into tokens, ending with a tokEOF token.
func lex(src string) ([]token, error) {
	lx := &lexer{src: src, line: 1, col: 1}
	var tokens []token
	afterDot := false
	for {
		tok, err := lx.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, tok)
		if tok.kind == tokEOF {
			return tokens, nil
		}
		// A path pattern has its own syntax ("/" and "{" mean something
		// else there), so it is read as one token right after "match".
		// After a dot, "match" is a field name (resource.data.match), not
		// the keyword.
		isKeyword := tok.kind == tokIdent && tok.text == "match" && !afterDot
		afterDot = tok.kind == tokPunct && tok.text == "."
		if isKeyword {
			path, err := lx.path()
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, path)
		}
	}
}

func (lx *lexer) errorf(pos Pos, format string, args ...any) error {
	return &SyntaxError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

func (lx *lexer) pos() Pos { return Pos{lx.line, lx.col} }

// peek returns the next rune without consuming it, or 0 at the end.
func (lx *lexer) peek() rune {
	if lx.off >= len(lx.src) {
		return 0
	}
	r, _ := utf8.DecodeRuneInString(lx.src[lx.off:])
	return r
}

func (lx *lexer) advance() rune {
	r, size := utf8.DecodeRuneInString(lx.src[lx.off:])
	lx.off += size
	if r == '\n' {
		lx.line++
		lx.col = 1
	} else {
		lx.col++
	}
	return r
}

func (lx *lexer) skipSpaceAndComments() {
	for lx.off < len(lx.src) {
		switch {
		case unicode.IsSpace(lx.peek()):
			lx.advance()
		case strings.HasPrefix(lx.src[lx.off:], "//"):
			for lx.off < len(lx.src) && lx.peek() != '\n' {
				lx.advance()
			}
		default:
			return
		}
	}
}

func (lx *lexer) next() (token, error) {
	lx.skipSpaceAndComments()
	pos := lx.pos()
	if lx.off >= len(lx.src) {
		return token{kind: tokEOF, pos: pos}, nil
	}
	r := lx.peek()
	switch {
	case r == '_' || unicode.IsLetter(r):
		start := lx.off
		for r := lx.peek(); r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r); r = lx.peek() {
			lx.advance()
		}
		return token{tokIdent, lx.src[start:lx.off], pos}, nil
	case r >= '0' && r <= '9':
		return lx.number(pos), nil
	case r == '"' || r == '\'':
		return lx.str(pos)
	}
	for _, p := range punctuation {
		if strings.HasPrefix(lx.src[lx.off:], p) {
			for range p {
				lx.advance()
			}
			return token{tokPunct, p, pos}, nil
		}
	}
	return token{}, lx.errorf(pos, "unexpected character %q", r)
}

func (lx *lexer) number(pos Pos) token {
	start := lx.off
	digits := func() {
		for r := lx.peek(); r >= '0' && r <= '9'; r = lx.peek() {
			lx.advance()
		}
	}
	digits()
	kind := tokInt
	// A dot only belongs to the number when a digit follows, so "1.size()"
	// style member access on a literal is not misread.
	if lx.peek() == '.' && lx.off+1 < len(lx.src) && lx.src[lx.off+1] >= '0' && lx.src[lx.off+1] <= '9' {
		kind = tokFloat
		lx.advance()
		digits()
	}
	return token{kind, lx.src[start:lx.off], pos}
}

func (lx *lexer) str(pos Pos) (token, error) {
	quote := lx.advance()
	var b strings.Builder
	for {
		if lx.off >= len(lx.src) || lx.peek() == '\n' {
			return token{}, lx.errorf(pos, "string is not closed")
		}
		r := lx.advance()
		switch r {
		case quote:
			return token{tokString, b.String(), pos}, nil
		case '\\':
			if lx.off >= len(lx.src) {
				return token{}, lx.errorf(pos, "string is not closed")
			}
			switch esc := lx.advance(); esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '"', '\'':
				b.WriteRune(esc)
			default:
				// Unknown escapes are kept as written, which lets regular
				// expressions such as "\d+" be typed without doubling.
				b.WriteByte('\\')
				b.WriteRune(esc)
			}
		default:
			b.WriteRune(r)
		}
	}
}

// path reads a match pattern such as /users/{uid}/posts/{post=**}.
func (lx *lexer) path() (token, error) {
	lx.skipSpaceAndComments()
	pos := lx.pos()
	start := lx.off
	if lx.peek() != '/' {
		return token{}, lx.errorf(pos, "expected a path starting with / after match")
	}
	for lx.peek() == '/' {
		lx.advance()
		if lx.peek() == '{' {
			for lx.off < len(lx.src) && lx.peek() != '}' && lx.peek() != '\n' {
				lx.advance()
			}
			if lx.peek() != '}' {
				return token{}, lx.errorf(pos, "wildcard in path is not closed")
			}
			lx.advance()
			continue
		}
		for r := lx.peek(); r != 0 && r != '/' && r != '{' && !unicode.IsSpace(r); r = lx.peek() {
			lx.advance()
		}
	}
	return token{tokPath, lx.src[start:lx.off], pos}, nil
}
