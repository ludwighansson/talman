package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v4"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
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

			if !versionPattern.MatchString(talosVersion) {
				return fmt.Errorf("%q is not a Talos version: pass --talos-version", talosVersion)
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

var versionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

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
