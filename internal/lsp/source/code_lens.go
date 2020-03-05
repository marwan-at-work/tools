package source

import (
	"context"
	"path/filepath"
	"strings"

	"golang.org/x/tools/internal/lsp/protocol"
)

// GenerateRequest specifies the directory from which
// gopls would run "go generate" as well as whether to
// recursively run "go generate" for all of its sub directories
type GenerateRequest struct {
	Dir       string
	Recursive bool
}

func CodeLens(ctx context.Context, snapshot Snapshot, fh FileHandle, supportsWorkDoneProgress bool) ([]protocol.CodeLens, error) {
	if !supportsWorkDoneProgress {
		return nil, nil
	}
	f, _, m, _, err := snapshot.View().Session().Cache().ParseGoHandle(fh, ParseFull).Parse(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range f.Comments {
		for _, l := range c.List {
			if strings.HasPrefix(l.Text, "//go:generate") {
				fset := snapshot.View().Session().Cache().FileSet()
				rng, err := newMappedRange(fset, m, l.Pos(), l.End()).Range()
				if err != nil {
					return nil, err
				}
				return []protocol.CodeLens{
					{
						Range: rng,
						Command: protocol.Command{
							Title:   "run go generate",
							Command: "generate",
							Arguments: []interface{}{GenerateRequest{
								Dir: filepath.Dir(fh.Identity().URI.Filename()),
							}},
						},
					},
					{
						Range: rng,
						Command: protocol.Command{
							Title:   "run go generate ./...",
							Command: "generate",
							Arguments: []interface{}{GenerateRequest{
								Dir:       filepath.Dir(fh.Identity().URI.Filename()),
								Recursive: true,
							}},
						},
					},
				}, nil
			}
		}
	}
	return nil, nil
}
