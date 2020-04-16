// Package impl can generate method stubs to implement a given interface
// For usage and installation, see README file
package impl

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"log"
	"strconv"
	"strings"
	"text/template"

	"golang.org/x/tools/go/ast/astutil"
)

// Implementation defines the results of
// the implement method
type Implementation struct {
	File         string         // path to the Go file of the implementing type
	FileContent  []byte         // the Go file plus the method implementations at the bottom of the file
	Methods      []byte         // only the method implementations, helpful if you want to insert the methods elsewhere in the file
	AddedImports []*AddedImport // all the required imports for the methods, it does not filter out imports already imported by the file
	Node         ast.Node
	Error        error // any error encountered during the process
}

// AddedImport represents a newly added import
// statement to the concrete type. If name is not
// empty, then that import is required to have that name.
type AddedImport struct {
	Name, Path string
}

// Package pkg
type Package struct {
	Fset      *token.FileSet
	Target    string
	Files     []*ast.File
	Content   []byte
	Types     *types.Package
	Imports   map[string]*Package
	TypesInfo *types.Info
}

// Implement an interface and return the path to as well as the content of the
// file where the concrete type was defined updated with all of the missing methods
func Implement(
	ifacePkg *Package,
	implPkg *Package,
) (*Implementation, error) {
	ifacePath := ifacePkg.Types.Path()
	iface := ifacePkg.Target
	implPath := implPkg.Types.Path()
	impl := implPkg.Target
	ifaceObj := ifacePkg.Types.Scope().Lookup(iface)
	if ifaceObj == nil {
		return nil, fmt.Errorf("could not find interface declaration (%s) in %s", iface, ifacePath)
	}
	implObj := implPkg.Types.Scope().Lookup(impl)
	if implObj == nil {
		return nil, fmt.Errorf("could not find type declaration (%s) in %s", impl, implPath)
	}
	implFilename, implFileAST := getFile(implPkg.Files, implPkg.Fset, implObj)
	ct := &concreteType{
		pkg:  implPkg.Types,
		fset: ifacePkg.Fset,
		file: implFileAST,
		tms:  types.NewMethodSet(implObj.Type()),
		pms:  types.NewMethodSet(types.NewPointer(implObj.Type())),
	}
	missing, err := missingMethods(ct, ifacePkg, map[string]struct{}{})
	if err != nil {
		return nil, err
	}
	if len(missing) == 0 {
		return nil, nil
	}
	var methodsBuffer bytes.Buffer
	for _, mm := range missing {
		t := template.Must(template.New("").Parse(tmpl))
		for _, m := range mm.missing {
			var sig bytes.Buffer

			nn, _ := astutil.PathEnclosingInterval(mm.file, m.Pos(), m.Pos())
			var n ast.Node = nn[1].(*ast.Field).Type
			n = copyAST(n)
			n = astutil.Apply(n, func(c *astutil.Cursor) bool {
				sel, ok := c.Node().(*ast.SelectorExpr)
				if ok {
					renamed := mightRenameSelector(c, sel, ifacePkg.TypesInfo, ct)
					removed := mightRemoveSelector(c, sel, ifacePkg.TypesInfo, implPath)
					return removed || renamed
				}
				ident, ok := c.Node().(*ast.Ident)
				if ok {
					return mightAddSelector(c, ident, ifacePkg, ct)
				}
				return true
			}, nil)
			err = format.Node(&sig, ifacePkg.Fset, n)
			if err != nil {
				return nil, fmt.Errorf("could not format function signature: %w", err)
			}
			md := methodData{
				Name:        m.Name(),
				Implementer: impl,
				Interface:   iface,
				Signature:   strings.TrimPrefix(sig.String(), "func"),
			}
			err = t.Execute(&methodsBuffer, md)
			if err != nil {
				return nil, fmt.Errorf("error executing method template: %w", err)
			}
			methodsBuffer.WriteRune('\n')
		}
	}
	nodes, _ := astutil.PathEnclosingInterval(implFileAST, implObj.Pos(), implObj.Pos())
	insertPos := implPkg.Fset.Position(nodes[1].End())
	offset := insertPos.Offset
	var buf bytes.Buffer
	buf.Write(implPkg.Content[:offset])
	buf.WriteByte('\n')
	buf.Write(methodsBuffer.Bytes())
	buf.Write(implPkg.Content[offset:])
	fset := token.NewFileSet()
	newF, err := parser.ParseFile(fset, implFilename, buf.Bytes(), parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("could not reparse file: %w", err)
	}
	for _, imp := range ct.addedImports {
		astutil.AddNamedImport(fset, newF, imp.Name, imp.Path)
	}
	var source bytes.Buffer
	err = format.Node(&source, fset, newF)
	if err != nil {
		return nil, err
	}
	return &Implementation{
		File:         implFilename,
		FileContent:  source.Bytes(),
		Methods:      methodsBuffer.Bytes(),
		Error:        err,
		AddedImports: ct.addedImports,
		Node:         nodes[1],
	}, err
}

