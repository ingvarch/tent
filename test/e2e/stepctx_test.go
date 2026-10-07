package e2e

import (
	"context"
	"io"
	"testing"
)

// interruptedText is what the suite prints once when a signal ends the run.
const interruptedText = "e2e: interrupted; deleting the clusters of this run\n"

// joinContext returns a context that ends when parent or other ends. Call release when the context is no
// longer used: it ends the context and stops the wait on other.
func joinContext(parent, other context.Context) (ctx context.Context, release context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(other, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// stepContext returns the context for the calls of a test. It ends when the test's own context or suiteCtx ends, and
// suiteCtx ends when a signal interrupts the run. It is released when the test ends.
func stepContext(suiteCtx context.Context, t testing.TB) context.Context {
	ctx, release := joinContext(t.Context(), suiteCtx)
	t.Cleanup(release)
	return ctx
}

// announceInterrupt writes interruptedText to out when ctx ends, unless the returned stop is called first.
func announceInterrupt(ctx context.Context, out io.Writer) (stop func() bool) {
	return context.AfterFunc(ctx, func() { _, _ = io.WriteString(out, interruptedText) })
}
