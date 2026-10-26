package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// operatorAccessors bypass the read allowlist and exist for the operator's
// /access page alone. A tool that referenced one could hand the model chats the
// operator never allowed.
var operatorAccessors = map[string]bool{
	"OperatorJoinedGroups": true,
	"ReplaceReadAllowlist": true,
	"AddReadAllowlist":     true,
	"RemoveReadAllowlist":  true,
	"ListAccessChats":      true,
}

func TestMCPPackageDoesNotReachOperatorAccessors(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && operatorAccessors[id.Name] {
				t.Errorf("%s: %s is operator-only and must not be used by MCP tools", fset.Position(id.Pos()), id.Name)
			}
			return true
		})
	}
}
