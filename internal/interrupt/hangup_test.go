//go:build !windows

package interrupt

import (
	"syscall"
	"testing"
	"time"
)

// A terminal closing or an ssh session dropping sends SIGHUP, and SIGQUIT is
// Ctrl-\: both end the run in the same order a Ctrl-C does, rather than by
// Go's default, which leaves the decrypted bundle in $TMPDIR.
func TestHangupStopsInOrder(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			fresh(t)

			stop := Watch()
			defer stop()

			if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
				t.Fatal(err)
			}

			deadline := time.Now().Add(5 * time.Second)
			for !Interrupted() {
				if time.Now().After(deadline) {
					t.Fatalf("%s did not stop the run", sig)
				}

				time.Sleep(10 * time.Millisecond)
			}

			if got, want := ExitCode(), 128+int(sig); got != want {
				t.Errorf("exit code %d, want %d", got, want)
			}
		})
	}
}
