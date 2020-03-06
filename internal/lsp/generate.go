// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"context"
	"io"
	"log"
	"math/rand"
	"os/exec"
	"strconv"

	"golang.org/x/tools/internal/lsp/protocol"
	errors "golang.org/x/xerrors"
)

func (s *Server) runGenerate(dir string, recursive bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	token := strconv.FormatInt(rand.Int63(), 10)
	s.inProgressMu.Lock()
	s.inProgress[token] = cancel
	s.inProgressMu.Unlock()
	defer s.clearInProgress(token)

	args := []string{"generate", "-x"}
	if recursive {
		args = append(args, "./...")
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = s.session.Options().Env
	gp := &withGenPrefix{w: log.Writer()}
	wc := s.newProgressWriter(ctx, cancel, token)
	defer wc.Close()
	cmd.Stdout = gp
	cmd.Stderr = io.MultiWriter(gp, wc)
	err := cmd.Run()
	if err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		log.Printf("generate: command error: %v", err)
		s.client.ShowMessage(ctx, &protocol.ShowMessageParams{
			Type:    protocol.Error,
			Message: "go generate exited with an error, check gopls logs",
		})
	}
}

func (s *Server) clearInProgress(token string) {
	s.inProgressMu.Lock()
	delete(s.inProgress, token)
	s.inProgressMu.Unlock()
}

// withGenPrefix prefixes the incoming []byte in Write method
// with "generate: ". This is done to distinguish between
// regular LSP logs and "go generate" logs.
type withGenPrefix struct{ w io.Writer }

func (gw *withGenPrefix) Write(p []byte) (n int, err error) {
	const (
		genPrefix    = "generate: "
		genPrefixLen = len(genPrefix)
	)
	n, err = gw.w.Write(append([]byte(genPrefix), p...))
	if n >= genPrefixLen {
		n -= genPrefixLen
	}
	return n, err
}

// newProgressWriter returns an io.WriterCloser that can be used
// to report progress on the "go generate" command based on the
// client capabilities.
func (s *Server) newProgressWriter(ctx context.Context, cancel func(), token string) io.WriteCloser {
	var wc interface {
		io.WriteCloser
		start()
	}
	if s.supportsWorkDoneProgress {
		wc = &workDoneWriter{ctx, token, s.client}
	} else {
		wc = &messageWriter{cancel, ctx, s.client}
	}
	wc.start()
	return wc
}

// messageWriter implements progressWriter
// and only tells the user that "go generate"
// has started through window/showMessage but does not
// report anything afterwards. This is because each
// log shows up as a separate window and therefore
// would be obnoxious to show every incoming line.
type messageWriter struct {
	cancel func()
	ctx    context.Context
	client protocol.Client
}

func (lw *messageWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

func (lw *messageWriter) start() {
	go func() {
		msg, _ := lw.client.ShowMessageRequest(lw.ctx, &protocol.ShowMessageRequestParams{
			Type:    protocol.Log,
			Message: "go generate has started, check logs for progress",
			Actions: []protocol.MessageActionItem{{
				Title: "Cancel",
			}},
		})
		if msg != nil && msg.Title == "Cancel" {
			lw.cancel()
		}
	}()
}

func (lw *messageWriter) Close() error {
	return lw.client.ShowMessage(lw.ctx, &protocol.ShowMessageParams{
		Type:    protocol.Info,
		Message: "go generate has finished",
	})
}

// workDoneWriter implements progressWriter
// that will send $/progress notifications
// to the client
type workDoneWriter struct {
	ctx    context.Context
	token  string
	client protocol.Client
}

func (pw *workDoneWriter) Write(p []byte) (n int, err error) {
	return len(p), pw.client.Progress(pw.ctx, &protocol.ProgressParams{
		Token: pw.token,
		Value: &protocol.WorkDoneProgressReport{
			Kind:        "report",
			Cancellable: true,
			Message:     string(p),
		},
	})
}

func (pw *workDoneWriter) start() {
	err := pw.client.WorkDoneProgressCreate(pw.ctx, &protocol.WorkDoneProgressCreateParams{
		Token: pw.token,
	})
	if err != nil {
		return
	}
	pw.client.Progress(pw.ctx, &protocol.ProgressParams{
		Token: pw.token,
		Value: &protocol.WorkDoneProgressBegin{
			Kind:        "begin",
			Cancellable: true,
			Message:     "running go generate",
			Title:       "generate",
		},
	})
}

func (pw *workDoneWriter) Close() error {
	return pw.client.Progress(pw.ctx, &protocol.ProgressParams{
		Token: pw.token,
		Value: protocol.WorkDoneProgressEnd{
			Kind:    "end",
			Message: "finished",
		},
	})
}
