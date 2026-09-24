package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// initTemplate is the talman.yaml `init` writes: the required keys filled in,
// and the optional ones present as comments to uncomment.
var initTemplate = template.Must(template.New("talman.yaml").Parse(`# yaml-language-server: $schema={{ .SchemaURL }}
apiVersion: {{ .APIVersion }}
clusterName: {{ .ClusterName }}
endpoint: {{ .Endpoint }}
talosVersion: {{ .TalosVersion }}
kubernetesVersion: {{ .KubernetesVersion }}

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
  - hostname: {{ .Hostname }}
    ipAddress: {{ .IPAddress }}
    role: {{ .Role }}
{{- end }}
`))

// initSopsTemplate is the .sops.yaml `init --age` writes: one rule, matching
// the secrets bundle and any encrypted patch.
var initSopsTemplate = template.Must(template.New(".sops.yaml").Parse(`creation_rules:
  - path_regex: \.sops\.yaml$
    age: {{ . }}
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
		Long: `Init writes a talman.yaml for a new cluster into the directory, the current one
by default, with the nodes named on the command line:

  talman init prod --controlplane cp-01=10.0.0.11 --worker w-01=10.0.0.21

The cluster is named after the directory unless --cluster-name says otherwise,
and the endpoint is the first control plane's address unless --endpoint names
a VIP or a load balancer, which a cluster of more than one control plane
wants. The Talos and Kubernetes versions default to the ones the talosctl on
PATH generates.

With --age it also writes a .sops.yaml that encrypts the secrets bundle, and
any patch named *.sops.yaml, to that age recipient.

It writes no secrets and touches no machine; "talman secrets generate" is next.
It refuses to overwrite a talman.yaml or a .sops.yaml that is already there.`,
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

			if clusterName == "" {
				clusterName = filepath.Base(abs)
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

					nodes = append(nodes, node{host, ip, string(list.role)})
				}
			}

			if len(controlPlanes) == 0 {
				return errors.New("name at least one control plane: --controlplane hostname=address")
			}

			if endpoint == "" {
				_, ip, _ := strings.Cut(controlPlanes[0], "=")
				endpoint = "https://" + net.JoinHostPort(ip, "6443")
			}

			tal := runnerFor(config.DefaultTalosctl)

			if talosVersion == "" {
				if talosVersion, err = tal.ClientVersion(); err != nil {
					return fmt.Errorf("finding the Talos version to target (or pass --talos-version): %w", err)
				}
			}

			if k8sVersion == "" {
				if k8sVersion, err = defaultKubernetesVersion(tal); err != nil {
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

			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}

			if err := os.WriteFile(path, body.Bytes(), 0o644); err != nil {
				return err
			}

			// Checked as any config is, so a hostname or an endpoint init
			// was handed is refused here rather than at the next command.
			if _, err := config.Load(path); err != nil {
				_ = os.Remove(path)

				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "wrote %s\n", render.Rel(path))

			if age != "" {
				var sops bytes.Buffer
				if err := initSopsTemplate.Execute(&sops, age); err != nil {
					return err
				}

				if err := os.WriteFile(sopsPath, sops.Bytes(), 0o644); err != nil {
					return err
				}

				fmt.Fprintf(out, "wrote %s\n", render.Rel(sopsPath))
			}

			fmt.Fprintln(out, "next:")

			if age == "" {
				fmt.Fprintln(out, "  add a .sops.yaml with a creation rule for secrets.sops.yaml (or pass --age)")
			}

			fmt.Fprintln(out, "  talman secrets generate   the cluster's CAs and keys, encrypted")
			fmt.Fprintln(out, "  talman render             then read what it wrote")

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

var k8sDefault = regexp.MustCompile(`--kubernetes-version string\s.*\(default "([^"]+)"\)`)

// defaultKubernetesVersion is the Kubernetes version `talosctl gen config`
// generates for when none is named. talosctl has no command that prints it,
// so it is read off the flag's help.
func defaultKubernetesVersion(tal *talosctl.Runner) (string, error) {
	help, err := tal.Output("gen", "config", "--help")
	if err != nil {
		return "", err
	}

	m := k8sDefault.FindSubmatch(help)
	if m == nil {
		return "", errors.New("talosctl gen config --help names no default Kubernetes version")
	}

	return string(m[1]), nil
}
