package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestEveryExitPathAfterTheRecorderSettlesTheVerdict pins the claim run()'s own
// doc comment makes: that returning the status rather than calling os.Exit is
// what stops a new early return from forgetting the run's verdict.
//
// Nothing was asserting it. run() has two exits after the recorder exists today
// and will grow more; an exit added below the recorder that returns a bare 0
// re-creates card b4130d06 exactly — a tick that recorded shortfalls and told
// the scheduler `success` — and it would do so with every other test in this
// package green, because none of them can enter run().
//
// The rule is stated over the syntax rather than over a run because run() reads
// os.Args, resolves a live API key and calls log.Fatalf on six paths, so a test
// cannot enter it at all. That is a real limit and it is the reason this check
// is worth having: the one function no test executes is the one holding the
// verdict.
func TestEveryExitPathAfterTheRecorderSettlesTheVerdict(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	run := findFunction(file, "run")
	if run == nil {
		t.Fatal("main.go declares no run() — if the verdict moved, move this check with it")
	}

	recorderPosition := token.NoPos
	ast.Inspect(run, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		if name, ok := assign.Lhs[0].(*ast.Ident); !ok || name.Name != "degraded" {
			return true
		}
		recorderPosition = assign.Pos()
		return false
	})
	if recorderPosition == token.NoPos {
		t.Fatal("run() no longer declares `degraded` — the recorder is what this check is about")
	}

	returns := 0
	ast.Inspect(run, func(n ast.Node) bool {
		// A function literal inside run() has its own returns, which are not
		// the command's exit paths.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || ret.Pos() < recorderPosition {
			return true
		}
		returns++
		if len(ret.Results) != 1 || !isRecorderVerdict(ret.Results[0]) {
			t.Errorf("run(): the exit at %s returns %s, not degraded.ExitCode() — "+
				"a run that recorded shortfalls would report `success` through it",
				fset.Position(ret.Pos()), render(ret))
		}
		return true
	})

	if returns == 0 {
		t.Error("found no exit path after the recorder was declared; the check asserted nothing")
	}
}

// TestMainExitsWithTheStatusRunReturned — run() computing the right status is
// worth nothing if main() drops it on the floor.
func TestMainExitsWithTheStatusRunReturned(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	main := findFunction(file, "main")
	if main == nil {
		t.Fatal("main.go declares no main()")
	}
	if got := len(main.Body.List); got != 1 {
		t.Fatalf("main() has %d statement(s); it must do nothing but exit with run()'s status", got)
	}

	call, ok := main.Body.List[0].(*ast.ExprStmt)
	if !ok {
		t.Fatalf("main()'s only statement is %s, want os.Exit(run())", render(main.Body.List[0]))
	}
	if got := render(call); got != "os.Exit(run())" {
		t.Errorf("main() is %s, want os.Exit(run()) — any other shape discards the run's verdict", got)
	}
}

func findFunction(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// isRecorderVerdict reports whether an expression is `degraded.ExitCode()`.
func isRecorderVerdict(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "ExitCode" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == "degraded"
}

// render prints a node the way it reads in the source, so a failure names the
// offending line rather than an ast type.
func render(node ast.Node) string {
	var b strings.Builder
	writeExpr(&b, node)
	return b.String()
}

func writeExpr(b *strings.Builder, node ast.Node) {
	switch n := node.(type) {
	case *ast.ExprStmt:
		writeExpr(b, n.X)
	case *ast.ReturnStmt:
		b.WriteString("return")
		for i, r := range n.Results {
			if i == 0 {
				b.WriteString(" ")
			} else {
				b.WriteString(", ")
			}
			writeExpr(b, r)
		}
	case *ast.CallExpr:
		writeExpr(b, n.Fun)
		b.WriteString("(")
		for i, arg := range n.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeExpr(b, arg)
		}
		b.WriteString(")")
	case *ast.SelectorExpr:
		writeExpr(b, n.X)
		b.WriteString(".")
		b.WriteString(n.Sel.Name)
	case *ast.Ident:
		b.WriteString(n.Name)
	case *ast.BasicLit:
		b.WriteString(n.Value)
	default:
		b.WriteString("<expression>")
	}
}
