package interrupt

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestForcedExitKillsChildren is the second signal's half: a child still
// running is killed rather than left behind.
func TestForcedExitKillsChildren(t *testing.T) {
	fresh(t)

	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sleep")
	}

	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}

	done := make(chan error, 1)

	go func() { done <- Run(Command("sleep", "30")) }()

	deadline := time.Now().Add(5 * time.Second)

	for {
		runMu.Lock()
		n := len(running)
		runMu.Unlock()

		if n > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the child was never recorded as running")
		}

		time.Sleep(10 * time.Millisecond)
	}

	killRunning()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a killed child reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the child survived killRunning")
	}

	runMu.Lock()
	defer runMu.Unlock()

	if len(running) != 0 {
		t.Errorf("%d process(es) still recorded after exiting", len(running))
	}
}

func TestCancelStopsChildrenAndWaits(t *testing.T) {
	fresh(t)

	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sleep")
	}

	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary")
	}

	dir := t.TempDir()
	staged := filepath.Join(dir, "staged")

	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}

	unregister := RemoveAllOnExit(staged)

	cmd := Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	slept := make(chan error, 1)

	go func() { slept <- Sleep(time.Minute) }()

	started := time.Now()

	cancel()

	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited cleanly; it should have been signalled")
	}

	if err := <-slept; err == nil {
		t.Fatal("Sleep returned nil after the run was interrupted")
	}

	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("stopping took %s; the child was not signalled promptly", elapsed)
	}

	if !Interrupted() {
		t.Fatal("Interrupted() is false after cancel")
	}

	if err := Command("true").Run(); err == nil {
		t.Fatal("a process started after the interrupt ran anyway")
	}

	RunCleanups()

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("registered directory survived RunCleanups: %v", err)
	}

	unregister() // after RunCleanups: must be harmless
}

func TestExitCodeFollowsTheSignal(t *testing.T) {
	fresh(t)

	if got := ExitCode(); got != 130 {
		t.Errorf("before any signal: %d, want 130", got)
	}

	interruptedBy = syscall.SIGTERM

	if got := ExitCode(); got != 143 {
		t.Errorf("after SIGTERM: %d, want 143", got)
	}
}

// A process that starts after a forced exit began killing the others is not
// left running: it registers too late to be on the list, so it kills itself.
func TestStartedDuringForcedExitIsKilled(t *testing.T) {
	fresh(t)

	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sleep")
	}

	// A start already in flight when the kill begins: the forced exit waits
	// for it, and it kills its own process on landing.
	runMu.Lock()
	starting++
	runMu.Unlock()

	drained := make(chan struct{})

	go func() {
		killRunning()
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("killRunning returned with a start still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	runMu.Lock()
	starting--
	runMu.Unlock()

	<-drained

	// And one asked for after the kill began is not started at all.
	done := make(chan error, 1)

	go func() { done <- Run(Command("sleep", "30")) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a process started during a forced exit ran to completion")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a process started during a forced exit was left running")
	}
}
