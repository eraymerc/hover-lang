package ast

import (
	"strings"

	token "hover/compiler/token"
)

type NumberExpression struct {
	Token token.Token
	Value string
}

func (n *NumberExpression) expressionNode()      {}
func (n *NumberExpression) TokenLiteral() string { return n.Token.Literal }
func (n *NumberExpression) Line() int            { return n.Token.Line }
func (n *NumberExpression) String() string       { return n.Value }

type IdentifierExpression struct {
	Token token.Token
	Value string
}

func (i *IdentifierExpression) expressionNode()      {}
func (i *IdentifierExpression) TokenLiteral() string { return i.Token.Literal }
func (i *IdentifierExpression) Line() int            { return i.Token.Line }
func (i *IdentifierExpression) String() string       { return i.Value }

type BinaryExpression struct {
	Token    token.Token
	Left     Expression
	Operator string
	Right    Expression

	// IsFieldAccess is set by semantic analysis (never by the parser) when
	// Operator == "." resolves to real struct member access — Left's type
	// is a declared struct and Right names one of its fields — as opposed
	// to an aliased-import-qualified call (M.sin) or the loose fallback
	// behavior of "." on anything else. Codegen reads this flag to decide
	// whether to emit real "left.field" C++ member access or fall back to
	// its existing discard-the-left behavior. Kept as a tag on the existing
	// node, rather than a separate FieldAccessExpression node, because the
	// parser cannot tell the two cases apart at parse time (both are just
	// IDENT DOT IDENT) — only name resolution in semantic analysis can.
	IsFieldAccess bool
}

func (b *BinaryExpression) expressionNode()      {}
func (b *BinaryExpression) TokenLiteral() string { return b.Token.Literal }
func (b *BinaryExpression) Line() int            { return b.Token.Line }
func (b *BinaryExpression) String() string {
	left := "<nil>"
	if b.Left != nil {
		left = b.Left.String()
	}
	right := "<nil>"
	if b.Right != nil {
		right = b.Right.String()
	}
	return "(" + left + " " + b.Operator + " " + right + ")"
}

type CallExpression struct {
	Token     token.Token
	Function  Expression
	Arguments []Expression
}

func (c *CallExpression) expressionNode()      {}
func (c *CallExpression) TokenLiteral() string { return c.Token.Literal }
func (c *CallExpression) Line() int            { return c.Token.Line }
func (c *CallExpression) String() string {
	fn := "<nil>"
	if c.Function != nil {
		fn = c.Function.String()
	}
	return fn + "(...)"
}

type IndexExpression struct {
	Token token.Token
	Left  Expression
	Index Expression
}

func (ie *IndexExpression) expressionNode()      {}
func (ie *IndexExpression) TokenLiteral() string { return ie.Token.Literal }
func (ie *IndexExpression) Line() int            { return ie.Token.Line }
func (ie *IndexExpression) String() string {
	left := "<nil>"
	if ie.Left != nil {
		left = ie.Left.String()
	}
	idx := "<nil>"
	if ie.Index != nil {
		idx = ie.Index.String()
	}
	return left + "[" + idx + "]"
}

type UnaryExpression struct {
	Token    token.Token
	Operator string
	Right    Expression
}

func (u *UnaryExpression) expressionNode()      {}
func (u *UnaryExpression) TokenLiteral() string { return u.Token.Literal }
func (u *UnaryExpression) Line() int            { return u.Token.Line }
func (u *UnaryExpression) String() string {
	right := "<nil>"
	if u.Right != nil {
		right = u.Right.String()
	}
	return "(" + u.Operator + right + ")"
}

// StructLiteralExpression constructs a struct value with named fields:
//
//	Point{x: 1.0, y: 2.0}
//
// Fields may appear in any order — they're matched by name, not position,
// both in semantic checking and in codegen (which emits a C++ designated
// initializer).
type StructLiteralExpression struct {
	Token    token.Token // the type-name IDENT token
	TypeName string
	Fields   []StructFieldInit
}

type StructFieldInit struct {
	Name  string
	Value Expression
}

func (s *StructLiteralExpression) expressionNode()      {}
func (s *StructLiteralExpression) TokenLiteral() string { return s.Token.Literal }
func (s *StructLiteralExpression) Line() int            { return s.Token.Line }
func (s *StructLiteralExpression) String() string {
	parts := make([]string, len(s.Fields))
	for i, f := range s.Fields {
		val := "<nil>"
		if f.Value != nil {
			val = f.Value.String()
		}
		parts[i] = f.Name + ": " + val
	}
	return s.TypeName + "{" + strings.Join(parts, ", ") + "}"
}

type ArrayExpression struct {
	Token    token.Token
	Elements []Expression
}

func (a *ArrayExpression) expressionNode()      {}
func (a *ArrayExpression) TokenLiteral() string { return a.Token.Literal }
func (a *ArrayExpression) Line() int            { return a.Token.Line }
func (a *ArrayExpression) String() string {
	return "{...}"
}
