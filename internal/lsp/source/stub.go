// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/span"
)

type methodData struct {
	Method    string
	Interface string
	Concrete  string
	Signature string
}

const tmpl = `// {{ .Method }} implements {{ .Interface }}
func ({{ .Concrete }}) {{ .Method }}{{ .Signature }} {
	panic("unimplemented")
}
`

// MethodStubActions returns code actions that can generate interface
// stubs to fix "missing method" actions. The CodeAction fix will contain the entire
// source file as it might add new imports along with the interface stubs
func MethodStubActions(ctx context.Context, diagnostics []protocol.Diagnostic, snapshot Snapshot, fh VersionedFileHandle) ([]protocol.CodeAction, error) {
	actions := []protocol.CodeAction{}
	for _, d := range diagnostics {
		if !isMissingMethodErr(d) && !isConversionErr(d) {
			continue
		}
		pkg, pgf, err := GetParsedFile(ctx, snapshot, fh, NarrowestPackage)
		if err != nil {
			return nil, fmt.Errorf("GetParsedFile: %w", err)
		}
		nodes, pos, err := getNodes(pgf, d)
		if err != nil {
			return nil, fmt.Errorf("getNodes: %w", err)
		}
		ir := getImplementRequest(nodes, pkg, pos)
		if ir == nil {
			continue
		}
		concreteFile, concreteFH, err := getFile(ctx, ir.concreteObj, snapshot)
		if err != nil {
			return nil, fmt.Errorf("getFile(concrete): %w", err)
		}
		ct := &concreteType{
			pkg:  ir.concreteObj.Pkg(),
			fset: snapshot.FileSet(),
			file: concreteFile,
			tms:  types.NewMethodSet(ir.concreteObj.Type()),
			pms:  types.NewMethodSet(types.NewPointer(ir.concreteObj.Type())),
		}
		missing, err := missingMethods(ctx, snapshot, ct, ir.ifaceObj, ir.ifacePkg, map[string]struct{}{})
		if err != nil {
			return nil, fmt.Errorf("missingMethods: %w", err)
		}
		if len(missing) == 0 {
			return nil, nil
		}
		t := template.Must(template.New("").Parse(tmpl))
		var methodsBuffer bytes.Buffer
		for _, mi := range missing {
			for _, m := range mi.missing {
				var sig bytes.Buffer
				nn, _ := astutil.PathEnclosingInterval(mi.file, m.Pos(), m.Pos())
				var n ast.Node = nn[1].(*ast.Field).Type
				n = copyAST(n)
				n = astutil.Apply(n, func(c *astutil.Cursor) bool {
					sel, ok := c.Node().(*ast.SelectorExpr)
					if ok {
						renamed := mightRenameSelector(ctx, c, sel, mi.pkg, ct)
						removed := mightRemoveSelector(ctx, c, sel, mi.pkg, ct.pkg.Path())
						return removed || renamed
					}
					ident, ok := c.Node().(*ast.Ident)
					if ok {
						return mightAddSelector(c, ident, ir.ifacePkg, ct)
					}
					return true
				}, nil)
				err = format.Node(&sig, snapshot.FileSet(), n)
				if err != nil {
					return nil, fmt.Errorf("could not format function signature: %w", err)
				}
				concrete := ir.concreteObj.Name()
				if ir.pointer {
					concrete = "*" + concrete
				}
				md := methodData{
					Method:    m.Name(),
					Concrete:  concrete,
					Interface: getIfaceName(pkg, ir.ifacePkg, ir.ifaceObj),
					Signature: strings.TrimPrefix(sig.String(), "func"),
				}
				err = t.Execute(&methodsBuffer, md)
				if err != nil {
					return nil, fmt.Errorf("error executing method template: %w", err)
				}
				methodsBuffer.WriteRune('\n')
			}
		}
		nodes, _ = astutil.PathEnclosingInterval(concreteFile, ir.concreteObj.Pos(), ir.concreteObj.Pos())
		var concBuf bytes.Buffer
		err = format.Node(&concBuf, snapshot.FileSet(), concreteFile)
		if err != nil {
			return nil, fmt.Errorf("error formatting concrete file: %w", err)
		}
		concreteSrc := concBuf.Bytes()
		insertPos := snapshot.FileSet().Position(nodes[1].End()).Offset
		var buf bytes.Buffer
		buf.Write(concreteSrc[:insertPos])
		buf.WriteByte('\n')
		buf.Write(methodsBuffer.Bytes())
		buf.Write(concreteSrc[insertPos:])
		fset := token.NewFileSet()
		newF, err := parser.ParseFile(fset, concreteFile.Name.Name, buf.Bytes(), parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("could not reparse file: %w", err)
		}
		for _, imp := range ct.addedImports {
			astutil.AddNamedImport(fset, newF, imp.Name, imp.Path)
		}
		var source bytes.Buffer
		err = format.Node(&source, fset, newF)
		if err != nil {
			return nil, fmt.Errorf("format.Node: %w", err)
		}
		_, pgf, err = GetParsedFile(ctx, snapshot, concreteFH, NarrowestPackage)
		if err != nil {
			return nil, fmt.Errorf("GetParsedFile(concrete): %w", err)
		}
		edits, err := computeTextEdits(ctx, snapshot, pgf, source.String())
		if err != nil {
			return nil, fmt.Errorf("computeTextEdit: %w", err)
		}
		actions = append(actions, protocol.CodeAction{
			Title:       fmt.Sprintf("Implement %s", getIfaceName(pkg, ir.ifacePkg, ir.ifaceObj)),
			Diagnostics: []protocol.Diagnostic{d},
			Kind:        protocol.QuickFix,
			Edit: protocol.WorkspaceEdit{
				DocumentChanges: []protocol.TextDocumentEdit{
					{
						TextDocument: protocol.VersionedTextDocumentIdentifier{
							Version: concreteFH.Version(),
							TextDocumentIdentifier: protocol.TextDocumentIdentifier{
								URI: protocol.URIFromSpanURI(concreteFH.URI()),
							},
						},
						Edits: edits,
					},
				},
			},
		})
	}
	return actions, nil
}

