// Package interrupt turns SIGINT and SIGTERM into an orderly stop.
//
// Left to Go's default, either signal ends the process on the spot: no
// deferred function runs, so the decrypted secrets bundle a render stages in
// $TMPDIR stays there, and a talosctl child that a CI runner's SIGTERM never
// reached carries on applying or resetting after talman is gone.
//
// Instead the first signal cancels Context. Every talosctl and sops process
// talman starts is bound to it and is signalled in turn, every wait selects on
// it, and the command unwinds through its deferred cleanup and its metrics the
// way a failure does. A second signal is for the run that will not stop: it
// removes whatever was registered with Cleanup and exits at once.
package interrupt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

// ExitCode is what an interrupted run exits with: 128 + SIGINT, as a shell
// reports it.
const ExitCode = 130

// waitDelay is how long a child gets to exit after it has been signalled
// before it is killed outright.
const waitDelay = 10 * time.Second

var (
	ctx, cancel = context.WithCancel(context.Background())

	mu       sync.Mutex
	cleanups = map[int]func(){}
	nextID   int
)

// Context is cancelled by the first SIGINT or SIGTERM.
func Context() context.Context { return ctx }

// Interrupted reports whether a signal has cancelled the run.
func Interrupted() bool { return ctx.Err() != nil }

// Watch installs the signal handler. The returned function uninstalls it.
func Watch() (stop func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})

	go func() {
		select {
		case sig := <-ch:
			fmt.Fprintf(os.Stderr, "\n%s: stopping (again to exit immediately)\n", sig)
			cancel()
		case <-done:
			return
		}

		select {
		case <-ch:
			RunCleanups()
			os.Exit(ExitCode)
		case <-done:
		}
	}()

	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// Cleanup registers fn to run if a second signal forces an immediate exit.
// The returned function unregisters it; call it once the ordinary cleanup has
// run.
func Cleanup(fn func()) (unregister func()) {
	mu.Lock()
	defer mu.Unlock()

	id := nextID
	nextID++
	cleanups[id] = fn

	return func() {
		mu.Lock()
		defer mu.Unlock()

		delete(cleanups, id)
	}
}

// RemoveAllOnExit registers dir for removal on a forced exit.
func RemoveAllOnExit(dir string) (unregister func()) {
	return Cleanup(func() { _ = os.RemoveAll(dir) })
}

// RunCleanups runs every registered cleanup.
func RunCleanups() {
	mu.Lock()
	defer mu.Unlock()

	for id, fn := range cleanups {
		fn()
		delete(cleanups, id)
	}
}

// Command is exec.Command bound to Context: when the run is interrupted the
// child is sent SIGTERM, and killed if it has not exited within waitDelay.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // args are built by talman, not user shell input

	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}

		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = waitDelay

	return cmd
}

// Sleep waits for d, or until the run is interrupted, whichever is first.
func Sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