// mightRemoveSelector will replace a selector such as *models.User to just be *User.
// This is needed if the interface method imports the same package where the concrete type
// is going to implement that method
func mightRemoveSelector(c *astutil.Cursor, sel *ast.SelectorExpr, ifacePkgInfo *types.Info, implPath string) bool {
	obj := ifacePkgInfo.Uses[sel.Sel]
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
func mightRenameSelector(c *astutil.Cursor, sel *ast.SelectorExpr, ifaceInfo *types.Info, ct *concreteType) bool {
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	obj := ifaceInfo.Uses[ident]
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
	if imp, ok := ct.hasImport(pkg.Path()); ok {
		hasImport = true
		importName = pkg.Name()
		if imp.Name != nil && imp.Name.Name != pkg.Name() {
			importName = imp.Name.Name
		}
	}

	if hasImport {
		ident.Name = importName
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
	ifacePkg *Package,
	ct *concreteType,
) bool {
	if ident.Name == "Problem" {
		log.Printf("HELLO!\n")
	}
	obj := ifacePkg.TypesInfo.Uses[ident]
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
	if imp, ok := ct.hasImport(pkg.Path()); ok {
		missingImport = false
		if imp.Name != nil {
			pkgName = imp.Name.Name
		}
	}
	isNotImportingDestination := pkg.Path() != ct.pkg.Path()
	if missingImport && isNotImportingDestination {
		ct.addImport("", pkg.Path())
	}
	isLocalDeclaration := pkg.Path() == ifacePkg.Types.Path() && pkg.Path() != ct.pkg.Path()
	isDotImport := pkg.Path() != ifacePkg.Types.Path() && pkg.Path() != ct.pkg.Path()
	if ident.Name == "Problem" {
		log.Printf("PKG PATH: %v -- ifaceTP: %v --- ctPath: %v \n\n", pkg.Path(), ifacePkg.Types.Path(), ct.pkg.Path())
	}
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

type methodData struct {
	Name        string
	Interface   string
	Implementer string
	Signature   string
}

const tmpl = `// {{ .Name }} implements {{ .Interface }}
func (*{{ .Implementer }}) {{ .Name }}{{ .Signature }} {
	panic("unimplemented")
}
`

type mismatchError struct {
	name       string
	have, want *types.Signature
}

func (me *mismatchError) Error() string {
	return fmt.Sprintf("mimsatched %q function singatures:\nhave: %s\nwant: %s", me.name, me.have, me.want)
}

// missingInterface represents an interface
// that has all or some of its methods missing
// from the destination concrete type
type missingInterface struct {
	iface   *types.Interface
	file    *ast.File
	missing []*types.Func
}

// concreteType is the destination type
// that will implement the interface methods
type concreteType struct {
	pkg          *types.Package
	fset         *token.FileSet
	file         *ast.File
	tms, pms     *types.MethodSet
	addedImports []*AddedImport
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
	ct.addedImports = append(ct.addedImports, &AddedImport{name, path})
}

func (ct *concreteType) hasImport(path string) (*ast.ImportSpec, bool) {
	for _, imp := range ct.file.Imports {
		impPath, _ := strconv.Unquote(imp.Path.Value)
		if impPath == path && !isIgnoredImport(imp) {
			return imp, true
		}
	}
	for _, imp := range ct.addedImports {
		if imp.Path == path {
			var name *ast.Ident
			if imp.Name != "" {
				name = &ast.Ident{Name: imp.Name}
			}
			return &ast.ImportSpec{Path: &ast.BasicLit{Value: imp.Path}, Name: name}, true
		}
	}
	return nil, false
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
func missingMethods(ct *concreteType, ifacePkg *Package, visited map[string]struct{}) ([]*missingInterface, error) {
	ifaceObj := ifacePkg.Types.Scope().Lookup(ifacePkg.Target)
	iface, ok := ifaceObj.Type().Underlying().(*types.Interface)
	if !ok {
		return nil, fmt.Errorf("expected %v to be an interface but got %T", iface, ifaceObj.Type().Underlying())
	}
	missing := []*missingInterface{}
	for i := 0; i < iface.NumEmbeddeds(); i++ {
		eiface := iface.Embedded(i).Obj()
		depPkg := ifacePkg
		if eiface.Pkg().Path() != ifacePkg.Types.Path() {
			depPkg = ifacePkg.Imports[eiface.Pkg().Path()]
			if depPkg == nil {
				return nil, fmt.Errorf("missing dependency for %v", eiface.Name())
			}
		}
		depPkg.Target = eiface.Name()
		em, err := missingMethods(ct, depPkg, visited)
		if err != nil {
			return nil, err
		}
		missing = append(missing, em...)
	}
	_, astFile := getFile(ifacePkg.Files, ifacePkg.Fset, ifaceObj)
	mm := &missingInterface{
		iface: iface,
		file:  astFile,
	}
	if mm.file == nil {
		return nil, fmt.Errorf("could not find ast.File for %v", ifaceObj.Name())
	}
	for i := 0; i < iface.NumExplicitMethods(); i++ {
		method := iface.ExplicitMethod(i)
		if ct.doesNotHaveMethod(method.Name()) {
			if _, ok := visited[method.Name()]; !ok {
				mm.missing = append(mm.missing, method)
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
	if len(mm.missing) > 0 {
		missing = append(missing, mm)
	}
	return missing, nil
}

// getFile returns the local path to as well as the AST of a Go file where
// the given types.Object was defined.
func getFile(files []*ast.File, fset *token.FileSet, obj types.Object) (string, *ast.File) {
	objFile := fset.Position(obj.Pos()).Filename
	for _, astFile := range files {
		f := fset.Position(astFile.Pos()).Filename
		if strings.HasSuffix(f, objFile) {
			return objFile, astFile
		}
	}
	return "", nil
}

func isIgnoredImport(imp *ast.ImportSpec) bool {
	return imp.Name != nil && (imp.Name.Name == "." || imp.Name.Name == "_")
}
