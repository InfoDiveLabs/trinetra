package trinetra

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

var forbiddenInSampler = map[string]bool{
	"pluginhost": true, "pluginrt": true, "pluginDeps": true, "Caller": true,
	"RenderSlot": true, "InvokeAction": true, "plugin": true,
}

func isEvaluateOfFastChecks(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Evaluate" {
		return false
	}
	inner, ok := call.Args[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := inner.Fun.(*ast.Ident)
	return ok && id.Name == "buildFastChecks"
}

func TestSamplerLoopNeverCallsPlugins(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var loop ast.Node
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if loop == nil && isEvaluateOfFastChecks(n) {
			for i := len(stack) - 1; i >= 0; i-- {
				switch stack[i].(type) {
				case *ast.ForStmt, *ast.RangeStmt:
					loop = stack[i]
				}
				if loop != nil {
					break
				}
			}
		}
		return true
	})
	if loop == nil {
		t.Fatal("sampler loop (for containing alerts.Evaluate(buildFastChecks(...))) not found in daemon.go")
	}

	if span := fset.Position(loop.End()).Line - fset.Position(loop.Pos()).Line; span < 30 {
		t.Fatalf("located loop at daemon.go:%d is only %d lines; the sampler loop moved", fset.Position(loop.Pos()).Line, span)
	}

	ast.Inspect(loop, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if forbiddenInSampler[x.Name] {
				t.Errorf("daemon.go:%d: sampler loop references %q", fset.Position(x.Pos()).Line, x.Name)
			}
		case *ast.SelectorExpr:
			if forbiddenInSampler[x.Sel.Name] {
				t.Errorf("daemon.go:%d: sampler loop references .%s", fset.Position(x.Pos()).Line, x.Sel.Name)
			}
		}
		return true
	})
}

func TestSubmitSeriesClosureOnlyTrySubmits(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "daemon.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncLit
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if id, ok := cl.Type.(*ast.Ident); !ok || id.Name != "pluginDeps" {
			return true
		}
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "submitSeries" {
				fn, _ = kv.Value.(*ast.FuncLit)
			}
		}
		return true
	})
	if fn == nil {
		t.Fatal("pluginDeps{submitSeries: func...} literal not found in daemon.go")
	}
	var calls []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.SelectorExpr:
			calls = append(calls, f.Sel.Name)
		case *ast.Ident:
			calls = append(calls, f.Name)
		}
		return true
	})
	if len(calls) != 1 || calls[0] != "trySubmit" {
		t.Fatalf("submitSeries must call only sw.trySubmit, calls: %s", strings.Join(calls, ", "))
	}
}
