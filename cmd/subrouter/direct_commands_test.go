package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Every top-level verb srRunner.run dispatches must be a direct command, or
// it is dead under `sr` and `subrouter` and reaches only `cx` (the rule on
// directSRCommands; `sr export` stayed unreachable its whole life). Upstream
// #600's `sr errors` was dispatched and in the help, and answered `unknown
// command "errors"`. TestDirectSRCommandNames pins a hand-written list, which
// cannot catch the next verb left out; this reads the dispatch itself.
//
// Exceptions: the verbs runForProgram handles before srRunner.run -- `codex`
// (the launcher, and isCodexAccountCommand for its account verbs) and help.
func TestEverySRRunVerbIsADirectCommand(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "sr.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		if ident, ok := fn.Recv.List[0].Type.(*ast.Ident); ok && ident.Name == "srRunner" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("srRunner.run not found in sr.go")
	}
	var verbs []string
	ast.Inspect(run.Body, func(node ast.Node) bool {
		sw, ok := node.(*ast.SwitchStmt)
		if !ok || !isArgsZero(sw.Tag) {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if verb, err := strconv.Unquote(lit.Value); err == nil {
						verbs = append(verbs, verb)
					}
				}
			}
		}
		return true
	})
	seen := map[string]bool{}
	for _, verb := range verbs {
		seen[verb] = true
		switch verb {
		case "codex", "help", "-h", "--help":
			continue
		}
		if !isDirectSRCommand(verb) {
			t.Errorf("srRunner.run dispatches %q, but it is not in directSRCommands: dead under sr and subrouter", verb)
		}
	}
	for _, want := range []string{"status", "errors", "error", "sessions"} {
		if !seen[want] {
			t.Fatalf("calibration: verb %q not found in srRunner.run's dispatch (found %v)", want, verbs)
		}
	}
}

// isArgsZero reports whether expr is args[0].
func isArgsZero(expr ast.Expr) bool {
	index, ok := expr.(*ast.IndexExpr)
	if !ok {
		return false
	}
	ident, ok := index.X.(*ast.Ident)
	lit, okLit := index.Index.(*ast.BasicLit)
	return ok && okLit && ident.Name == "args" && lit.Value == "0"
}
