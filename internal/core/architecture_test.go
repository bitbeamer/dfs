package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestPublicCoreContractHasNoImplementationDependencies(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate core package")
	}
	apiPath := filepath.Join(filepath.Dir(filename), "api.go")
	parsed, err := parser.ParseFile(token.NewFileSet(), apiPath, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"context": true, "io": true, "time": true}
	for _, imported := range parsed.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !allowed[path] {
			t.Errorf("core contract imports implementation package %q", path)
		}
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok {
			name, _ := selector.X.(*ast.Ident)
			if name != nil && strings.Contains(strings.ToLower(name.Name), "fuse") {
				t.Errorf("core contract exposes frontend selector %s.%s", name.Name, selector.Sel.Name)
			}
		}
		return true
	})
}