func getIfaceName(pkg, ifacePkg Package, ifaceObj types.Object) string {
	if pkg.PkgPath() == ifacePkg.PkgPath() {
		return ifaceObj.Name()
	}
	return fmt.Sprintf("%s.%s", ifacePkg.Name(), ifaceObj.Name())
}

func getNodes(pgf *ParsedGoFile, d protocol.Diagnostic) ([]ast.Node, token.Pos, error) {
	spn, err := pgf.Mapper.RangeSpan(d.Range)
	if err != nil {
		return nil, 0, err
	}
	rng, err := spn.Range(pgf.Mapper.Converter)
	if err != nil {
		return nil, 0, err
	}
	nodes, _ := astutil.PathEnclosingInterval(pgf.File, rng.Start, rng.End)
	return nodes, rng.Start, nil
}

// mightRemoveSelector will replace a selector such as *models.User to just be *User.
// This is needed if the interface method imports the same package where the concrete type
// is going to implement that method
func mightRemoveSelector(ctx context.Context, c *astutil.Cursor, sel *ast.SelectorExpr, ifacePkg Package, implPath string) bool {
	obj := ifacePkg.GetTypesInfo().Uses[sel.Sel]
	if obj == nil || obj.Pkg() == nil {
		// shouldn't really happen because the selector
		// is always coming from the given given package
		return false
	}
	if obj.Pkg().Path() == implPath {
		c.Replace(sel.Sel)
		return false
	}
	return false
}

// mightRenameSelector will take a selector such as *models.User and rename it to *somethingelse.User
// if the target conrete type file already imports the "models" package but has renamed it.
// If the concrete type does not have the import file, then the import file will be added along with its
// rename if the interface file has defined one.
func mightRenameSelector(ctx context.Context, c *astutil.Cursor, sel *ast.SelectorExpr, ifacePkg Package, ct *concreteType) bool {
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	obj := ifacePkg.GetTypesInfo().Uses[ident]
	if obj == nil {
		return false
	}
	pn, ok := obj.(*types.PkgName)
	if !ok {
		return false
	}
	pkg := pn.Imported()
	var hasImport bool
	var importName string
	for _, imp := range ct.file.Imports {
		impPath, _ := strconv.Unquote(imp.Path.Value)
		if impPath == pkg.Path() && !isIgnoredImport(imp) {
			hasImport = true
			importName = pkg.Name()
			if imp.Name != nil && imp.Name.Name != pkg.Name() {
				importName = imp.Name.Name
			}
			break
		}
	}
	if hasImport {
		sel.X = &ast.Ident{
			Name:    importName,
			NamePos: ident.NamePos,
			Obj:     ident.Obj,
		}
		c.Replace(sel)
		return false
	}
	// there is no import, let's add a new one
	if pkg.Path() == ct.pkg.Path() {
		// but not if we're importing the path to the concrete type
		// in which case we're dropping this in mightRemoveSelector
		return false
	}
	// if we're adding a new import to the concrete type file, and
	// it has been renamed in the interface file, honor the rename.
	if pn.Name() != pkg.Name() {
		importName = pn.Name()
	}
	ct.addImport(importName, pkg.Path())
	return false
}

