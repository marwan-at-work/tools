// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"

	"golang.org/x/tools/internal/lsp/protocol"
	errors "golang.org/x/xerrors"
)

func (s *Server) workDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) error {
	token, ok := params.Token.(string)
	if !ok {
		return errors.Errorf("expected params.Token to be string but got %T", params.Token)
	}
	s.inProgressMu.Lock()
	defer s.inProgressMu.Unlock()
	cancel, ok := s.inProgress[token]
	if !ok {
		return errors.Errorf("token %q not found in progress", token)
	}
	cancel()
	return nil
}
