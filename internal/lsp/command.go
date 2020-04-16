// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"strings"

	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/impl"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
	"golang.org/x/tools/internal/span"
	"golang.org/x/tools/internal/xcontext"
	errors "golang.org/x/xerrors"
)

type x struct{}

// Write implements WriteCloser
func (*x) Write(p []byte) (n int, err error) {
	panic("unimplemented")
}

func blah() io.WriteCloser {
	return nil
}

func (s *Server) executeCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
	switch params.Command {
	case "generate":
		dir, recursive, err := getGenerateRequest(params.Arguments)
		if err != nil {
			return nil, err
		}
		go s.runGenerate(xcontext.Detach(ctx), dir, recursive)
	case "implement":
		ir, err := toIR(params.Arguments)
		if err != nil {
			return nil, err
		}
		v := s.session.View(ir.View)
		imps, err := v.Snapshot().CachedImportPaths(ctx)
		if err != nil {
			return nil, err
		}
		concreteSrcPkg := imps[ir.ConcretePath]
		obj := concreteSrcPkg.GetTypes().Scope().Lookup(ir.ConcreteName)
		fset := s.session.Cache().FileSet()
		concreteFilePath := fset.Position(obj.Pos()).Filename
		if err != nil {
			return nil, fmt.Errorf("could not get go handle for: %v", concreteFilePath)
		}
		pgh, err := concreteSrcPkg.File(span.URIFromPath(concreteFilePath))
		uri := protocol.URIFromPath(concreteFilePath)
		concretePkg, err := getPkgs(ctx, ir.ConcreteName, fset, concreteSrcPkg)
		if err != nil {
			return nil, fmt.Errorf("could not get concretePkg: %v", err)
		}
		var concreteFileAST *ast.File
		concreteFileAST, concretePkg.Content, _, _, err = pgh.Cached()
		if err != nil {
			return nil, fmt.Errorf("could not return cached file: %v", err)
		}
		ifaceSrcPkg := imps[ir.InterfacePath]
		ifacePkg, err := getPkgs(ctx, ir.InterfaceName, fset, ifaceSrcPkg)
		if err != nil {
			return nil, fmt.Errorf("could not get ifacePkg: %v", err)
		}
		resp, err := impl.Implement(
			ifacePkg,
			concretePkg,
		)
		if err != nil {
			return nil, fmt.Errorf("could not implement: %v", err)
		}
		rng, err := source.NodeToProtocolRange(v, concreteSrcPkg, concreteFileAST)
		if err != nil {
			return nil, errors.Errorf("could not get concrete file range: %v", err)
		}
		edits := []protocol.TextEdit{{
			Range:   rng,
			NewText: strings.TrimSpace(string(resp.FileContent)),
		}}
		_, err = s.client.ApplyEdit(v.BackgroundContext(), &protocol.ApplyWorkspaceEditParams{
			Label: "implement interface",
			Edit: protocol.WorkspaceEdit{
				DocumentChanges: []protocol.TextDocumentEdit{
					{
						TextDocument: protocol.VersionedTextDocumentIdentifier{
							TextDocumentIdentifier: protocol.TextDocumentIdentifier{
								URI: uri,
							},
						},
						Edits: edits,
					},
				},
			},
		})
		return nil, err
	case "tidy":
		if len(params.Arguments) == 0 || len(params.Arguments) > 1 {
			return nil, errors.Errorf("expected one file URI for call to `go mod tidy`, got %v", params.Arguments)
		}
		uri := protocol.DocumentURI(params.Arguments[0].(string))
		snapshot, _, ok, err := s.beginFileRequest(uri, source.Mod)
		if !ok {
			return nil, err
		}
		// Run go.mod tidy on the view.
		inv := gocommand.Invocation{
			Verb:       "mod",
			Args:       []string{"tidy"},
			Env:        snapshot.Config(ctx).Env,
			WorkingDir: snapshot.View().Folder().Filename(),
		}
		if _, err := inv.Run(ctx); err != nil {
			return nil, err
		}
	case "upgrade.dependency":
		if len(params.Arguments) < 2 {
			return nil, errors.Errorf("expected one file URI and one dependency for call to `go get`, got %v", params.Arguments)
		}
		uri := protocol.DocumentURI(params.Arguments[0].(string))
		deps := params.Arguments[1].(string)
		snapshot, _, ok, err := s.beginFileRequest(uri, source.UnknownKind)
		if !ok {
			return nil, err
		}
		// Run "go get" on the dependency to upgrade it to the latest version.
		inv := gocommand.Invocation{
			Verb:       "get",
			Args:       strings.Split(deps, " "),
			Env:        snapshot.Config(ctx).Env,
			WorkingDir: snapshot.View().Folder().Filename(),
		}
		if _, err := inv.Run(ctx); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func getGenerateRequest(args []interface{}) (string, bool, error) {
	if len(args) != 2 {
		return "", false, errors.Errorf("expected exactly 2 arguments but got %d", len(args))
	}
	dir, ok := args[0].(string)
	if !ok {
		return "", false, errors.Errorf("expected dir to be a string value but got %T", args[0])
	}
	recursive, ok := args[1].(bool)
	if !ok {
		return "", false, errors.Errorf("expected recursive to be a boolean but got %T", args[1])
	}
	return dir, recursive, nil
}

func getPkgs(ctx context.Context, target string, fset *token.FileSet, pkg source.Package) (*impl.Package, error) {
	files := []*ast.File{}
	for _, f := range pkg.CompiledGoFiles() {
		astF, _, _, _, err := f.Cached()
		if err != nil {
			return nil, err
		}
		files = append(files, astF)
	}
	mp := map[string]*impl.Package{}
	for _, imp := range pkg.Imports() {
		impPkg, err := getPkgs(ctx, "", fset, imp)
		if err != nil {
			return nil, err
		}
		mp[imp.GetTypes().Path()] = impPkg
	}
	return &impl.Package{
		Target:    target,
		Files:     files,
		Fset:      fset,
		Types:     pkg.GetTypes(),
		TypesInfo: pkg.GetTypesInfo(),
		Imports:   mp,
	}, nil
}