// mightAddSelector takes an identifier such as "User" and might turn into a selector
// such as "models.User". This is needed when an interface method references
// a type declaration in its own package while the concrete type is in a different package.
// If an import already exists, it will use that import's name. If it does not exist,
// it will add it to the ct's *ast.File.
func mightAddSelector(
	c *astutil.Cursor,
	ident *ast.Ident,
	ifacePkg Package,
	ct *concreteType,
) bool {
	obj := ifacePkg.GetTypesInfo().Uses[ident]
	if obj == nil {
		return false
	}
	n, ok := obj.Type().(*types.Named)
	if !ok {
		return false
	}
	pkg := n.Obj().Pkg()
	if pkg == nil {
		return false
	}
	pkgName := pkg.Name()
	missingImport := true
	for _, imp := range ct.file.Imports {
		impPath, _ := strconv.Unquote(imp.Path.Value)
		if pkg.Path() == impPath && !isIgnoredImport(imp) {
			missingImport = false
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
			break
		}
	}
	isNotImportingDestination := pkg.Path() != ct.pkg.Path()
	if missingImport && isNotImportingDestination {
		ct.addImport("", pkg.Path())
	}
	isLocalDeclaration := pkg.Path() == ifacePkg.PkgPath() && pkg.Path() != ct.pkg.Path()
	isDotImport := pkg.Path() != ifacePkg.PkgPath() && pkg.Path() != ct.pkg.Path()
	// the only reason we know it's a dotImport is because we never visit an Identifier
	// that was part of a SelectorExpr.
	if isLocalDeclaration || isDotImport {
		c.Replace(&ast.SelectorExpr{
			X:   &ast.Ident{Name: pkgName},
			Sel: ident,
		})
		return false
	}
	return false
}

/*
missingMethods takes a concrete type and returns any missing methods for the given interface as well as
any missing interface that might have been embedded to its parent. For example:

type I interface {
	io.Writer
	Hello()
}
returns []*missingInterface{
	{
		iface: *types.Interface (io.Writer),
		file: *ast.File: io.go,
		missing []*types.Func{Write},
	},
	{
		iface: *types.Interface (I),
		file: *ast.File: myfile.go,
		missing: []*types.Func{Hello}
	},
}
*/
func missingMethods(ctx context.Context, snapshot Snapshot, ct *concreteType, ifaceObj types.Object, ifacePkg Package, visited map[string]struct{}) ([]*missingInterface, error) {
	iface, ok := ifaceObj.Type().Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("expected %v to be an interface but got %T", iface, ifaceObj.Type().Underlying())
	}
	missing := []*missingInterface{}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		eiface := iface.Embedded(i).Obj()
		depPkg := ifacePkg
		if eiface.Pkg().Path() != ifacePkg.PkgPath() {
			var err error
			depPkg, err = ifacePkg.GetImport(eiface.Pkg().Path())
			if err != nil {
				return nil, err
			}
		}
		em, err := missingMethods(ctx, snapshot, ct, eiface, depPkg, visited)
		if err != nil {
			return nil, err
		}
		missing = append(missing, em...)
	}
	astFile, _, err := getFile(ctx, ifaceObj, snapshot)
	if err != nil {
		return nil, fmt.Errorf("error getting iface file: %w", err)
	}
	mi := &missingInterface{
		pkg:   ifacePkg,
		iface: iface,
		file:  astFile,
	}
	if mi.file == nil {
		return nil, fmt.Errorf("could not find ast.File for %v", ifaceObj.Name())
	}
	for i := 0; i < iface.NumExplicitMethods(); i++ {
		method := iface.ExplicitMethod(i)
		if ct.doesNotHaveMethod(method.Name()) {
			if _, ok := visited[method.Name()]; !ok {
				mi.missing = append(mi.missing, method)
				visited[method.Name()] = struct{}{}
			}
		}
		if sel := ct.getMethodSelection(method.Name()); sel != nil {
			implSig := sel.Type().(*types.Signature)
			ifaceSig := method.Type().(*types.Signature)
			if !types.Identical(ifaceSig, implSig) {
				return nil, &mismatchError{
					name: method.Name(),
					have: implSig,
					want: ifaceSig,
				}
			}
		}
	}
	if len(mi.missing) > 0 {
		missing = append(missing, mi)
	}
	return missing, nil
}

