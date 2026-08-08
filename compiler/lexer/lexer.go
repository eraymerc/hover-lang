package lexer

import (
	"hover/compiler/token"
	"strings"
	"unicode"
)

type Lexer struct {
	input        string
	position     int
	readPosition int
	ch           byte
	line         int
	column       int
}

func New(input string) *Lexer {
	l := &Lexer{input: input, line: 1, column: 0}
	l.readChar()
	return l
}

func (l *Lexer) readChar() {
	if l.ch == '\n' {
		l.line++
		l.column = 0
	}

	if l.readPosition >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPosition]
	}
	l.position = l.readPosition
	l.readPosition++
	l.column++
}

func (l *Lexer) NextToken() token.Token {
	var tok token.Token

	l.skipWhitespace()

	// Snapshot the starting position of this token
	startLine := l.line
	startCol := l.column

	switch l.ch {
	case '=':
		if l.peekChar() == '=' {
			l.readChar()
			tok = token.Token{Type: token.EQ, Literal: "==", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.ASSIGN, l.ch, startLine, startCol)
		}
	case '!':
		if l.peekChar() == '=' {
			l.readChar()
			tok = token.Token{Type: token.NOT_EQ, Literal: "!=", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.BANG, l.ch, startLine, startCol)
		}
	case '+':
		tok = newToken(token.PLUS, l.ch, startLine, startCol)
	case '-':
		tok = newToken(token.MINUS, l.ch, startLine, startCol)
	case '*':
		if l.peekChar() == '*' {
			l.readChar()
			tok = token.Token{Type: token.POW, Literal: "**", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.ASTERISK, l.ch, startLine, startCol)
		}
	case '%':
		tok = newToken(token.MOD, l.ch, startLine, startCol)
	case '/':
		if l.peekChar() == '/' {
			// Single-line comment: skip everything until newline or EOF
			for l.ch != '\n' && l.ch != 0 {
				l.readChar()
			}
			return l.NextToken() // Re-enter to get the next real token
		}
		tok = newToken(token.DIV, l.ch, startLine, startCol)
	case '&':
		if l.peekChar() == '&' {
			l.readChar()
			tok = token.Token{Type: token.AND, Literal: "&&", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.BIT_AND, l.ch, startLine, startCol)
		}
	case '|':
		if l.peekChar() == '|' {
			l.readChar()
			tok = token.Token{Type: token.OR, Literal: "||", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.BIT_OR, l.ch, startLine, startCol)
		}
	case '^':
		tok = newToken(token.BIT_XOR, l.ch, startLine, startCol)
	case '~':
		tok = newToken(token.BIT_NOT, l.ch, startLine, startCol)
	case '<':
		if l.peekChar() == '=' {
			l.readChar()
			tok = token.Token{Type: token.LTE, Literal: "<=", Line: startLine, Column: startCol}
		} else if l.peekChar() == '<' {
			l.readChar()
			tok = token.Token{Type: token.SHL, Literal: "<<", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.LT, l.ch, startLine, startCol)
		}
	case '>':
		if l.peekChar() == '=' {
			l.readChar()
			tok = token.Token{Type: token.GTE, Literal: ">=", Line: startLine, Column: startCol}
		} else if l.peekChar() == '>' {
			l.readChar()
			tok = token.Token{Type: token.SHR, Literal: ">>", Line: startLine, Column: startCol}
		} else {
			tok = newToken(token.GT, l.ch, startLine, startCol)
		}
	case ';':
		tok = newToken(token.SEMI, l.ch, startLine, startCol)
	case ',':
		tok = newToken(token.COMMA, l.ch, startLine, startCol)
	case ':':
		tok = newToken(token.COLON, l.ch, startLine, startCol)
	case '(':
		tok = newToken(token.LPAREN, l.ch, startLine, startCol)
	case ')':
		tok = newToken(token.RPAREN, l.ch, startLine, startCol)
	case '[':
		tok = newToken(token.LBRACKET, l.ch, startLine, startCol)
	case ']':
		tok = newToken(token.RBRACKET, l.ch, startLine, startCol)
	case '{':
		tok = newToken(token.LBRACE, l.ch, startLine, startCol)
	case '}':
		tok = newToken(token.RBRACE, l.ch, startLine, startCol)
	case '.':
		tok = newToken(token.DOT, l.ch, startLine, startCol)
	case '"':
		str := l.readString()
		tok = token.Token{Type: token.STRING, Literal: str, Line: startLine, Column: startCol}
	case 0:
		tok.Literal = ""
		tok.Type = token.EOF
		tok.Line = startLine
		tok.Column = startCol
	default:
		if isLetter(l.ch) {
			tok.Literal = l.readIdentifier()
			tok.Type = token.LookupIdent(tok.Literal)
			tok.Line = startLine
			tok.Column = startCol
			return tok
		} else if isDigit(l.ch) {
			tok.Literal = l.readNumber()
			tok.Type = token.NUMBER
			tok.Line = startLine
			tok.Column = startCol
			return tok
		} else {
			tok = newToken(token.ILLEGAL, l.ch, startLine, startCol)
		}
	}

	l.readChar()
	return tok
}

