package stub

import (
	"io"
)

func newCloser() io.Closer {
	return &closer{} //@suggestedfix("&", "quickfix")
}

type closer struct{}
