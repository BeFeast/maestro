package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The orchestrator parks a slot and notifies the operator exactly for
// NativeProjectedReceiptHoldCodes. That list must name every launch-uncertain
// code the termination fence can report for an untrusted projected receipt, so
// this test derives the codes from the fence's own source instead of trusting a
// second copy: every NativeRegistrationHold literal with a launch-uncertain
// value reachable from NativeSessionProcessTerminal through package-level
// calls (readNativeWorkerReceipt, validateNativeWorkerOutcome,
// validateNativeOperatorRecovery, the process evidence and marker checks),
// plus the codes projectedGenerationReceiptHold assigns. A new code on either
// side alone fails here.
func TestNativeProjectedReceiptHoldCodesMatchTerminationFence(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	derived := map[string]bool{}
	visited := map[string]bool{}
	queue := []string{"NativeSessionProcessTerminal", "projectedGenerationReceiptHold"}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		fn := funcs[name]
		if fn == nil {
			t.Fatalf("fence function %s not found", name)
		}
		if visited[name] {
			continue
		}
		visited[name] = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if ident, ok := n.Fun.(*ast.Ident); ok && funcs[ident.Name] != nil && !visited[ident.Name] {
					queue = append(queue, ident.Name)
				}
			case *ast.CompositeLit:
				if ident, ok := n.Type.(*ast.Ident); !ok || ident.Name != "NativeRegistrationHold" {
					return true
				}
				var code ast.Expr
				uncertain := false
				for _, elt := range n.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						t.Fatalf("%s: unkeyed NativeRegistrationHold literal in %s", fset.Position(n.Pos()), name)
					}
					switch kv.Key.(*ast.Ident).Name {
					case "Code":
						code = kv.Value
					case "LaunchUncertain":
						// Anything but a literal false may be launch-uncertain.
						value, ok := kv.Value.(*ast.Ident)
						uncertain = !ok || value.Name != "false"
					}
				}
				if !uncertain {
					return true
				}
				if code == nil {
					t.Fatalf("%s: launch-uncertain hold without a code in %s", fset.Position(n.Pos()), name)
				}
				for _, c := range holdCodeValues(t, fset, fn, code) {
					derived[c] = true
				}
			}
			return true
		})
	}
	var got []string
	for code := range derived {
		got = append(got, code)
	}
	slices.Sort(got)
	want := NativeProjectedReceiptHoldCodes()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("termination fence launch-uncertain codes = %q, NativeProjectedReceiptHoldCodes = %q", got, want)
	}
}

// holdCodeValues resolves the Code of a hold literal to its string values: a
// string literal, or a local variable whose every assignment is a string
// literal. A field of another hold (hold.Code) passes that hold through, and
// its codes are collected where that hold is built.
func holdCodeValues(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, code ast.Expr) []string {
	t.Helper()
	switch code := code.(type) {
	case *ast.BasicLit:
		value, err := strconv.Unquote(code.Value)
		if err != nil {
			t.Fatal(err)
		}
		return []string{value}
	case *ast.SelectorExpr:
		if ident, ok := code.X.(*ast.Ident); ok && code.Sel.Name == "Code" && ident.Name == "hold" {
			return nil
		}
	case *ast.Ident:
		var values []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for i, lhs := range assign.Lhs {
				if ident, ok := lhs.(*ast.Ident); ok && ident.Name == code.Name {
					values = append(values, holdCodeValues(t, fset, fn, assign.Rhs[i])...)
				}
			}
			return true
		})
		if len(values) > 0 {
			return values
		}
	}
	t.Fatalf("%s: cannot derive the hold code in %s; extend holdCodeValues", fset.Position(code.Pos()), fn.Name.Name)
	return nil
}
