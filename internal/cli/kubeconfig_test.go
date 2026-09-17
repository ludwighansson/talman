package cli

import (
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
)

// The destination is positional and must survive --extra-flags, which is
// appended verbatim and would otherwise sit between the flag and its value.
func TestKubeconfigArgs(t *testing.T) {
	from := &config.Node{Hostname: "c01", IPAddress: "10.0.0.11"}

	tests := []struct {
		name  string
		dest  string
		merge bool
		force bool
		extra []string
		want  string
	}{
		{
			name: "talman's own copy is overwritten",
			dest: "/cluster/clusterconfig/kubeconfig",
			want: "--talosconfig /tmp/tc kubeconfig --nodes 10.0.0.11 --merge=false " +
				"/cluster/clusterconfig/kubeconfig",
		},
		{
			name:  "a named file is merged into",
			dest:  "/home/o/.kube/config",
			merge: true,
			want:  "--talosconfig /tmp/tc kubeconfig --nodes 10.0.0.11 --merge=true /home/o/.kube/config",
		},
		{
			name:  "force accompanies a merge",
			dest:  "/home/o/.kube/config",
			merge: true,
			force: true,
			want: "--talosconfig /tmp/tc kubeconfig --nodes 10.0.0.11 --merge=true --force " +
				"/home/o/.kube/config",
		},
		{
			name:  "the destination stays last",
			dest:  "/cluster/clusterconfig/kubeconfig",
			extra: []string{"--force-context-name=sto1"},
			want: "--talosconfig /tmp/tc kubeconfig --nodes 10.0.0.11 --merge=false " +
				"--force-context-name=sto1 /cluster/clusterconfig/kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(kubeconfigArgs("/tmp/tc", from, tt.dest, tt.merge, tt.force, tt.extra), " ")
			if got != tt.want {
				t.Errorf("kubeconfigArgs() =\n  %s\nwant\n  %s", got, tt.want)
			}
		})
	}
}

// Whatever else changes, a destination is always named: with none, talosctl
// merges into whichever kubeconfig the environment points at.
func TestKubeconfigAlwaysNamesADestination(t *testing.T) {
	from := &config.Node{Hostname: "c01", IPAddress: "10.0.0.11"}

	for _, dest := range []string{"/cluster/clusterconfig/kubeconfig", "-", "/home/o/.kube/config"} {
		args := kubeconfigArgs("/tmp/tc", from, dest, false, false, nil)
		if args[len(args)-1] != dest {
			t.Errorf("destination %q is not the last argument: %v", dest, args)
		}
	}
}
