package token

// Type uses an integer for ultra-fast CPU comparisons
type Type int

// Token struct holds the exact position for modern error reporting
type Token struct {
	Type    Type
	Literal string
	Line    int
	Column  int
}

// iota auto-increments the integers (0, 1, 2, 3...)
const (
	ILLEGAL Type = iota
	EOF

	IDENT
	NUMBER
	SUFFIX // p, n, u, m, k, Meg, G
	STRING // "..." — currently only used by import statements

	// Operators
	ASSIGN   // =
	PLUS     // +
	MINUS    // -
	ASTERISK // *
	DIV      // /
	POW      // **
	BANG     // !
	DOT      // .
	MOD      // %

	// Bitwise
	BIT_AND // &
	BIT_OR  // |
	BIT_XOR // ^
	BIT_NOT // ~
	SHL     // <<
	SHR     // >>

	// Comparators & Logical
	LT     // <
	GT     // >
	EQ     // ==
	NOT_EQ // !=
	LTE    // <=
	GTE    // >=
	AND    // &&
	OR     // ||

	// Delimiters
	LPAREN   // (
	RPAREN   // )
	LBRACKET // [
	RBRACKET // ]
	LBRACE   // {
	RBRACE   // }
	COMMA    // ,
	SEMI     // ;

	// Keywords
	MODULE
	ANALOG
	DIGITAL
	FUNC
	STATE
	IF
	ELSE
	WHILE
	RETURN
	INPUT
	OUTPUT
	IMPORT
	AS

	// Types
	WIRE
	DOUBLE
	INT
	FLOAT
	UNSIGNED
)

// tokens maps the integer back to a string for easy terminal debugging
var tokens = [...]string{
	ILLEGAL:  "ILLEGAL",
	EOF:      "EOF",
	IDENT:    "IDENT",
	NUMBER:   "NUMBER",
	SUFFIX:   "SUFFIX",
	STRING:   "STRING",
	ASSIGN:   "=",
	PLUS:     "+",
	MINUS:    "-",
	ASTERISK: "*",
	DIV:      "/",
	POW:      "**",
	MOD:      "%",
	BIT_AND:  "&",
	BIT_OR:   "|",
	BIT_XOR:  "^",
	BIT_NOT:  "~",
	SHL:      "<<",
	SHR:      ">>",
	BANG:     "!",
	DOT:      ".",
	LT:       "<",
	GT:       ">",
	EQ:       "==",
	NOT_EQ:   "!=",
	LTE:      "<=",
	GTE:      ">=",
	AND:      "&&",
	OR:       "||",
	LPAREN:   "(",
	RPAREN:   ")",
	LBRACKET: "[",
	RBRACKET: "]",
	LBRACE:   "{",
	RBRACE:   "}",
	COMMA:    ",",
	SEMI:     ";",
	MODULE:   "MODULE",
	FUNC:     "FUNC",
	STATE:    "STATE",
	IF:       "IF",
	ELSE:     "ELSE",
	WHILE:    "WHILE",
	RETURN:   "RETURN",
	INPUT:    "INPUT",
	OUTPUT:   "OUTPUT",
	IMPORT:   "IMPORT",
	AS:       "AS",
	WIRE:     "WIRE",
	DOUBLE:   "DOUBLE",
	INT:      "INT",
	FLOAT:    "FLOAT",
	UNSIGNED: "UNSIGNED",
	ANALOG:   "ANALOG",
	DIGITAL:  "DIGITAL",
}

// String allows fmt.Println(token.Type) to print the human-readable name
func (t Type) String() string {
	if t >= 0 && int(t) < len(tokens) {
		return tokens[t]
	}
	return "UNKNOWN"
}

// keywords maps exact string matches to their token Type
var keywords = map[string]Type{
	"module":   MODULE,
	"func":     FUNC,
	"state":    STATE,
	"if":       IF,
	"else":     ELSE,
	"while":    WHILE,
	"return":   RETURN,
	"input":    INPUT,
	"output":   OUTPUT,
	"import":   IMPORT,
	"as":       AS,
	"wire":     WIRE,
	"double":   DOUBLE,
	"int":      INT,
	"float":    FLOAT,
	"unsigned": UNSIGNED,
	"analog":   ANALOG,
	"digital":  DIGITAL,
}

// LookupIdent checks if an identifier is actually a reserved keyword
func LookupIdent(ident string) Type {
	if tok, ok := keywords[ident]; ok {
		return tok
	}
	return IDENT
}
