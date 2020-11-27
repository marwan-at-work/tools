package stub

import (
	"context"
	"io"
)

var _ embeddedInterface = (*embeddedConcrete)(nil) //@suggestedfix("(", "quickfix")

type embeddedConcrete struct{}

type embeddedInterface interface {
	io.Reader
	context.Context
}
