// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"
	"strings"

	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
	errors "golang.org/x/xerrors"
)

func (s *Server) executeCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
	switch params.Command {
	case "generate":
		dir, recursive, err := getGenerateRequest(params.Arguments)
		if err != nil {
			return nil, err
		}
		go s.runGenerate(dir, recursive)
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
	if len(args) != 1 && len(args) != 2 {
		return "", false, errors.Errorf("expected exactly 1 or 2 arguments but got %d", len(args))
	}
	dir, ok := args[0].(string)
	if !ok {
		return "", false, errors.Errorf("expected dir to be a string value but got %T", args[0])
	}
	var recursive bool
	if len(args) == 2 {
		recursive, ok = args[1].(bool)
		if !ok && len(args) == 2 {
			return "", false, errors.Errorf("expected recursive to be a boolean but got %T", args[1])
		}
	}
	return dir, recursive, nil
}
