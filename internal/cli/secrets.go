package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/sopsx"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the cluster secrets bundle",
	}

	cmd.AddCommand(newSecretsGenerateCmd())

	return cmd
}

func newSecretsGenerateCmd() *cobra.Command {
	var (
		fromConfig string
		force      bool
		plaintext  bool
		toStdout   bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate the cluster secrets bundle",
		Long: `Generate creates a Talos secrets bundle with "talosctl gen secrets" and writes
it SOPS-encrypted according to the creation rule your .sops.yaml defines for
the destination path.

It refuses to overwrite an existing bundle: the certificate authorities and
cluster identity in it are what a running cluster trusts, so replacing them
orphans every node. Use --from-controlplane-config to adopt the secrets of a
cluster that already exists.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			tal := runner(cfg)
			if err := tal.Ensure(); err != nil {
				return err
			}

			dest := cfg.SecretPath()

			if !toStdout && !force {
				if _, err := os.Stat(dest); err == nil {
					return fmt.Errorf("%s already exists: regenerating secrets orphans a running cluster; "+
						"pass --force only if you mean to replace them", cfg.SecretFile)
				}
			}

			bundle, err := tal.GenSecrets(talosctl.GenSecretsOptions{
				TalosVersion:           cfg.TalosVersion,
				FromControlPlaneConfig: fromConfig,
				ExtraArgs:              extraFlags,
			})
			if err != nil {
				return err
			}

			if toStdout {
				_, err := cmd.OutOrStdout().Write(bundle)

				return err
			}

			out := bundle

			if !plaintext {
				if out, err = sopsx.EncryptTo(bundle, dest); err != nil {
					return err
				}
			}

			// --force replaces the one bundle a running cluster trusts: the
			// old one is kept, so a mistaken --from-controlplane-config can
			// be undone.
			if old, err := os.ReadFile(dest); err == nil && !toStdout {
				backup, err := keepBundle(cfg, old, "pre-generate")
				if err != nil {
					return err
				}

				fmt.Fprintf(cmd.OutOrStdout(), "kept the old bundle as %s\n", render.Rel(backup))
			}

			// Atomically: os.WriteFile truncates first, so an interrupted
			// write leaves a half-written bundle where the working one was --
			// and a cluster whose secrets bundle is gone cannot be rendered
			// for, upgraded or reached again.
			if err := render.WriteAtomic(dest, out); err != nil {
				return err
			}

			state := "SOPS-encrypted"
			if plaintext {
				state = "UNENCRYPTED -- do not commit it"
			}

			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%s)\n", cfg.SecretFile, state)

			return nil
		},
	}

	cmd.Flags().StringVar(&fromConfig, "from-controlplane-config", "",
		"extract the bundle from an existing control plane machine config instead of generating new material")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing secrets bundle")
	cmd.Flags().BoolVar(&plaintext, "plaintext", false, "write the bundle unencrypted")
	cmd.Flags().BoolVar(&toStdout, "stdout", false, "write the plaintext bundle to stdout instead of a file")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// keepBundle copies the bundle about to be replaced into the output directory
// as secrets-<why>-<UTC time>.yaml, and says where.
//
// Into the output directory, gitignored and 0700: a plaintext bundle's copy
// holds every key the cluster has, and beside the bundle it would be one
// `git add .` from a commit. An encrypted one is kept the same way, for
// symmetry and because it costs nothing.
func keepBundle(cfg *config.Config, old []byte, why string) (string, error) {
	if err := render.PrepareOutput(cfg, os.Stderr); err != nil {
		return "", err
	}

	backup := filepath.Join(cfg.OutputPath(),
		fmt.Sprintf("secrets-%s-%s.yaml", why, time.Now().UTC().Format("20060102T150405Z")))

	if exists(backup) {
		return "", fmt.Errorf("keeping the old bundle: %s already exists", render.Rel(backup))
	}

	if err := render.WriteAtomic(backup, old); err != nil {
		return "", fmt.Errorf("keeping the old bundle: %w", err)
	}

	return backup, nil
}