func (l *Lexer) skipWhitespace() {
	for l.ch == ' ' || l.ch == '\t' || l.ch == '\n' || l.ch == '\r' {
		l.readChar()
	}
}

func (l *Lexer) readIdentifier() string {
	position := l.position
	for isLetter(l.ch) || isDigit(l.ch) {
		l.readChar()
	}
	return l.input[position:l.position]
}

// readString consumes a double-quoted string literal and returns its
// contents without the surrounding quotes. Assumes l.ch == '"' on entry.
// No escape sequence support — import paths don't need them. Leaves l.ch
// sitting on the closing '"'; the shared l.readChar() at the end of
// NextToken() consumes it, the same way EQ/NOT_EQ/AND/OR rely on that
// final readChar() to advance past their second character.
func (l *Lexer) readString() string {
	l.readChar() // consume opening '"'
	position := l.position
	for l.ch != '"' && l.ch != 0 {
		l.readChar()
	}
	return l.input[position:l.position]
}

// readNumber handles integers, floats, scientific notation (1e-4, 2.5E+10),
// and engineering suffixes (2m, 20k, etc.)
func (l *Lexer) readNumber() string {
	position := l.position
	hasDot := false

	for isDigit(l.ch) || l.ch == '.' {
		if l.ch == '.' {
			if hasDot {
				break // Stop consuming if we encounter a second decimal point
			}
			hasDot = true
		}
		l.readChar()
	}

	// Check for scientific notation: e/E followed by an optional sign and
	// at least one digit (1e-4, 2.5E+10, 3e5). Checked BEFORE engineering
	// suffixes since 'e' would otherwise never be reached — the suffix
	// check below only recognizes a fixed single-character set that does
	// not include 'e'/'E', so there's no ambiguity between the two: a
	// number is read as scientific notation if 'e'/'E' is followed by a
	// valid exponent, and falls through to the engineering-suffix check
	// otherwise (which still only matches "munpkG"/"Meg" — none starting
	// with 'e'/'E', so the two paths never compete for the same input).
	if l.ch == 'e' || l.ch == 'E' {
		// Determine how far the sign+digits run extends before committing,
		// since a bare 'e' with no following digits (e.g. the identifier
		// "e" itself appearing right after a number with no space, which
		// would be unusual Hover source but shouldn't silently consume
		// part of an identifier) must NOT be treated as an exponent.
		peekPos := l.readPosition
		checkPos := peekPos
		if checkPos < len(l.input) && (l.input[checkPos] == '+' || l.input[checkPos] == '-') {
			checkPos++
		}
		hasExponentDigits := checkPos < len(l.input) && isDigit(l.input[checkPos])

		if hasExponentDigits {
			l.readChar() // consume 'e'/'E'
			if l.ch == '+' || l.ch == '-' {
				l.readChar() // consume sign
			}
			for isDigit(l.ch) {
				l.readChar()
			}
			// Scientific notation numbers are not followed by an
			// engineering suffix — "1e-4m" is not a meaningful Hover
			// literal, so no suffix check runs after this branch.
			return l.input[position:l.position]
		}
	}

	// Check for engineering suffixes (f, p, n, u, m, k, Meg, G)
	// We handle "Meg" specifically as it's multiple chars.
	//
	// 'f' (femto, 1e-15) is here because gate and junction capacitances are
	// routinely written in femtofarads — a MOSFET model's Cgd is 10f, not
	// 0.01p — and without it the lexer stopped at the digits, handed the
	// parser a stray identifier, and produced a cascade of syntax errors
	// nowhere near the real mistake.
	if l.ch == 'M' && l.peekChar() == 'e' {
		l.readChar() // M
		l.readChar() // e
		l.readChar() // g
	} else if strings.ContainsRune("fmunpkG", rune(l.ch)) {
		l.readChar()
	}

	return l.input[position:l.position]
}

func isLetter(ch byte) bool {
	return unicode.IsLetter(rune(ch)) || ch == '_'
}

func isDigit(ch byte) bool {
	return unicode.IsDigit(rune(ch))
}

func (l *Lexer) peekChar() byte {
	if l.readPosition >= len(l.input) {
		return 0
	}
	return l.input[l.readPosition]
}

func newToken(tokenType token.Type, ch byte, line int, column int) token.Token {
	return token.Token{Type: tokenType, Literal: string(ch), Line: line, Column: column}
}
