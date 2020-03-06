// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package source

import (
	"context"
	"go/token"
	"path/filepath"
	"strings"

	"golang.org/x/tools/internal/lsp/protocol"
)

func CodeLens(ctx context.Context, snapshot Snapshot, fh FileHandle, supportsWorkDoneProgress bool) ([]protocol.CodeLens, error) {
	f, _, m, _, err := snapshot.View().Session().Cache().ParseGoHandle(fh, ParseFull).Parse(ctx)
	if err != nil {
		return nil, err
	}
	const (
		ggDirective    = "//go:generate"
		ggDirectiveLen = len(ggDirective)
	)
	for _, c := range f.Comments {
		for _, l := range c.List {
			if !strings.HasPrefix(l.Text, ggDirective) {
				continue
			}
			fset := snapshot.View().Session().Cache().FileSet()
			rng, err := newMappedRange(fset, m, l.Pos(), l.Pos()+token.Pos(ggDirectiveLen)).Range()
			if err != nil {
				return nil, err
			}
			dir := filepath.Dir(fh.Identity().URI.Filename())
			return []protocol.CodeLens{
				{
					Range: rng,
					Command: protocol.Command{
						Title:     "run go generate",
						Command:   "generate",
						Arguments: []interface{}{dir},
					},
				},
				{
					Range: rng,
					Command: protocol.Command{
						Title:   "run go generate ./...",
						Command: "generate",
						Arguments: []interface{}{
							dir,
							true,
						},
					},
				},
			}, nil

		}
	}
	return nil, nil
}
