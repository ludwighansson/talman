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

// ExitCode is what an interrupted run exits with: 128 + the signal, as a shell
// reports it -- 130 for Ctrl-C, 143 for a CI runner's SIGTERM, so tooling can
// tell a person stopping a run from a job being cancelled.
func ExitCode() int {
	mu.Lock()
	defer mu.Unlock()

	if s, ok := interruptedBy.(syscall.Signal); ok {
		return 128 + int(s)
	}

	return 128 + int(syscall.SIGINT)
}

// waitDelay is how long a child gets to exit after it has been signalled
// before it is killed outright.
const waitDelay = 10 * time.Second

var (
	ctx, cancel = context.WithCancel(context.Background())

	mu       sync.Mutex
	cleanups = map[int]func(){}
	nextID   int

	running = map[*os.Process]bool{}

	// interruptedBy is the first signal, for ExitCode.
	interruptedBy os.Signal
)

// Context is cancelled by the first SIGINT or SIGTERM.
func Context() context.Context {
	mu.Lock()
	defer mu.Unlock()

	return ctx
}

// Interrupted reports whether a signal has cancelled the run.
func Interrupted() bool { return Context().Err() != nil }

// Watch installs the signal handler. The returned function uninstalls it.
func Watch() (stop func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})

	go func() {
		select {
		case sig := <-ch:
			fmt.Fprintf(os.Stderr, "\n%s: stopping (again to exit immediately)\n", sig)

			mu.Lock()
			interruptedBy = sig
			stop := cancel
			mu.Unlock()

			stop()
		case <-done:
			return
		}

		select {
		case <-ch:
			killRunning()
			RunCleanups()
			os.Exit(ExitCode())
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
	cmd := exec.CommandContext(Context(), name, args...) //nolint:gosec // args are built by talman, not user shell input

	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}

		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = waitDelay

	return cmd
}

// Run starts cmd and waits for it, like cmd.Run, keeping track of the process
// while it runs so a forced exit can kill it: exiting without doing so would
// leave a talosctl that ignored SIGTERM carrying on against the cluster after
// talman is gone.
func Run(cmd *exec.Cmd) error {
	mu.Lock()

	if err := cmd.Start(); err != nil {
		mu.Unlock()

		return err
	}

	proc := cmd.Process
	running[proc] = true
	mu.Unlock()

	defer func() {
		mu.Lock()
		delete(running, proc)
		mu.Unlock()
	}()

	return cmd.Wait()
}

func killRunning() {
	mu.Lock()
	defer mu.Unlock()

	for proc := range running {
		_ = proc.Kill()
	}
}

// Sleep waits for d, or until the run is interrupted, whichever is first.
func Sleep(d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-Context().Done():
		return Context().Err()
	}
}
