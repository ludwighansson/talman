package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v4"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/sopsx"
)

// initTemplate is the talman.yaml `init` writes: the required keys filled in,
// and the optional ones present as comments to uncomment.
var initTemplate = template.Must(template.New("talman.yaml").Funcs(template.FuncMap{"yaml": yamlScalar}).Parse(`# yaml-language-server: $schema={{ .SchemaURL }}
apiVersion: {{ .APIVersion }}
clusterName: {{ yaml .ClusterName }}
endpoint: {{ yaml .Endpoint }}
talosVersion: {{ yaml .TalosVersion }}
kubernetesVersion: {{ yaml .KubernetesVersion }}

# Free-form data for patch templates, as .Values.
# values:
#   registryMirror: harbor.example.net

# Where installer images come from. Uncomment for anything but bare metal.
# imageFactory:
#   platform: openstack

# Extensions and kernel arguments baked into the image.
# schematic:
#   customization:
#     systemExtensions:
#       officialExtensions:
#         - siderolabs/iscsi-tools

# Patch files by group, applied in this order: all, the node's role, its
# groups in the order it lists them, then its own patches.
patches:
  all: []
#   controlplane:
#     - ./patches/controlplane/vip.yaml
#   worker:
#     - ./patches/worker/kubelet.yaml

nodes:
{{- range .Nodes }}
  - hostname: {{ yaml .Hostname }}
    ipAddress: {{ yaml .IPAddress }}
    role: {{ yaml .Role }}
{{- end }}
`))

// initSopsTemplate is the .sops.yaml `init --age` writes: one rule, matching
// the secrets bundle and any encrypted patch.
var initSopsTemplate = template.Must(template.New(".sops.yaml").Funcs(template.FuncMap{"yaml": yamlScalar}).Parse(`creation_rules:
  - path_regex: \.sops\.yaml$
    age: {{ yaml . }}
`))

