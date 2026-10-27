package secrets_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// exportedSignatures maps "Func" or "Type.Method" to its result list, for
// every exported function and every exported method of an exported type in
// the package's non-test files.
func exportedSignatures(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, d := range file.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || !fd.Name.IsExported() {
					continue
				}
				name := fd.Name.Name
				if fd.Recv != nil {
					recv := fd.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					id, ok := recv.(*ast.Ident)
					if !ok || !id.IsExported() {
						continue
					}
					name = id.Name + "." + name
				}
				out[name] = results(fset, fd)
			}
		}
	}
	return out
}

func results(fset *token.FileSet, fd *ast.FuncDecl) string {
	if fd.Type.Results == nil {
		return "()"
	}
	var parts []string
	for _, f := range fd.Type.Results.List {
		var sb strings.Builder
		_ = printer.Fprint(&sb, fset, f.Type)
		n := max(len(f.Names), 1)
		for range n {
			parts = append(parts, sb.String())
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
