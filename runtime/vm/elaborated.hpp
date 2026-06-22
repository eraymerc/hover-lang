#pragma once

#include <string>
#include <vector>
#include <unordered_map>

// ─────────────────────────────────────────────────────────────────────────────
// ELABORATED PROGRAM TYPES
//
// These mirror the Go elaborator output structs.
// The full elaborator will populate these; the VM only reads them.
//
// Mirrors Go package elaborator:
//   type LogicBlock struct { Domain; Prefix; Params; Ports; Source }
//   type Physical   struct { Type; Name; CtrlSignal }
//   type ElaboratedProgram struct { Logic; Physicals; Functions; Directives }
// ─────────────────────────────────────────────────────────────────────────────

// Token domain types — mirrors Go token.Type for domain tagging
enum Domain {
    DOMAIN_MODULE,
    DOMAIN_DIGITAL,
    DOMAIN_ANALOG,
};

// ── AST node types ────────────────────────────────────────────────────────────
// Minimal set needed by the VM to evaluate expressions and execute statements.
// The full AST will live in the Go side; the C++ VM receives a pre-flattened
// representation from the elaborator.

enum NodeKind {
    // Expressions
    NODE_NUMBER,        // literal number
    NODE_IDENT,         // identifier
    NODE_UNARY,         // -x, !x
    NODE_BINARY,        // x + y, x > y, etc.
    NODE_CALL,          // f(args...)
    NODE_INDEX,         // a[i]

    // Statements
    NODE_ASSIGN,        // left = right
    NODE_LOCAL_DECL,    // [state] type name [= value], ...
    NODE_IF,            // if (cond) { ... } else if ... else ...
    NODE_WHILE,         // while (cond) { ... }
    NODE_RETURN,        // return expr
    NODE_BLOCK,         // { stmts... }
};

struct ASTNode {
    NodeKind kind;

    // Shared fields
    std::string str_val;    // number literal, identifier name, operator, type name
    int         is_state;   // LocalDecl: 1 if 'state' modifier

    // Children (owned by the tree, allocated once at elaboration time)
    std::vector<ASTNode*> children;  // layout depends on kind — see comments below

    // Kind-specific child layouts:
    //   NODE_NUMBER:     children empty, str_val = literal
    //   NODE_IDENT:      children empty, str_val = name
    //   NODE_UNARY:      children[0] = operand,      str_val = operator ("-","!")
    //   NODE_BINARY:     children[0] = left,          children[1] = right, str_val = operator
    //   NODE_CALL:       children[0] = function ident, children[1..] = args
    //   NODE_INDEX:      children[0] = array expr,    children[1] = index expr
    //   NODE_ASSIGN:     children[0] = lhs,           children[1] = rhs
    //   NODE_LOCAL_DECL: children in pairs (name_node, value_node_or_null), str_val = type
    //   NODE_IF:         children[0]=cond, children[1]=consequence,
    //                    children[2..2+2*N-1]=elseif pairs (cond,body),
    //                    children.back()=else_body (or nullptr if absent)
    //   NODE_WHILE:      children[0]=cond, children[1]=body
    //   NODE_RETURN:     children[0]=value
    //   NODE_BLOCK:      children = statement list
};

// ── Logic block ───────────────────────────────────────────────────────────────
// One flattened module body after elaboration.
// Mirrors Go: type LogicBlock struct { Domain; Prefix; Params; Ports; Source }

struct LogicBlock {
    Domain      domain;
    std::string prefix;                                  // mangled name, e.g. "main.ctrl_pid"
    std::unordered_map<std::string, double> params;      // static args substituted
    std::unordered_map<std::string, std::string> ports;  // local name → mangled signal name
    ASTNode    *source;                                  // block body (NODE_BLOCK)
};

// ── Physical primitive ────────────────────────────────────────────────────────
// Mirrors Go: type Physical struct { Type; Name; CtrlSignal }

struct Physical {
    std::string type;         // "voltage_source", "current_source", etc.
    std::string name;         // mangled MNA element name
    std::string ctrl_signal;  // mangled VM signal name (empty = fixed source)
};

// ── Function declaration ──────────────────────────────────────────────────────
// Mirrors Go: *ast.FuncDeclStatement stored in ElaboratedProgram.Functions

struct FuncParam {
    std::string type;
    std::string name;
};

struct FuncDecl {
    std::string            name;
    std::string            return_type;
    std::vector<FuncParam> params;
    ASTNode               *body;   // NODE_BLOCK
};

// ── Directive ─────────────────────────────────────────────────────────────────
// Mirrors Go: elaborator.Directive { Name; Args []ast.Expression }

struct DirectiveArg {
    std::string value;  // pre-stringified for .tran/.solver, dotted path for .save
};

struct Directive {
    std::string              name;
    std::vector<DirectiveArg> args;
};

// ── Elaborated program ────────────────────────────────────────────────────────
// The complete flat program handed to the VM.
// Mirrors Go: type ElaboratedProgram struct { Logic; Physicals; Functions; Directives }

struct ElaboratedProgram {
    std::vector<LogicBlock>  logic;
    std::vector<Physical>    physicals;
    std::unordered_map<std::string, FuncDecl*> functions;
    std::vector<Directive>   directives;
};