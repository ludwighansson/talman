package interrupt

import (
	"context"
	"testing"
)

// fresh gives a test an uncancelled Context, and puts one back afterwards:
// cancelling is one-way for the process, and a test that relied on the order
// the others happened to run in failed under -count=2 and -shuffle.
func fresh(t *testing.T) {
	t.Helper()

	reset := func() {
		mu.Lock()
		defer mu.Unlock()

		ctx, cancel = context.WithCancel(context.Background())
		interruptedBy = nil

		runMu.Lock()
		killing = false
		runMu.Unlock()
	}

	reset()
	t.Cleanup(reset)
}
