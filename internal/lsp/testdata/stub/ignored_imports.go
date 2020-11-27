package stub

import (
	"context"
	. "time"
	_ "time"
)

// This file tests that dot-imports and underscore imports
// are properly ignored and that a new import is added to
// refernece method types

var (
	_                 = Time{}
	_ context.Context = (*ignoredContext)(nil) //@suggestedfix("(", "quickfix")
)

type ignoredContext struct{}
