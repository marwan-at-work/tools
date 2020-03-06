package lsp

import (
	"bufio"
	"context"
	"log"
	"math/rand"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/tools/internal/gocommand"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
	errors "golang.org/x/xerrors"
)

func (s *Server) executeCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
	switch params.Command {
	case "generate":
		gr, err := getGenerateRequest(params.Arguments)
		if err != nil {
			return nil, err
		}
		go s.runGenerate(gr)
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

type genWriter struct{}

func (genWriter) Write(p []byte) (n int, err error) {
	log.Printf("generate: %s", p)
	return len(p), nil
}

func (s *Server) runGenerate(req source.GenerateRequest) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{"generate", "-x"}
	if req.Recursive {
		args = append(args, "./...")
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = req.Dir
	cmd.Env = s.session.Options().Env
	cmd.Stdout = genWriter{}
	out, _ := cmd.StderrPipe()
	scnr := bufio.NewScanner(out)
	token := strconv.FormatInt(rand.Int63(), 10)
	s.inProgressMu.Lock()
	s.inProgress[token] = cancel
	s.inProgressMu.Unlock()
	defer s.clearInProgress(token)
	err := s.client.WorkDoneProgressCreate(ctx, &protocol.WorkDoneProgressCreateParams{
		Token: token,
	})
	if err != nil {
		return
	}
	s.client.Progress(ctx, &protocol.ProgressParams{
		Token: token,
		Value: protocol.WorkDoneProgressBegin{
			Kind:        "begin",
			Title:       "generate",
			Cancellable: true,
			Message:     "running go generate",
		},
	})
	err = cmd.Start()
	if err != nil {
		return
	}
	for scnr.Scan() {
		txt := scnr.Text()
		log.Printf("generate: %v", txt)
		s.client.Progress(ctx, &protocol.ProgressParams{
			Token: token,
			Value: protocol.WorkDoneProgressReport{
				Kind:        "report",
				Cancellable: true,
				Message:     txt,
			},
		})
	}
	err = cmd.Wait()
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	if err != nil {
		log.Printf("generate: command error: %v", err)
		s.client.ShowMessage(ctx, &protocol.ShowMessageParams{
			Type:    protocol.Error,
			Message: "go generate exited with an error, check gopls logs",
		})
	}
	s.client.Progress(ctx, &protocol.ProgressParams{
		Token: token,
		Value: protocol.WorkDoneProgressEnd{
			Kind:    "end",
			Message: "finished",
		},
	})
}

func (s *Server) clearInProgress(token string) {
	s.inProgressMu.Lock()
	delete(s.inProgress, token)
	s.inProgressMu.Unlock()
}

func getGenerateRequest(args []interface{}) (source.GenerateRequest, error) {
	var gr source.GenerateRequest
	if len(args) != 1 {
		return gr, errors.Errorf("expected exactly 1 argument but got %d", len(args))
	}
	mp, ok := args[0].(map[string]interface{})
	if !ok {
		return gr, errors.Errorf("expected request to be a map[string]interface{} but got %T", args[0])
	}
	gr.Dir, ok = mp["Dir"].(string)
	if !ok {
		return gr, errors.Errorf("expected a Dir key to a string value")
	}
	gr.Recursive, _ = mp["Recursive"].(bool)
	return gr, nil
}
