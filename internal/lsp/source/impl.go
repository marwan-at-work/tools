package source

import (
	"go/ast"
	"go/token"
	"go/types"
	"log"

	errors "golang.org/x/xerrors"
)

type ImplementRequest struct {
	InterfacePath string
	InterfaceName string
	ConcretePath  string
	ConcreteName  string
	View          string
}

func GetRequest(path []ast.Node, pos token.Pos, info *types.Info, fset *token.FileSet) (*ImplementRequest, error) {
	if rs, ok := isReturnStatement(path); ok {
		return getRequestFromReturn(pos, path, rs, info)
	}
	if vs := isValueSpec(path); vs != nil {
		return requestFromDeclaration(vs, pos, info)
	}
	// for _, p := range path {

	// }
	return nil, nil
}

func requestFromDeclaration(vs *ast.ValueSpec, pos token.Pos, info *types.Info) (*ImplementRequest, error) {
	var idx int
	for i, vs := range vs.Values {
		if pos >= vs.Pos() && pos <= vs.End() {
			idx = i
			break
		}
	}
	n := vs.Values[idx]
	var ir *ImplementRequest
	ast.Inspect(n, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj, ok := info.Uses[ident]
		if !ok {
			return true
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			return true
		}
		ir = &ImplementRequest{
			ConcreteName: tn.Name(),
			ConcretePath: tn.Pkg().Path(),
		}
		return false
	})
	if ir == nil {
		return nil, nil
	}
	ast.Inspect(vs.Type, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj, ok := info.Uses[ident]
		if !ok {
			log.Printf("fuck: %T\n", obj)
			return true
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			log.Printf("noooo: %T\n", obj)
			return true
		}
		ir.InterfaceName = tn.Name()
		ir.InterfacePath = tn.Pkg().Path()
		return false
	})
	log.Printf("%v@%v implements %v@%v\n\n", ir.ConcreteName, ir.ConcretePath, ir.InterfaceName, ir.InterfacePath)
	return ir, nil
}

func isValueSpec(path []ast.Node) *ast.ValueSpec {
	for _, n := range path {
		if decl, ok := n.(*ast.ValueSpec); ok {
			return decl
		}
	}
	return nil
}

func getRequestFromReturn(pos token.Pos, path []ast.Node, rs *ast.ReturnStmt, info *types.Info) (*ImplementRequest, error) {
	idx, err := getReturnIndex(rs, pos)
	if err != nil {
		return nil, err
	}
	n := rs.Results[idx]
	// TODO: make(), and a lot of other shizz
	if _, ok := n.(*ast.MapType); ok {
		return nil, nil
	}
	if _, ok := n.(*ast.ArrayType); ok {
		return nil, nil
	}
	var ir *ImplementRequest
	ast.Inspect(n, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj, ok := info.Uses[ident]
		if !ok {
			return true
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			return true
		}
		ir = &ImplementRequest{
			ConcreteName: tn.Name(),
			ConcretePath: tn.Pkg().Path(),
		}
		return false
	})
	if ir == nil {
		return nil, nil
	}
	fi := enclosingFunction(path, info)
	if fi == nil {
		return nil, errors.Errorf("could not find function in a return statement")
	}
	result, ok := fi.sig.Results().At(idx).Type().(*types.Named)
	if !ok {
		return nil, nil
	}
	ir.InterfaceName = result.Obj().Name()
	ir.InterfacePath = result.Obj().Pkg().Path()
	return ir, nil
}

func isReturnStatement(path []ast.Node) (*ast.ReturnStmt, bool) {
	for _, n := range path {
		rs, ok := n.(*ast.ReturnStmt)
		if ok {
			return rs, true
		}
	}
	return nil, false
}

func getReturnIndex(rs *ast.ReturnStmt, pos token.Pos) (int, error) {
	for idx, r := range rs.Results {
		if pos >= r.Pos() && pos <= r.End() {
			return idx, nil
		}
	}
	return -1, errors.Errorf("pos %d not within return statement bounds: [%d-%d]", pos, rs.Pos(), rs.End())
}