func getFile(ctx context.Context, obj types.Object, snapshot Snapshot) (*ast.File, VersionedFileHandle, error) {
	objPos := snapshot.FileSet().Position(obj.Pos())
	objFile := span.URIFromPath(objPos.Filename)
	objectFH := snapshot.FindFile(objFile)
	_, goFile, err := GetParsedFile(ctx, snapshot, objectFH, WidestPackage)
	if err != nil {
		return nil, nil, fmt.Errorf("GetParsedFile: %w", err)
	}
	return goFile.File, objectFH, nil
}

// missingInterface represents an interface
// that has all or some of its methods missing
// from the destination concrete type
type missingInterface struct {
	iface   *types.Interface
	file    *ast.File
	pkg     Package
	missing []*types.Func
}

func isMissingMethodErr(d protocol.Diagnostic) bool {
	return d.Source == "compiler" && strings.Contains(d.Message, "missing method")
}

func isConversionErr(d protocol.Diagnostic) bool {
	return d.Source == "compiler" && strings.HasPrefix(d.Message, "cannot convert")
}

type stubRequest struct {
	ifacePkg Package
	ifaceObj types.Object

	concretePkg Package
	concreteObj types.Object

	pointer bool
}

// getImplementRequest determines whether the "missing method error"
// can be used to deduced what the concrete and interface types are
func getImplementRequest(nodes []ast.Node, pkg Package, pos token.Pos) *stubRequest {
	if vs := isVariableDeclaration(nodes); vs != nil {
		return fromValueSpec(vs, pkg, pos)
	}
	if ret, ok := isReturnStatement(nodes); ok {
		ir, _ := getRequestFromReturn(pos, nodes, ret, pkg)
		return ir
	}
	return nil
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

func getRequestFromReturn(pos token.Pos, path []ast.Node, rs *ast.ReturnStmt, pkg Package) (*stubRequest, error) {
	idx, err := getReturnIndex(rs, pos)
	if err != nil {
		return nil, err
	}
	n := rs.Results[idx]
	// TODO: make(), and other builtins
	if _, ok := n.(*ast.MapType); ok {
		return nil, nil
	}
	if _, ok := n.(*ast.ArrayType); ok {
		return nil, nil
	}
	var ir *stubRequest
	ast.Inspect(n, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj, ok := pkg.GetTypesInfo().Uses[ident]
		if !ok {
			return true
		}
		concretePkg := pkg
		if obj.Pkg().Path() != pkg.PkgPath() {
			var err error
			concretePkg, err = pkg.GetImport(obj.Pkg().Path())
			if err != nil {
				return true
			}
		}
		_, ok = obj.(*types.TypeName)
		if !ok {
			return true
		}
		ir = &stubRequest{
			concreteObj: obj,
			concretePkg: concretePkg,
		}
		return false
	})
	if ir == nil {
		return nil, nil
	}
	fi := EnclosingFunction(path, pkg.GetTypesInfo())
	if fi == nil {
		return nil, fmt.Errorf("could not find function in a return statement")
	}
	result, ok := fi.Sig.Results().At(idx).Type().(*types.Named)
	if !ok {
		return nil, nil
	}
	ir.ifaceObj = result.Obj()
	ifacePkg := pkg
	if ifacePkg.PkgPath() != ir.ifaceObj.Pkg().Path() {
		ifacePkg, _ = pkg.GetImport(ir.ifaceObj.Pkg().Path())
	}
	ir.ifacePkg = ifacePkg
	return ir, nil
}

