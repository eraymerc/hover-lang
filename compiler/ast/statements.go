package ast

import (
	"strings"

	token "hover/compiler/token"
)

// ============================================
// GENERIC STATEMENTS
// ============================================

type BlockStatement struct {
	Token token.Token // the '{' token
	Body  []Statement
}

func (b *BlockStatement) statementNode()       {}
func (b *BlockStatement) TokenLiteral() string { return b.Token.Literal }
func (b *BlockStatement) Line() int            { return b.Token.Line }
func (b *BlockStatement) String() string {
	var out strings.Builder
	out.WriteString("{\n")
	for _, s := range b.Body {
		if s != nil {
			out.WriteString("\t" + s.String() + "\n")
		}
	}
	out.WriteString("}")
	return out.String()
}

type ExpressionStatement struct {
	Token      token.Token
	Expression Expression
}

func (es *ExpressionStatement) statementNode()       {}
func (es *ExpressionStatement) TokenLiteral() string { return es.Token.Literal }
func (es *ExpressionStatement) Line() int            { return es.Token.Line }
func (es *ExpressionStatement) String() string {
	if es.Expression != nil {
		return es.Expression.String() + ";"
	}
	return "<nil>;"
}

// ============================================
// CONTROL FLOW STATEMENTS
// ============================================

type IfStatement struct {
	Token        token.Token
	Condition    Expression
	Consequence  *BlockStatement
	Alternatives []*ElseIfBlock
	Alternative  *BlockStatement
}

type ElseIfBlock struct {
	Token     token.Token
	Condition Expression
	Body      *BlockStatement
}

func (is *IfStatement) statementNode()       {}
func (is *IfStatement) TokenLiteral() string { return is.Token.Literal }
func (is *IfStatement) Line() int            { return is.Token.Line }
func (is *IfStatement) String() string {
	var out strings.Builder
	cond := "<nil>"
	if is.Condition != nil {
		cond = is.Condition.String()
	}
	cons := "<nil>"
	if is.Consequence != nil {
		cons = is.Consequence.String()
	}

	out.WriteString("if (" + cond + ") " + cons)
	for _, alt := range is.Alternatives {
		altCond := "<nil>"
		if alt.Condition != nil {
			altCond = alt.Condition.String()
		}
		altBody := "<nil>"
		if alt.Body != nil {
			altBody = alt.Body.String()
		}
		out.WriteString(" else if (" + altCond + ") " + altBody)
	}
	if is.Alternative != nil {
		out.WriteString(" else " + is.Alternative.String())
	}
	return out.String()
}

type WhileStatement struct {
	Token     token.Token
	Condition Expression
	Body      *BlockStatement
}

func (ws *WhileStatement) statementNode()       {}
func (ws *WhileStatement) TokenLiteral() string { return ws.Token.Literal }
func (ws *WhileStatement) Line() int            { return ws.Token.Line }
func (ws *WhileStatement) String() string {
	cond := "<nil>"
	if ws.Condition != nil {
		cond = ws.Condition.String()
	}
	body := "<nil>"
	if ws.Body != nil {
		body = ws.Body.String()
	}
	return "while (" + cond + ") " + body
}

type ReturnStatement struct {
	Token       token.Token
	ReturnValue Expression
}

func (rs *ReturnStatement) statementNode()       {}
func (rs *ReturnStatement) TokenLiteral() string { return rs.Token.Literal }
func (rs *ReturnStatement) Line() int            { return rs.Token.Line }
func (rs *ReturnStatement) String() string {
	out := "return "
	if rs.ReturnValue != nil {
		out += rs.ReturnValue.String()
	}
	return out + ";"
}

// ============================================
// DECLARATIONS & ASSIGNMENTS
// ============================================

type LocalDeclStatement struct {
	Token   token.Token
	IsState bool
	Type    Type
	Decls   []*VarDecl
}

type VarDecl struct {
	Name  string
	Value Expression
}

func (ld *LocalDeclStatement) statementNode()       {}
func (ld *LocalDeclStatement) TokenLiteral() string { return ld.Token.Literal }
func (ld *LocalDeclStatement) Line() int            { return ld.Token.Line }
func (ld *LocalDeclStatement) String() string {
	var out strings.Builder
	if ld.IsState {
		out.WriteString("state ")
	}
	out.WriteString(ld.Type.String() + " ")
	decls := []string{}
	for _, d := range ld.Decls {
		decl := d.Name
		if d.Value != nil {
			decl += " = " + d.Value.String()
		}
		decls = append(decls, decl)
	}
	out.WriteString(strings.Join(decls, ", ") + ";")
	return out.String()
}

type AssignmentStatement struct {
	Token token.Token
	Left  Expression
	Right Expression
}

func (as *AssignmentStatement) statementNode()       {}
func (as *AssignmentStatement) TokenLiteral() string { return as.Token.Literal }
func (as *AssignmentStatement) Line() int            { return as.Token.Line }
func (as *AssignmentStatement) String() string {
	left := "<nil>"
	if as.Left != nil {
		left = as.Left.String()
	}
	right := "<nil>"
	if as.Right != nil {
		right = as.Right.String()
	}
	return left + " = " + right + ";"
}

// ============================================
// TOP LEVEL
// ============================================

type FuncDeclStatement struct {
	Token      token.Token
	ReturnType Type
	Name       string
	Parameters []FuncParam
	Body       *BlockStatement
	IsExtern   bool // `extern func` — no body; defined in an importc header
}

type FuncParam struct {
	Type Type
	Name string
}