func newInitCmd() *cobra.Command {
	var (
		clusterName   string
		endpoint      string
		talosVersion  string
		k8sVersion    string
		controlPlanes []string
		workers       []string
		age           string
	)

	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Start a cluster directory",
		Long: `Init writes a talman.yaml for a new cluster, into the current directory or the
one named:

  talman init prod --controlplane cp-01=10.0.0.11 --worker w-01=10.0.0.21

--age also writes a .sops.yaml encrypting to that recipient. Versions default
to the local talosctl's. It writes no secrets and never overwrites a file;
"talman secrets generate" is next.

More in the README: "Layout", "Configuration".`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}

			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}

			// Checked before anything is written, so the error names what
			// the operator typed -- or, for a name taken from the directory,
			// says that is where it came from.
			if clusterName != "" {
				if err := config.CheckClusterName(clusterName); err != nil {
					return fmt.Errorf("--cluster-name: %w", err)
				}
			} else {
				clusterName = filepath.Base(abs)

				if err := config.CheckClusterName(clusterName); err != nil {
					return fmt.Errorf("%w\n  it is the name of the directory %q; pass --cluster-name to choose one",
						err, render.Rel(abs))
				}
			}

			type node struct{ Hostname, IPAddress, Role string }

			var nodes []node

			for _, list := range []struct {
				role  config.Role
				specs []string
			}{{config.RoleControlPlane, controlPlanes}, {config.RoleWorker, workers}} {
				for _, spec := range list.specs {
					host, ip, ok := strings.Cut(spec, "=")
					if !ok || host == "" || ip == "" {
						return fmt.Errorf("--%s %q must be hostname=address", list.role, spec)
					}

					// Here rather than when the written file is loaded, which
					// would name nodes[2] of a file the operator has not seen.
					for _, err := range []error{config.CheckHostname(host), config.CheckAddress(ip)} {
						if err != nil {
							return fmt.Errorf("--%s %q: %w", list.role, spec, err)
						}
					}

					nodes = append(nodes, node{host, ip, string(list.role)})
				}
			}

			if len(controlPlanes) == 0 {
				return errors.New("name at least one control plane: --controlplane hostname=address")
			}

			// One control plane's own address is the API for as long as that
			// machine is up, which with several of them is the outage they
			// were meant to survive. Said once the files are written.
			var endpointNote string

			if endpoint == "" {
				host, ip, _ := strings.Cut(controlPlanes[0], "=")
				endpoint = "https://" + net.JoinHostPort(ip, "6443")

				if len(controlPlanes) > 1 {
					endpointNote = fmt.Sprintf("note: endpoint is %s, %s's own address\n"+
						"  with %d control planes, point it at a VIP or a load balancer in talman.yaml",
						endpoint, host, len(controlPlanes))
				}
			}

			tal := runnerFor(config.DefaultTalosctl)

			// Both defaults are talosctl's answers. Without it, init still
			// writes a config when told the versions, and says so.
			if talosVersion == "" || k8sVersion == "" {
				if _, err := exec.LookPath(config.DefaultTalosctl); err != nil {
					return errors.New("talosctl is not on PATH, and init asks it which Talos and Kubernetes " +
						"versions to target: install it, or pass --talos-version and --kubernetes-version")
				}
			}

			if talosVersion == "" {
				if talosVersion, err = tal.ClientVersion(); err != nil {
					return fmt.Errorf("finding the Talos version to target (or pass --talos-version): %w", err)
				}
			}

			if !config.VersionPattern.MatchString(talosVersion) {
				return fmt.Errorf("%q is not a Talos version: pass --talos-version", talosVersion)
			}

			if k8sVersion == "" {
				if k8sVersion, err = tal.DefaultKubernetesVersion(); err != nil {
					return fmt.Errorf("finding the Kubernetes version to target (or pass --kubernetes-version): %w", err)
				}
			}

			var body bytes.Buffer

			if err := initTemplate.Execute(&body, map[string]any{
				"SchemaURL":         config.SchemaURL,
				"APIVersion":        config.APIVersion,
				"ClusterName":       clusterName,
				"Endpoint":          endpoint,
				"TalosVersion":      talosVersion,
				"KubernetesVersion": "v" + strings.TrimPrefix(k8sVersion, "v"),
				"Nodes":             nodes,
			}); err != nil {
				return err
			}

			path := filepath.Join(abs, config.DefaultFileName)
			sopsPath := filepath.Join(abs, ".sops.yaml")

			for _, p := range []string{path, sopsPath} {
				if p == sopsPath && age == "" {
					continue
				}

				if exists(p) {
					return fmt.Errorf("%s already exists; init does not overwrite it", render.Rel(p))
				}
			}

			var sops bytes.Buffer

			if age != "" {
				if err := initSopsTemplate.Execute(&sops, age); err != nil {
					return err
				}
			}

			// All or nothing: a half-written directory is one the next
			// init refuses to touch, so whatever this run made goes again
			// if any of it fails -- the directory too, if it made that.
			var written []string

			// Every directory MkdirAll is about to make, innermost first:
			// `init clusters/prod/eu` may make three.
			var created []string

			for d := abs; !exists(d); d = filepath.Dir(d) {
				created = append(created, d)
			}

			undo := func() {
				for _, p := range written {
					_ = os.Remove(p)
				}

				// os.Remove takes only an empty directory, so one someone
				// else wrote into meanwhile stays.
				for _, d := range created {
					_ = os.Remove(d)
				}
			}

			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}

			// Created exclusively: the check above is only a courtesy, and a
			// file that appeared since must be refused, not truncated.
			write := func(p string, b []byte) error {
				f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
				if err != nil {
					undo()

					if errors.Is(err, fs.ErrExist) {
						return fmt.Errorf("%s already exists; init does not overwrite it", render.Rel(p))
					}

					return err
				}

				written = append(written, p)

				if _, err := f.Write(b); err != nil {
					_ = f.Close()

					undo()

					return err
				}

				if err := f.Close(); err != nil {
					undo()

					return err
				}

				return nil
			}

			if err := write(path, body.Bytes()); err != nil {
				return err
			}

			// Checked as any config is, so a hostname or an endpoint init
			// was handed is refused here rather than at the next command.
			if _, err := config.Load(path); err != nil {
				undo()

				return err
			}

			if age != "" {
				if err := write(sopsPath, sops.Bytes()); err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()

			for _, p := range written {
				fmt.Fprintf(out, "wrote %s\n", render.Rel(p))
			}

			if endpointNote != "" {
				fmt.Fprintln(os.Stderr, endpointNote)
			}

			fmt.Fprintln(out, "next:")

			// Without --age the bundle is encrypted by whatever .sops.yaml
			// is found walking up from it, and with none there is nothing
			// to encrypt it to. init cannot be run again to add one -- it
			// does not overwrite talman.yaml -- so the rule to write is
			// spelled out instead.
			if _, err := sopsx.FindConfig(abs); age == "" && err != nil {
				fmt.Fprintf(out, "  write %s, so the bundle is encrypted; for an age key:\n", render.Rel(sopsPath))
				fmt.Fprintln(out, "      creation_rules:")
				fmt.Fprintln(out, `        - path_regex: \.sops\.yaml$`)
				fmt.Fprintln(out, "          age: <recipient, from age-keygen>")
			}

			// Naming the config written elsewhere, so the commands reach it.
			talman := "talman"
			if dir != "." {
				talman += " -c " + shellQuote(render.Rel(path))
			}

			for _, step := range [][2]string{
				{"secrets generate", "the cluster's CAs and keys, encrypted"},
				{"render", "then read what it wrote"},
				{"image url --kind iso", "boot media, for machines not running Talos yet"},
				{"apply --bootstrap", "build the cluster, once they are up in maintenance mode"},
			} {
				fmt.Fprintf(out, "  %s %-21s %s\n", talman, step[0], step[1])
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "the cluster's name (default: the directory's)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "",
		"the Kubernetes API URL (default: https://<first control plane>:6443)")
	cmd.Flags().StringVar(&talosVersion, "talos-version", "", "Talos version to target (default: talosctl's)")
	cmd.Flags().StringVar(&k8sVersion, "kubernetes-version", "",
		"Kubernetes version to target (default: the one talosctl generates)")
	cmd.Flags().StringArrayVar(&controlPlanes, "controlplane", nil, "a control plane, as hostname=address (repeatable)")
	cmd.Flags().StringArrayVar(&workers, "worker", nil, "a worker, as hostname=address (repeatable)")
	cmd.Flags().StringVar(&age, "age", "", "age recipient to encrypt secrets to, written to .sops.yaml")

	return cmd
}

// yamlScalar writes s as YAML does -- plain where that reads back the same,
// quoted where it would not -- so a value with a '#' or a ': ' in it lands in
// the file as it was given rather than cut short or unparseable.
func yamlScalar(s string) (string, error) {
	out, err := yaml.Marshal(s)
	if err != nil {
		return "", err
	}

	return strings.TrimSuffix(string(out), "\n"), nil
}