func getReturnIndex(rs *ast.ReturnStmt, pos token.Pos) (int, error) {
	for idx, r := range rs.Results {
		if pos >= r.Pos() && pos <= r.End() {
			return idx, nil
		}
	}
	return -1, fmt.Errorf("pos %d not within return statement bounds: [%d-%d]", pos, rs.Pos(), rs.End())
}

func fromValueSpec(vs *ast.ValueSpec, pkg Package, pos token.Pos) *stubRequest {
	var idx int
	for i, vs := range vs.Values {
		if pos >= vs.Pos() && pos <= vs.End() {
			idx = i
			break
		}
	}
	valueNode := vs.Values[idx]
	inspectNode := func(n ast.Node) (nodeObj types.Object, nodePkg Package) {
		ast.Inspect(n, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			obj, ok := pkg.GetTypesInfo().Uses[ident]
			if !ok {
				return true
			}
			_, ok = obj.(*types.TypeName)
			if !ok {
				return true
			}
			nodePkg = pkg
			if obj.Pkg().Path() != pkg.PkgPath() {
				var err error
				nodePkg, err = pkg.GetImport(obj.Pkg().Path())
				if err != nil {
					return true
				}
			}
			nodeObj = obj
			return false
		})
		return nodeObj, nodePkg
	}
	ifaceNode := vs.Type
	callExp, ok := valueNode.(*ast.CallExpr)
	// if the ValueSpec is `var _ = myInterface(...)`
	// as opposed to `var _ myInterface = ...`
	if ifaceNode == nil && ok {
		ifaceNode = callExp.Fun
	}
	ifaceObj, ifacePkg := inspectNode(ifaceNode)
	if ifaceObj == nil || ifacePkg == nil {
		return nil
	}
	concreteObj, concretePkg := inspectNode(valueNode)
	if concreteObj == nil || concretePkg == nil {
		return nil
	}
	var pointer bool
	ast.Inspect(valueNode, func(n ast.Node) bool {
		if ue, ok := n.(*ast.UnaryExpr); ok && ue.Op == token.AND {
			pointer = true
			return false
		}
		if _, ok := n.(*ast.StarExpr); ok {
			pointer = true
			return false
		}
		return true
	})
	return &stubRequest{
		concreteObj: concreteObj,
		concretePkg: concretePkg,
		ifaceObj:    ifaceObj,
		ifacePkg:    ifacePkg,
		pointer:     pointer,
	}
}

func isVariableDeclaration(nodes []ast.Node) *ast.ValueSpec {
	for _, n := range nodes {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			continue
		}
		return vs
	}
	return nil
}

// concreteType is the destination type
// that will implement the interface methods
type concreteType struct {
	pkg          *types.Package
	fset         *token.FileSet
	file         *ast.File
	tms, pms     *types.MethodSet
	addedImports []*addedImport
}

func (ct *concreteType) doesNotHaveMethod(name string) bool {
	return ct.tms.Lookup(ct.pkg, name) == nil && ct.pms.Lookup(ct.pkg, name) == nil
}

func (ct *concreteType) getMethodSelection(name string) *types.Selection {
	if sel := ct.tms.Lookup(ct.pkg, name); sel != nil {
		return sel
	}
	return ct.pms.Lookup(ct.pkg, name)
}

func (ct *concreteType) addImport(name, path string) {
	for _, imp := range ct.addedImports {
		if imp.Name == name && imp.Path == path {
			return
		}
	}
	ct.addedImports = append(ct.addedImports, &addedImport{name, path})
}

// addedImport represents a newly added import
// statement to the concrete type. If name is not
// empty, then that import is required to have that name.
type addedImport struct {
	Name, Path string
}

func isIgnoredImport(imp *ast.ImportSpec) bool {
	return imp.Name != nil && (imp.Name.Name == "." || imp.Name.Name == "_")
}

type mismatchError struct {
	name       string
	have, want *types.Signature
}

func (me *mismatchError) Error() string {
	return fmt.Sprintf("mimsatched %q function singatures:\nhave: %s\nwant: %s", me.name, me.have, me.want)
}