func (fd *FuncDeclStatement) statementNode()       {}
func (fd *FuncDeclStatement) TokenLiteral() string { return fd.Token.Literal }
func (fd *FuncDeclStatement) Line() int            { return fd.Token.Line }
func (fd *FuncDeclStatement) String() string {
	body := "<nil>"
	if fd.Body != nil {
		body = fd.Body.String()
	}
	return "func " + fd.ReturnType.String() + " " + fd.Name + "(...) " + body
}

type ModuleDeclStatement struct {
	Token      token.Token
	Name       string
	StaticArgs []DeclStaticArg
	LogicArgs  []DeclLogicArg
	PhysPorts  []string
	Body       *BlockStatement
}

type DeclStaticArg struct {
	Type  Type
	Name  string
	Value Expression
}

type DeclLogicArg struct {
	Direction string
	Type      Type
	Name      string
}

func (md *ModuleDeclStatement) statementNode()       {}
func (md *ModuleDeclStatement) TokenLiteral() string { return md.Token.Literal }
func (md *ModuleDeclStatement) Line() int            { return md.Token.Line }
func (md *ModuleDeclStatement) String() string {
	body := "<nil>"
	if md.Body != nil {
		body = md.Body.String()
	}
	return "module " + md.Name + " <...> (...) [...] " + body
}

type ModuleInstStatement struct {
	Token        token.Token
	InstanceName string
	ModuleName   string
	StaticArgs   []Expression
	LogicArgs    []Expression
	PhysArgs     []Expression
}

func (mi *ModuleInstStatement) statementNode()       {}
func (mi *ModuleInstStatement) TokenLiteral() string { return mi.Token.Literal }
func (mi *ModuleInstStatement) Line() int            { return mi.Token.Line }
func (mi *ModuleInstStatement) String() string {
	return "module " + mi.InstanceName + " = " + mi.ModuleName + "<...>(...)[...];"
}

type PhysicalPrimitiveStatement struct {
	Token    token.Token
	PrimType string
	// Name is the optional user-supplied instance name in
	//     R rsense<1m>() [b, s];
	// Empty for the unnamed form, which stays the common case. A non-empty
	// Name becomes the element's flattened name (prefix + "." + Name) instead
	// of the positional PrimType_<n> the elaborator synthesizes otherwise —
	// and that flattened name is what the runtime keys its branch map by, so
	// naming an element is what makes I() and CCCS/CCVS able to refer to it.
	Name       string
	StaticArgs []Expression
	LogicArgs  []Expression
	PhysArgs   []Expression
}

func (pp *PhysicalPrimitiveStatement) statementNode()       {}
func (pp *PhysicalPrimitiveStatement) TokenLiteral() string { return pp.Token.Literal }
func (pp *PhysicalPrimitiveStatement) Line() int            { return pp.Token.Line }
func (pp *PhysicalPrimitiveStatement) String() string {
	if pp.Name != "" {
		return pp.PrimType + " " + pp.Name + "<...>(...)[...];"
	}
	return pp.PrimType + "<...>(...)[...];"
}

// IsCurrentControlled reports whether this primitive's LAST [] entry is a
// controlling-element reference rather than a wire terminal.
//
// CCCS/CCVS are the only primitives whose control input is a current, and a
// current in the MNA formulation is a branch variable belonging to a specific
// element — not something addressable by node pair the way VCCS/VCVS's control
// voltage is. So they name the element, SPICE-style:
//
//	CCCS<beta>() [out_p, out_n, vsense];
//
// Lives on the AST node so semantic analysis and the elaborator share one
// definition of the set — the same reason isPhysicalPrimitive lives in one
// place in the parser.
func (pp *PhysicalPrimitiveStatement) IsCurrentControlled() bool {
	switch strings.ToUpper(pp.PrimType) {
	case "CCCS", "F", "CCVS", "H":
		return true
	}
	return false
}

type DirectiveStatement struct {
	Token token.Token
	Name  string
	Args  []Expression
}

func (ds *DirectiveStatement) statementNode()       {}
func (ds *DirectiveStatement) TokenLiteral() string { return ds.Token.Literal }
func (ds *DirectiveStatement) Line() int            { return ds.Token.Line }
func (ds *DirectiveStatement) String() string {
	return "." + ds.Name + "(...);"
}

type ImportStatement struct {
	Token    token.Token
	Path     string
	Alias    string
	IsSystem bool // true for `import <...>` (standard library), false for `import "..."`
}

func (is *ImportStatement) statementNode()       {}
func (is *ImportStatement) TokenLiteral() string { return is.Token.Literal }
func (is *ImportStatement) Line() int            { return is.Token.Line }
func (is *ImportStatement) String() string {
	if is.Alias != "" {
		return "import \"" + is.Path + "\" as " + is.Alias + ";"
	}
	return "import \"" + is.Path + "\";"
}

// ImportCStatement is `importc "header.hpp";` / `importc "<cmath>";` — a direct
// pass-through to the C++ code generator that injects a #include. The target is
// NOT parsed as a Hover file. Path holds the string-literal contents (e.g.
// `native_math.h` or `<cmath>`); codegen decides quote vs angle form.
type ImportCStatement struct {
	Token token.Token
	Path  string
}

func (ic *ImportCStatement) statementNode()       {}
func (ic *ImportCStatement) TokenLiteral() string { return ic.Token.Literal }
func (ic *ImportCStatement) Line() int            { return ic.Token.Line }
func (ic *ImportCStatement) String() string       { return "importc \"" + ic.Path + "\";" }
