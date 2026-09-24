package interrupt

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// One test, because cancelling is one-way: once Context is done it stays
// done for the rest of the package's run.
func TestCancelStopsChildrenAndWaits(t *testing.T) {
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
