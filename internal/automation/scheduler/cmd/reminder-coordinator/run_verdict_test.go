package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestEveryExitPathAfterTheCoordinatorSettlesTheVerdict pins the claim run()'s
// own doc comment makes: that returning the status rather than calling os.Exit
// is what stops a new early return from forgetting the run's verdict.
//
// The rule is stated over the syntax rather than over a run because run() reads
// os.Args, loads a timezone and calls log.Fatalf on three paths, so no test can
// enter it. That limit is the reason the check is worth having: the one function
// no test executes is the one holding the verdict. An early return added below
// the coordinator that returns a bare 0 re-creates card b4130d06 exactly — a
// tick that recorded shortfalls and told the scheduler `success` — and it would
// do so with every other test in this package green.
//
// Ported from cmd/kanban-classifier, where the same guard exists over a local
// `degraded`. Here the recorder is a field on the coordinator, so the anchor is
// the coordinator's own declaration and the accepted verdict is the field's.
func TestEveryExitPathAfterTheCoordinatorSettlesTheVerdict(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	run := findTopLevelFunction(file, "run")
	if run == nil {
		t.Fatal("main.go declares no run() — if the verdict moved, move this check with it")
	}

	coordinatorPosition := token.NoPos
	ast.Inspect(run, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		if name, ok := assign.Lhs[0].(*ast.Ident); !ok || name.Name != "coordinator" {
			return true
		}
		coordinatorPosition = assign.Pos()
		return false
	})
	if coordinatorPosition == token.NoPos {
		t.Fatal("run() no longer declares `coordinator` — the recorder it carries is what this check is about")
	}

	returns := 0
	ast.Inspect(run, func(n ast.Node) bool {
		// A function literal inside run() has its own returns, which are not the
		// command's exit paths.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok || ret.Pos() < coordinatorPosition {
			return true
		}
		returns++
		if len(ret.Results) != 1 || !isRecorderVerdict(ret.Results[0]) {
			t.Errorf("run(): the exit at %s returns %s, not coordinator.Degraded.ExitCode() — "+
				"a run that recorded shortfalls would report `success` through it",
				fset.Position(ret.Pos()), renderNode(ret))
		}
		return true
	})

	if returns == 0 {
		t.Error("found no exit path after the coordinator was built; the check asserted nothing")
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

	main := findTopLevelFunction(file, "main")
	if main == nil {
		t.Fatal("main.go declares no main()")
	}
	if got := len(main.Body.List); got != 1 {
		t.Fatalf("main() has %d statement(s); it must do nothing but exit with run()'s status", got)
	}

	call, ok := main.Body.List[0].(*ast.ExprStmt)
	if !ok {
		t.Fatalf("main()'s only statement is %s, want os.Exit(run())", renderNode(main.Body.List[0]))
	}
	if got := renderNode(call); got != "os.Exit(run())" {
		t.Errorf("main() is %s, want os.Exit(run()) — any other shape discards the run's verdict", got)
	}
}

func findTopLevelFunction(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// isRecorderVerdict reports whether an expression is
// `coordinator.Degraded.ExitCode()`. The receiver is checked all the way down:
// `somethingElse.ExitCode()` is a different verdict and must not pass.
func isRecorderVerdict(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	exitCode, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || exitCode.Sel.Name != "ExitCode" {
		return false
	}
	recorder, ok := exitCode.X.(*ast.SelectorExpr)
	if !ok || recorder.Sel.Name != "Degraded" {
		return false
	}
	owner, ok := recorder.X.(*ast.Ident)
	return ok && owner.Name == "coordinator"
}

// renderNode prints a node the way it reads in the source, so a failure names
// the offending line rather than an ast type.
func renderNode(node ast.Node) string {
	var b strings.Builder
	writeNode(&b, node)
	return b.String()
}

func writeNode(b *strings.Builder, node ast.Node) {
	switch n := node.(type) {
	case *ast.ExprStmt:
		writeNode(b, n.X)
	case *ast.ReturnStmt:
		b.WriteString("return")
		for i, r := range n.Results {
			if i == 0 {
				b.WriteString(" ")
			} else {
				b.WriteString(", ")
			}
			writeNode(b, r)
		}
	case *ast.CallExpr:
		writeNode(b, n.Fun)
		b.WriteString("(")
		for i, arg := range n.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeNode(b, arg)
		}
		b.WriteString(")")
	case *ast.SelectorExpr:
		writeNode(b, n.X)
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
