package stub

import (
	"context"
	mytime "time"
)

var _ context.Context = &myContext{} //@suggestedfix("&", "quickfix")
var _ = mytime.Time{}

type myContext struct{}
