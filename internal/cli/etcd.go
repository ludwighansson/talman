package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newEtcdCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd",
		Short: "Work with the cluster's etcd",
	}

	cmd.AddCommand(recorded(newEtcdSnapshotCmd()))

	return cmd
}

func newEtcdSnapshotCmd() *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "snapshot [path]",
		Short: "Save a snapshot of etcd",
		Long: `Snapshot streams a snapshot of etcd from one control plane to a file: the
first in the config that answers the Talos API, or --node.

Without a path it lands in the output directory as
etcd-<cluster>-<UTC time>.db, beside the configs and credentials it is as
sensitive as: an etcd snapshot holds every Kubernetes Secret in the cluster.
It is written 0600 wherever it goes.

"talman upgrade --snapshot" and "talman upgrade-k8s --snapshot" take one before
they change anything.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)

			var from *config.Node

			if node != "" {
				if from, err = controlPlane(cfg, node); err != nil {
					return err
				}
			}

			path := ""
			if len(args) == 1 {
				path = args[0]
			}

			return etcdSnapshot(cfg, tal, tc, from, path)
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to snapshot (default: the first that answers)")

	return cmd
}

// etcdSnapshot saves a snapshot and says where it went. A nil from picks
// the first control plane that answers; an empty path picks a name in the
// output directory.
func etcdSnapshot(cfg *config.Config, tal *talosctl.Runner, tc string, from *config.Node, path string) error {
	if from == nil {
		n, err := healthNode(cfg, tal, tc)
		if err != nil {
			return err
		}

		from = n
	} else if !tal.Reachable(tc, from.IPAddress) {
		// Asked, as healthNode asks, so the snapshot goes whichever way the
		// node answered: pinned to it, or through the talosconfig's
		// endpoints when the node is only reachable behind them.
		return fmt.Errorf("%s (%s) does not answer the Talos API, directly or through the talosconfig's endpoints",
			from.Hostname, from.IPAddress)
	}

	if path == "" {
		if err := render.PrepareOutput(cfg, os.Stderr); err != nil {
			return err
		}

		stem := filepath.Join(cfg.OutputPath(),
			fmt.Sprintf("etcd-%s-%s", cfg.ClusterName, time.Now().UTC().Format("20060102T150405Z")))

		// Numbered rather than refused: two snapshots in one second are two
		// snapshots, and the name is talman's to choose.
		path = stem + ".db"
		for i := 2; exists(path); i++ {
			path = fmt.Sprintf("%s-%d.db", stem, i)
		}
	}

	if exists(path) {
		return fmt.Errorf("%s already exists; talman does not overwrite a snapshot", render.Rel(path))
	}

	fmt.Fprintf(os.Stderr, "== snapshotting etcd on %s (%s)\n", from.Hostname, from.IPAddress)

	// talosctl writes with the umask's permissions, usually 0644, and takes
	// seconds to stream a file holding every Secret in the cluster. So it
	// writes into a private directory beside the destination, and the file is
	// moved into place only once it is 0600.
	stage, err := os.MkdirTemp(filepath.Dir(path), ".talman-snapshot-")
	if err != nil {
		return err
	}

	unregister := interrupt.RemoveAllOnExit(stage)
	defer func() {
		_ = os.RemoveAll(stage)

		unregister()
	}()

	staged := filepath.Join(stage, "snapshot.db")
	args := append(tal.NodeArgs(tc, from.IPAddress), "etcd", "snapshot", staged)

	if out, err := tal.Combined(args...); err != nil {
		return fmt.Errorf("%w\n%s", err, indent(out))
	}

	if err := os.Chmod(staged, 0o600); err != nil {
		return err
	}

	if exists(path) {
		return fmt.Errorf("%s appeared while the snapshot was taken; talman does not overwrite a snapshot",
			render.Rel(path))
	}

	if err := os.Rename(staged, path); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "wrote %s\n", render.Rel(path))

	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}
