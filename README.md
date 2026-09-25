# talman

Manage [Talos Linux](https://www.talos.dev/) clusters and, above all, their
configuration patches.

> [!IMPORTANT]
> talman drives real clusters. Read what `render` writes and what
> `apply --dry-run` reports before you `apply`, and pin a tagged release rather
> than tracking `main`. What a release promises to keep stable is under
> [Compatibility](#compatibility).

```console
$ talman patches -n development-worker-01
development-worker-01 (worker, groups: db, storage)
  1.  [all]     ../base/patches/all/00-install.yaml
  2.  [all]     ../base/patches/all/10-registry-mirrors.yaml
  3.  [all]     ../base/patches/all/20-disable-cni.yaml
  4.  [all]     ../base/patches/all/30-discovery.yaml
  5.  [worker]  ./patches/worker/kubelet.yaml
  6.  [db]      ./patches/db/hugepages.yaml
  7.  [node]    ./patches/nodes/worker-01.yaml

$ talman render
wrote clusterconfig/development-control-01.yaml
wrote clusterconfig/development-worker-01.yaml
...
wrote clusterconfig/talosconfig
```

## Why

[talhelper](https://github.com/budimanjojo/talhelper) was archived in August
2026. Its structural problem was that it embedded upstream `v1alpha1` structs in
its own schema and hand-wrote a generator for each new Talos document kind, so
every Talos minor release meant a talhelper release.

[topf](https://github.com/postfinance/topf) replaced it and got the important
thing right — Go templating in patches — but targets patches by hardcoded
directory name. The entire mechanism is three string constants: `all/`,
`<role>/` and `node/<host>/`. There is no way to say "these five nodes", and no
tier between "every node" and "one node".

talman keeps the templating, addresses patches **by explicit path**, and adds
**node groups**:

```yaml
patches:
  all:
    - ../base/patches/all/00-install.yaml
  controlplane:
    - ./patches/controlplane/virtual-ip.yaml
  worker:
    - ./patches/worker/kubelet.yaml
  db:
    - ./patches/db/hugepages.yaml
  storage:
    - ./patches/storage/nvme.yaml
```

`all`, `controlplane` and `worker` are reserved and assigned from each node's
role. Every other key must name a group some node declares, and every value
must resolve to a file — both are errors otherwise, so a renamed group cannot
silently orphan its patches and a typo'd path cannot silently apply nothing.

## No Talos dependency

talman does not link against the Talos API. It shells out to `talosctl`, which
means a new Talos release needs no talman release.

Rendering a node is one `talosctl gen config` call carrying that node's ordered
patch chain — the workflow Sidero
[documents](https://docs.siderolabs.com/talos/v1.14/configure-your-talos-cluster/system-configuration/reproducible-machine-configuration)
but ships no tool for. Talos does the merging, so talman needs no opinion about
what a machine config contains.

The one thing `talosctl` cannot do is compute an Image Factory schematic ID.
talman does that offline (sha256 of the schematic's canonical form); `--submit`
registers it with the factory instead.

## Install

```console
go install github.com/ludwighansson/talman/cmd/talman@v1.0.0
```

Prebuilt archives for Linux, macOS, Windows and FreeBSD are on the
[releases page](https://github.com/ludwighansson/talman/releases). Pinning a
version is still the better habit than `@latest`: a cluster directory is
reviewed against the talman that renders it, and an upgrade is worth making on
purpose.

Or take the image, which carries `talosctl` and `sops` with it:

```console
$ docker run --rm --read-only --tmpfs /tmp -v "$PWD:/cluster" \
    ghcr.io/ludwighansson/talman:1.0.0 validate
```

It holds exactly three binaries — talman and the two it drives — on Alpine,
and runs as uid 65532. Alpine rather than distroless for the shell: GitLab CI
runs a job's script through the image's shell and GitHub Actions runs `run:`
steps the same way, so without one the image can only be used as `docker run`
rather than as the job image itself.

```yaml
# .gitlab-ci.yml
drift:
  image:
    name: ghcr.io/ludwighansson/talman:1.0.0
    entrypoint: [""]   # the image's entrypoint is talman; GitLab runs the script with a shell
  script:
    - talman apply --dry-run --detailed-exit-code
```

`--tmpfs /tmp` is not optional when running read-only — rendering decrypts the
secrets bundle into a temp directory, and a tmpfs is what keeps the plaintext
off a disk. The cluster directory is mounted at `/cluster`, which is also the
working directory, and must be writable by uid 65532 for `render` to write its
output.

The talosctl version inside is the one your cluster meets, so it is on the
image as a label as well as in `talman version`:

```console
$ docker inspect --format '{{ index .Config.Labels "dev.talman.talosctl.version" }}' \
    ghcr.io/ludwighansson/talman:1.0.0
v1.14.1
```

That pinning is the one cost of the image: talman's own promise is that a new
Talos release needs no talman release, and an image ties you to the talosctl it
shipped with. `talosctl:` in talman.yaml can point at a newer binary mounted
into the container when that matters.

Every release is signed, keylessly, by the workflow that built it, and each
archive ships a bill of materials beside it:

```console
$ cosign verify-blob checksums.txt \
    --signature checksums.txt.sig --certificate checksums.txt.pem \
    --certificate-identity-regexp 'https://github.com/ludwighansson/talman/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
$ cosign verify ghcr.io/ludwighansson/talman:1.0.0 \
    --certificate-identity-regexp 'https://github.com/ludwighansson/talman/.*' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The checksum file is what is signed, and it covers every archive. There is no
private key to trust or lose: the signature is bound to the workflow identity
that produced it and recorded in a public transparency log.

You also need [`talosctl`](https://docs.siderolabs.com/talos/v1.14/talosctl) on
`PATH`, at least as new as the Talos version you target — and no older than
v1.14.0, which talman checks before it reaches a cluster. It relies on flags
that arrived over time (`version --insecure`, `get services`,
`reset --wipe-labels`), and being told that up front beats an operation
stopping halfway through on an unknown flag. You also need
[`sops`](https://github.com/getsops/sops) if your secrets bundle or any patch
is encrypted.

talman shells out to both rather than linking them in. Embedding the SOPS
library pulled the AWS, GCP and Azure KMS SDKs along with it — several hundred
modules to support key services most clusters never use, every one of them
talman's to keep patched.

## Layout

```text
base/patches/all/…        # shared across clusters
development/
  talman.yaml
  secrets.sops.yaml       # age-encrypted; the only secret in the repo
  patches/…
  clusterconfig/          # rendered output, gitignored
```

Relative patch paths resolve against the directory holding `talman.yaml`, so
`../base/patches/…` works and shared patches need no duplication.

See [`example/`](example/) for a working tree, or start one:

```console
$ talman init prod --controlplane cp-01=10.0.0.11 --worker w-01=10.0.0.21 \
    --endpoint https://10.0.0.10:6443 --age age1…
wrote prod/talman.yaml
wrote prod/.sops.yaml
```

The Talos and Kubernetes versions default to what the `talosctl` on `PATH`
generates, and the endpoint to the first control plane's address — name a VIP
or a load balancer with `--endpoint` for more than one. `init` writes no
secrets, and never overwrites a file.

## Configuration

`apiVersion` names the schema the file is written against, and is required. It
is what lets a later, incompatible schema be told apart from this one instead
of misread: a talman that meets a schema it does not speak says so, rather than
reporting whichever key it did not recognise first.

```yaml
apiVersion: talman.dev/v1       # the schema this file is written against
clusterName: development
endpoint: https://10.0.0.10:6443
talosVersion: v1.14.0            # required: pinning it is what makes renders reproducible
kubernetesVersion: v1.37.0

values:                          # free-form, available to templates as .Values
  harbor: harbor.example.net

imageFactory:
  platform: openstack            # the installer and boot media are openstack's

schematic:                       # inline, or a path to a (templated) file
  customization:
    systemExtensions:
      officialExtensions:
        - siderolabs/drbd

patches:
  all:
    - ../base/patches/all/00-install.yaml
  db:
    - ./patches/db/hugepages.yaml

nodes:
  - hostname: development-worker-01
    ipAddress: 10.0.0.21
    role: worker                 # controlplane | worker
    groups:
      - db
      - storage
    values:                      # free-form, available as .Node.Values
      installDisk: /dev/vda
      zone: az1
    patches:
      - ./patches/nodes/worker-01.yaml
```

Optional top-level keys: `talosctl` (binary path), `outputDir`
(`clusterconfig`), `secretFile` (`secrets.sops.yaml`), `validationMode`,
`schematicID`, `valuesFiles`. Per-node: `talosVersion`, `schematic`,
`schematicID`, `imageFactory`, `valuesFiles`.

`valuesFiles` lists YAML files merged into `values`, for values several
clusters share:

```yaml
valuesFiles:
  - ../base/values.yaml       # the defaults
  - ./values-site.yaml        # this site's, over them
values:
  registry:
    mirror: harbor.sto1.example.net   # and this cluster's, over both
```

Files merge in the order listed and the inline `values` go on top. Maps merge
key by key, all the way down; anything else, a list included, is replaced
whole. A node's own `valuesFiles` build its `.Node.Values` the same way. They
are read as plain YAML: a secret belongs in an encrypted patch, which
`validate` can check without the key.

`schematic` and `schematicID` are two ways of saying the same thing, so set at
most one of them at each level. A node's own setting wins over the cluster's.

A `hostname` is an RFC 1123 host name, lower case: it is what Kubernetes will
call the node, and the name of the file its config is rendered to. `endpoint`
is an `https://` URL with a port.

`validationMode` is the `--mode` rendered configs are checked against with
`talosctl validate`. It follows the node's image platform by default, `metal`
for `metal` and `cloud` for everything else, so it only needs setting to
`container` for a cluster of containers.

Unknown keys are an error. In a config whose job is to route patch files, a
mistyped key that got ignored would mean a machine config quietly missing a
patch.

For completion and checking as you type, point your editor at the published
[JSON schema](schema/talman.v1.json). With the YAML language server — VS Code's
YAML extension, Neovim, Helix, Zed — that is one line at the top of the file:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/ludwighansson/talman/main/schema/talman.v1.json
```

The schema is generated from the types talman decodes into, and a test fails
when the two disagree.

### The Image Factory

`imageFactory` says where installer images come from and how their references
are spelled. Every key is optional:

| key | default | |
| --- | --- | --- |
| `registryURL` | `factory.talos.dev` | the factory host, and the registry the installer is pulled from |
| `protocol` | `https` | for submitting schematics with `--submit` |
| `schematicEndpoint` | `/schematics` | ditto |
| `platform` | `metal` | the installer's platform: `metal`, `openstack`, `aws`, … |
| `secureBoot` | `false` | use the secure boot installer |
| `installerURLTmpl` | `{{.RegistryURL}}/{{.Platform}}-installer{{if .SecureBoot}}-secureboot{{end}}/{{.ID}}:{{.Version}}` | the installer reference, as a Go template |

A node can set any of them under its own `imageFactory`, which overrides the
cluster's key by key. That is how one cluster mixes machines that boot
different platforms' images:

```yaml
imageFactory:
  platform: openstack
nodes:
  - hostname: gpu-01
    # ipAddress, role, …
    imageFactory:
      platform: metal
```

The same schematic and version name the boot media for a machine that has no
Talos on it yet, from the same factory:

```console
$ talman image url --kind iso -n talos-w01
talos-w01  https://factory.talos.dev/image/0791…/v1.14.0/metal-amd64.iso
$ talman image url --kind pxe --arch arm64
$ talman image url --kind disk -n talos-w01          # metal-amd64.raw.zst
```

`disk` uses the format each platform is published in (`raw.zst` for metal,
`ova` for vmware, …), and `--format` names another, or one for a platform
talman has no default for.

Schematic IDs are computed offline, as the sha256 of the schematic's canonical
form. That is the factory's own algorithm, and a test holds talman's output to
IDs the factory's code computed. `talman schematic id --submit` registers the
schematic instead, and warns if the factory's answer ever differs.

### The node schema is deliberately tiny

A node carries only what talman needs to route patches and reach the host.
There is no `installDisk`, no `nodeLabels`, no `networkInterfaces` — those are
Talos' shapes, and mirroring them is what made talhelper expensive. Put them in
a patch and template them from `values`:

```yaml
# patches/all/00-install.yaml
apiVersion: v1alpha1
kind: UnattendedInstallConfig
installer:
  image: {{ .Node.InstallerImage }}
provisioning:
  diskSelector:
    match: disk.dev_path == "{{ .Node.Values.installDisk }}"
  wipe: false
---
apiVersion: v1alpha1
kind: KubeNodeConfig
labels:
  topology.kubernetes.io/zone: {{ .Node.Values.zone }}
```

## Patches

Every referenced patch file is a Go template
([sprig](https://masterminds.github.io/sprig/) included) and may be
SOPS-encrypted. There is no opt-in suffix: topf gates templating on `.tpl`,
which means a templated patch can never also be encrypted.

Template scope:

| | |
| --- | --- |
| `.Cluster` | `.Name` `.Endpoint` `.TalosVersion` `.KubernetesVersion` |
| `.Node` | `.Hostname` `.IPAddress` `.Role` `.Groups` `.Values.<key>` `.TalosVersion` `.SchematicID` `.InstallerImage` |
| `.Values` | the cluster-wide `values:` map; the per-node one is `.Node.Values` |

`.Node.TalosVersion` is the *resolved* version and `.Node.HasGroup "db"` reads
better than sprig's `has`. Missing map keys are an error, not `<no value>` — a
typo must not become a subtly wrong machine config.

**Order matters.** Talos applies strategic merge patches in sequence, last
writer wins:

```text
all  ->  <role>  ->  each of nodes[].groups in the node's order  ->  nodes[].patches
```

`talman patches` prints exactly that, which is the authoritative answer to "why
does this node have that value".

Only strategic merge patches are supported. RFC6902 JSON patches cannot be
applied to a multi-document config, and Talos has emitted multi-document
configs since v1.12 — talman detects an op/path list and says so, rather than
letting `talosctl` report an error naming no file. Use `$patch: delete` to
remove a field or a whole document.

### When a patch is rejected

`talosctl` names the offending *document*, never the file. On failure talman
re-runs the generation with growing prefixes of the chain and reports the
culprit:

```console
$ talman render
error: node development-worker-01: talosctl gen config: error decoding document
v1alpha1/SysctlConfig/ (line 1): unknown keys found during decoding:
sysctls:
    vm.nr_hugepages: "1024"
  rejected by: patches.db -> ./patches/db/hugepages.yaml
  (it is the 6th patch in the chain `talman patches` lists; the ones before it applied cleanly)
```

Growing prefixes rather than testing each patch alone is deliberate: several
Talos v1.14 document pairs are *mutually exclusive* — `.machine.install` and
`UnattendedInstallConfig`, `.cluster.discovery` and `DiscoveryServiceConfig`,
`.machine.features.imageCache` and `ImageCacheConfig` — so a patch can be
perfectly valid alone and still be the one that makes the config unacceptable.

## Secrets

```console
$ talman secrets generate     # -> secrets.sops.yaml, encrypted per .sops.yaml
```

The bundle is generated with `talosctl gen secrets` and encrypted by invoking
`sops`, so `SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE` and `.sops.yaml`
`creation_rules` behave exactly as they do on the command line — including PGP
and cloud KMS keys.

One difference from calling `sops` yourself: talman resolves `.sops.yaml` by
walking up from the secrets file, not from your working directory, so where you
happen to be standing cannot change whether a bundle gets encrypted.

`render` decrypts it into a private temporary directory for the duration of the
run and removes it afterwards. Plaintext secrets never reach `clusterconfig/`
or the repository.

Generating refuses to overwrite an existing bundle: the CAs and cluster
identity in it are what a running cluster trusts. To adopt a cluster that
already exists, use `--from-controlplane-config`. `--force` replaces the
bundle anyway, and keeps the old one in the gitignored output directory as
`secrets-pre-generate-<time>.yaml`, so a mistake can be undone.

Rendered machine configs *do* contain secrets, so `render` writes them `0600`
and drops a `.gitignore` that excludes the whole output directory. That is why
`outputDir` has to be a directory of its own: one that holds `talman.yaml` is
refused, and a `.gitignore` already there that talman did not write is left
alone and the render stops, rather than being replaced.

## Commands

| Command | |
| --- | --- |
| `talman init [dir]` | start a cluster directory: `talman.yaml`, and `.sops.yaml` with `--age` |
| `talman validate` | check keys, paths and templates; needs no secrets, keys or network |
| `talman patches` | the resolved patch chain per node, in application order |
| `talman status` | the node table, with what each one is running (`--offline` asks nothing) |
| `talman secrets generate` | create the encrypted secrets bundle |
| `talman render` | write machine configs and a talosconfig |
| `talman schematic id` | the resolved schematic ID per node |
| `talman image url` | the installer image per node, or with `--kind` its ISO, disk image or iPXE script |
| `talman apply` | re-render, then apply; `--onboard-new-nodes` also configures new nodes, `--bootstrap` builds a new cluster |
| `talman kubeconfig` | fetch the kubeconfig into the output directory |
| `talman upgrade` | upgrade Talos to each node's configured installer image |
| `talman reboot` | a rolling reboot, one node at a time |
| `talman upgrade-k8s` | upgrade Kubernetes to `kubernetesVersion` (a noop when every node is already there) |
| `talman health` | cluster health |
| `talman etcd snapshot [path]` | save an etcd snapshot, by default into the output directory |
| `talman talosctl -n <node> …` | any other talosctl command, with the talosconfig and node addresses filled in |
| `talman rotate-ca` | rotate the Talos and Kubernetes API CAs, and the secrets bundle with them |
| `talman reset` | wipe nodes (requires typing the cluster name) |
| `talman version` | the talman and talosctl versions |

`-n/--node` restricts a command to named nodes, by hostname or IP. It is
repeatable wherever a command works node by node — `apply`, `upgrade`,
`reboot`, `reset`, `render`, `status`, `patches`, `schematic id`, `image url`,
`ctl` — and names the one control plane to run from on `health`,
`kubeconfig` and `upgrade-k8s`, which act on the cluster as a whole. Wherever
`-n` is repeatable, `-g/--group` selects by group or role as well:
`talman reboot -g blue` is every node in `blue`, and `-n` and `-g` together
select both. `-v` echoes every
`talosctl` invocation. `-c` points at a config other than `./talman.yaml`, and
so does `TALMAN_CONFIG` when `-c` is not given.

`status`, `patches`, `schematic id`, `image url` and `version` take
`-o json`. The text they print is for reading and may change to read better;
the JSON is for scripts, and keeps its shape.

```console
$ talman status -o json | jq -r '.nodes[] | select(.talos.running and .talos.running != .talos.configured) | .hostname'
```

Shell completion comes from `talman completion bash|zsh|fish|powershell`, and
completes node names for `-n` from the config in the working directory:

```console
$ source <(talman completion zsh)
```

Ctrl-C, a CI runner's SIGTERM, or a SIGHUP from a terminal closing or an ssh
session dropping stops a run in order rather than on the spot. The talosctl process in flight is signalled too, the decrypted secrets
are removed, metrics are written and a stopped roll-out names the nodes it did
not reach, as it does for any failure; talman then exits 128 plus the signal,
as a shell would report it: 130 for Ctrl-C, 143 for SIGTERM, 129 for SIGHUP. A
second signal exits at once, killing any talosctl still running and still
removing the decrypted secrets.

### Onboarding new nodes

A node that has never been configured — or that has just been `reset` — answers
only the maintenance service, while nodes already in the cluster answer with
cluster PKI. `apply` asks each node which it is, immediately before sending its
config.

The maintenance service authenticates nothing. Whatever answers at a node's
address gets its whole config, and a control plane's includes the cluster's CA
keys — so a reused address, a spoofed host, or a real node whose secure API
failed for a moment would be handed them. `apply` therefore sends a config
that way only when asked to onboard it:

```console
$ talman apply
== [1/2] talos-c01 (10.164.0.27)
     Applied configuration without a reboot
error: talos-w01 (10.164.0.32) is a new node: it answers only the maintenance service, which authenticates nothing, so it gets its config, CA keys included, only when asked
  talman apply --onboard-new-nodes -n talos-w01
$ talman apply --onboard-new-nodes -n talos-w01
== [1/1] talos-w01 (10.164.0.32) · onboarding
     Applied configuration without a reboot
```

Each node gets a heading with its place in the run, and what `talosctl` said
underneath it, indented as the detail it is. Anything talman decided not to do
— waiting, health checking — is said once at the end rather than under every
node, because the reason does not change between them.

Every question talman asks *about* a node — which API it answers, whether it
has come back after an apply, what it is running — is asked of that node first,
with `--endpoints` pinned to it, and through the talosconfig's endpoints second
if that fails.

Both routes are needed, which is why neither is a setting. Pinning is the only
way to reach a node in maintenance mode, or any node while the control planes
that would proxy for it are down — a cluster being built, or torn down. Going
through the endpoints is the only way to reach a node whose API is not exposed
outside the cluster network, which is how a worker is commonly firewalled:
talman applies its config through a control plane, so it has to be able to ask
after it the same way. Whichever route answered is remembered for the rest of
the run.

A new cluster is built with one command:

```console
$ talman apply --bootstrap
== [1/6] talos-c01 (10.164.0.27) · onboarding
     Applied configuration without a reboot
     waiting for 10.164.0.27 to come back, up to 10m0s
     ...
== bootstrapping etcd on talos-c01 (10.164.0.27)
     etcd is running on talos-c01 after 38s
== [2/6] talos-c02 (10.164.0.28) · onboarding
...
```

It configures the first control plane, bootstraps etcd on it and waits until
etcd runs, and only then configures the rest — which is the order that works:
a node given its config before there is a cluster to join cannot finish, and
waiting for it just times out. From the second node on it is an ordinary
apply, with its waits, its health gate, `--parallel` and the config's waves.

Before it touches anything, it checks there is nothing to bootstrap yet:
every control plane has to answer and none may run etcd, since bootstrapping
twice splits a cluster. Control planes in maintenance mode are certainly new.
Ones that are configured but run no etcd are either a cluster an earlier run
half built or one whose etcd is broken — talman cannot tell which, and a
bootstrap sent to a broken one stops its etcd — so it asks for the cluster
name first (`-y` skips the question). It builds the whole cluster, so it takes
no `-n`, `-g` or wave flags, and no `--wait=false`. `--bootstrap --dry-run`
prints the plan — which node is bootstrapped, the order the rest follow in,
which nodes are new — and sends nothing.

A plain `apply` on a cluster whose control planes are all new stops and says
to use `--bootstrap`. One whose control planes are configured but run no etcd
gets a warning and goes ahead, since that is also what a broken cluster looks
like, and refusing would stand in the way of the apply that fixes it.

Adding a machine to a live cluster is `talman apply --onboard-new-nodes -n
<new node>`, and bringing one back after `reset` is the same command.
`--only-new-nodes` does the
whole lot at once: it restricts the run to the nodes currently in maintenance
mode, and onboards them.

```console
$ talman apply --only-new-nodes
   talos-c01 (10.164.0.27) is running; not new, skipping
== [1/1] talos-w01 (10.164.0.32) · onboarding
```

`talman status` is the view of the same question, and of every other question
about what is actually out there — see below.

`-i` forces the maintenance service for every node and `--insecure=false`
forces cluster PKI, for when the answer is known better than the probe can
tell.

A node that answers neither API stops the run and names what is left, since
that is a fault rather than a state:

```console
error: talos-w02 (10.164.0.29) answers neither the Talos API nor the maintenance service
  2 node(s) were not applied: talos-w02, talos-w03
  continue with: talman apply -n talos-w02 -n talos-w03
```

With `--health`, the gate stands down while any node in the config is still
outside the cluster, said once for the run — `-v` names them:

```console
not gating on health: nodes not in the cluster yet
```

The check covers the cluster the config describes, so during a build-out it
checks machines that have not joined yet — and a node not onboarded yet answers with a
self-signed maintenance certificate, which `talosctl` reports as `certificate
signed by unknown authority`. That is a build-out step, not a broken cluster.
Once every node has joined, the gate runs between nodes as it should, which is
the roll-out it exists for.

### Seeing what is out there

`talman status` lists the nodes and asks each one what it is:

```console
$ talman status
HOSTNAME    ADDRESS       ROLE           STATUS             TALOS               KUBERNETES          GROUPS   PATCHES
talos-c01   10.164.0.27   controlplane   running            v1.14.0             v1.37.0             -        5
talos-c03   10.164.0.30   controlplane   running            v1.13.5 → v1.14.0   v1.36.2 → v1.37.0   -        5
talos-w01   10.164.0.32   worker         maintenance mode   -                   -                   db       6
talos-w02   10.164.0.29   worker         unreachable        -                   -                   -        4
4 node(s): 2 running, 1 in maintenance mode, 1 unreachable; 1 not on the configured version
```

An arrow is drift: what is running on the left, what the config resolves to on
the right, which is what `upgrade` and `upgrade-k8s` would close. A `-` is
something talman could not read — never the configured value dressed up as a
live one.

`unreachable` reads the same for a machine that is gone and for one that simply
has nowhere to report to, so when nodes are quiet and the control planes answer
that they have no etcd running, `status` says which it is — answer, not
silence: control planes that cannot be reached have established nothing, and a
cluster that is merely degraded is still a cluster, which is precisely when
bootstrapping again would be the wrong advice:

```console
6 node(s): 3 running, 3 unreachable
cluster: not bootstrapped — a node with a config but no cluster to join stays quiet; run `talman apply --bootstrap`
```

Nodes are asked in parallel, because the report is most wanted when
something is down, and a machine that is down takes two dial timeouts to admit
it.

`--offline` asks nothing at all. The table keeps its shape — same columns, same
order — with a dash wherever the answer could only have come from a node, and
the config's own answers still in place:

```console
$ talman status --offline
HOSTNAME    ADDRESS       ROLE           STATUS   TALOS     KUBERNETES   GROUPS   PATCHES
talos-c01   10.164.0.27   controlplane   -        v1.14.1   v1.37.0      -        5
talos-w01   10.164.0.32   worker         -        v1.14.1   v1.37.0      db       4
6 node(s)
```

It needs no cluster, no secrets bundle and no talosconfig, which is what makes
it the way to read a config while writing one — and the way to see the table
when the machines are off. `--wide` adds the schematic column.

`status` reports and always succeeds; `health` is the one that passes or fails.

### Everything else talosctl does

`talman talosctl` (or `talman ctl`) runs any talosctl command with this
cluster's talosconfig, and with `--nodes` set from the nodes named with `-n`:

```console
$ talman talosctl -n talos-w01 logs kubelet -f
$ talman ctl -n talos-c01 -n talos-c02 etcd members
$ talman ctl -- service
```

talman's own `-n`, `-g`, `-c` and `-v` come first; everything from the first argument
that is not one of them is talosctl's, its own flags included — `talman ctl -e
10.0.0.2 version` reaches talosctl whole. `--` is accepted and not needed. Without `-n`,
talosctl uses the talosconfig's default nodes, which are every node in the
config. talosctl's exit status is talman's.

### The kubeconfig

`talman kubeconfig` writes to `clusterconfig/kubeconfig`, beside the machine
configs and the talosconfig:

```console
$ talman kubeconfig
wrote clusterconfig/kubeconfig
  KUBECONFIG=clusterconfig/kubeconfig kubectl get nodes
```

It never touches `~/.kube/config` unless you name it. `talosctl kubeconfig`
left to itself merges into whatever kubeconfig the environment points at, which
for a tool that manages several clusters means one cluster's admin credentials
landing in another cluster's file — and the output directory is the gitignored
place a credential belongs.

Name a destination to override it, and `-` for stdout:

```console
$ talman kubeconfig ~/.kube/config      # merged, as talosctl would
$ talman kubeconfig - | kubectl --kubeconfig /dev/stdin get nodes
```

A path you name is merged into; talman's own copy is overwritten, because it is
a generated artefact of the cluster directory and merging the same context into
it twice would fail without `--force`. `--merge` set explicitly wins either way.

### The talosconfig

Every command that reaches the cluster — `apply`, `kubeconfig`,
`upgrade`, `upgrade-k8s`, `reboot`, `health`, `reset`, `rotate-ca`,
`etcd snapshot`, `status` and `ctl` — authenticates with
`clusterconfig/talosconfig`, and generates it when it is not there:

```console
$ talman kubeconfig
no talosconfig at clusterconfig/talosconfig; generating one from secrets.sops.yaml
wrote clusterconfig/talosconfig
```

The output directory is gitignored, so a freshly cloned cluster directory has
no talosconfig in it — and the file is derived entirely from the secrets bundle
and the node list, without touching the cluster. Demanding a `render` first was
asking for a command talman can run itself.

An existing talosconfig is never overwritten this way; `render` is the command
that rewrites it. Nothing in talman reads a kubeconfig — `kubeconfig` fetches
one over the Talos API for `kubectl`'s benefit, not talman's.

### Parallelism

Everything talman does per node is a talosctl process, and on a large cluster
one at a time takes as long as it sounds. `-p/--parallel` sets how many nodes
are worked on at once:

| command | default | why |
| --- | --- | --- |
| `render`, `status` | 8 | nothing is enacted; the work is local, or a question |
| `apply`, `upgrade`, `reboot`, `reset` | 1 | one node at a time is the unit of risk |

A run that enacts nothing — `apply --dry-run` or `--mode=staged` — batches
every node together, control planes included, and does so without being asked:
there are no reboots to stagger, so there is nothing to keep apart.

For the ones that change a cluster the flag raises the limit for **workers
only**. A control plane always goes alone, whatever the number says: two
rebooting together is how a three-node control plane loses quorum, and the
reason to reach for `--parallel` is a hundred workers rather than a shortcut
through etcd. Config order is kept — a run of workers batches up to the limit,
and a control plane interrupts the run:

```console
$ talman apply --parallel 4
== talos-c01 (10.164.0.27)      # alone
   checking cluster health before continuing
== talos-w01 (10.164.0.32)      # these four together
== talos-w02 (10.164.0.29)
== talos-w03 (10.164.0.31)
== talos-w04 (10.164.0.33)
   checking cluster health before continuing
```

The health check and the wait for a node to come back run per batch rather than
per node, so a batch is also the unit the roll-out stops at. With more than one
node in flight each node's output is captured and printed whole, because eight
talosctl processes sharing a terminal interleave into something no one can
attribute to a machine.

### Exit codes for CI

`apply`, `upgrade` and `upgrade-k8s` take `--detailed-exit-code`, which answers
"did anything change?" without anyone parsing output:

| code | meaning |
| --- | --- |
| 0 | nothing changed, or would have |
| 2 | something changed, or would have |
| 1 | the command failed, a usage error included |
| 129, 130, 131, 143 | the run was interrupted, by SIGHUP, SIGINT, SIGQUIT or SIGTERM |

Without the flag, a change is a 0 like anything else that succeeded. Test the
code rather than chaining `||` and `&&`, which reads 0 as drift too:

```console
$ talman apply --dry-run --detailed-exit-code; rc=$?
$ [ "$rc" -eq 2 ] && echo "drift"
```

`--diff` prints that answer instead of reducing it to a code — each node is
asked what would change, it is printed under the node's heading, and then the
config is sent:

```console
$ talman apply --diff
== [1/2] talos-c01 (10.164.0.27)
     Config diff: No changes.
     Applied configuration without a reboot
== [2/2] talos-w01 (10.164.0.32)
     Config diff:
     -  hostname: old
     +  hostname: new
     Applied configuration without a reboot
```

`--dry-run` stops after the asking, so it prints the diff by itself. However
many of these flags are passed, the node is asked once.

A diff is a diff of the machine configuration, so it carries what that carries:
join tokens, the cluster secret, the machine CA. This cluster's own secrets are
replaced before anything is printed —

```console
+    token: [redacted]
+        crt: [redacted]
+  id: [redacted]
     hostname: talos-w01
```

— by value rather than by guessing which fields are sensitive. talman decrypted
those values and rendered them into the config it is sending, so it knows
exactly what to look for, wherever it appears: the secrets bundle, in both the
base64 form it stores and the PEM blocks some of it is written out as; the
values SOPS encrypted in any patch, in every document of it; and anything of
six characters or more a patch read from the environment with `env` or
`expandenv` — except a short plain word like `staging`, or a value
`talman.yaml` itself spells out, like a version, which are plainly not
secrets and would otherwise be blanked everywhere they appear. Each is also found in its base64 forms, since a template that
writes a secret into an inline manifest usually pipes it through `b64enc`.
The same goes for a rejection: when talosctl refuses a config and quotes the
offending document, talman takes the secrets out of that too.
`--redact-secrets=false` prints diffs as they are.

Two things follow from being exact. Secrets talman has never seen cannot be
found this way — a node still holding a previous cluster's keys would show them
— and when talman cannot read them, it does not print the diff. `--diff` fails
before any config is sent. `--dry-run` prints `changes; diff not shown` or `no
changes` for each node and still sets the exit code, so a drift check with only
a talosconfig keeps working.

For `apply` the answer comes from Talos itself: each node computes the diff and
reports `Config diff: No changes.` when there is none. A real apply asks for
that diff before sending the config, so the exit code means the same thing
whether or not `--dry-run` was passed. Output talman cannot read counts as a
change — for a gate that decides whether something happened, "cannot tell" has
to mean "assume it did".

`upgrade` reports whether any node was upgraded rather than skipped as already
running its configured version and schematic, and `upgrade-k8s` whether the
upgrade ran or the cluster was already on the target. Both already made that
decision; the flag only surfaces it.

### Metrics for CI

`apply`, `upgrade`, `upgrade-k8s`, `reboot`, `reset`, `rotate-ca`, `health`
and `etcd snapshot` can record what a run did, for the whole run and
for each node, in the Prometheus text format. They write it to a file, push it to a metrics push endpoint, or both:

```console
$ talman upgrade --metrics-file talman.prom --metrics-url https://metrics.example.com \
    --metrics-label env=prod --metrics-label pipeline=nightly
```

| flag | environment | |
| --- | --- | --- |
| `--metrics-file <path>` | `TALMAN_METRICS_FILE` | write the metrics here, replacing the file in one step |
| `--metrics-url <url>` | `TALMAN_METRICS_URL` | push them to this endpoint |
| `--metrics-label k=v` | `TALMAN_METRICS_LABELS` (`k=v,k=v`) | add a label to every metric; repeatable |
| | `TALMAN_METRICS_TOKEN` | sent as a bearer token with the push |

The environment is there so a CI job can set them once for every step.
Metrics are recorded whether the run succeeds or fails. A file that cannot be
written or an endpoint that cannot be reached is a warning, and never changes
the exit code.

```text
talman_run_success{cluster,command}                   1 or 0
talman_run_changed{cluster,command}                   1 or 0, when talman knows
talman_run_duration_seconds{cluster,command}
talman_run_timestamp_seconds{cluster,command}         when the run ended
talman_run_last_success_timestamp_seconds{cluster,command}
talman_run_nodes{cluster,command,result}              how many nodes ended in each result
talman_run_info{cluster,command,talman_version}       1
talman_node_result{cluster,command,node,role,result}  1, one series per node
talman_node_success{cluster,command,node,role}        1 or 0, for nodes the run reached
talman_node_duration_seconds{cluster,command,node,role}
talman_node_info{cluster,command,node,role,from_version,to_version}  1, upgrade only
```

A node's result is `changed`, `unchanged`, `done` (it succeeded, but whether
it changed is not known), `failed`, or `not_reached` (the run stopped before
it got there). With metrics on, `apply` asks each node what would change before
changing it, the same way `--detailed-exit-code` does, so its nodes report
`changed` or `unchanged` rather than `done`. `apply --dry-run`,
`upgrade --dry-run` and `upgrade-k8s --dry-run` add `dry_run="true"`, so a
drift check and a roll-out of the same cluster are kept apart.

A push goes to `<url>/metrics/job/talman/cluster/<cluster>/command/<command>`,
followed by the extra labels. Each cluster, command and label set is its own
group, so an `apply` never replaces an `upgrade`'s metrics. Pushes are POSTs:
a failed run sends no last-success timestamp, so the one from the last run that
worked stays in place. The file does the same thing by carrying that timestamp
over from the file it replaces.

Some alerts this is meant for:

```yaml
- alert: TalmanRunFailed
  expr: talman_run_success == 0
- alert: TalmanNodeFailed
  expr: talman_node_success == 0
- alert: TalmanDrift                      # a nightly `apply --dry-run`
  expr: talman_run_changed{dry_run="true"} == 1
- alert: TalmanNotSucceededInADay         # also catches a job killed before it could report
  expr: time() - talman_run_last_success_timestamp_seconds > 86400
```

### Reaching talosctl flags talman does not model

Every command that shells out takes `--extra-flags`, appended verbatim to the
underlying invocation:

```console
$ talman render --extra-flags=--with-docs=true
$ talman apply --extra-flags=--timeout=5m
$ talman upgrade --extra-flags=--reboot-mode=powercycle
$ talman reset --extra-flags=--user-disks-to-wipe=/dev/sdb
```

It is repeatable rather than one string talman splits: values contain commas,
spaces and quotes, and splitting them here would mangle exactly the arguments
most worth passing through.

Flags land last on the command line. A flag that takes one value overrides
what talman set, which is the point, and also the caveat:
`--extra-flags=--output=/tmp/x` on `render` sends a machine config somewhere
talman then cannot find. A flag that takes a list is added to talman's instead:
`reset --extra-flags=--system-labels-to-wipe=STATE` wipes STATE on top of the
EPHEMERAL and STATE talman already asked for, rather than instead of them. To
change something talman sets, use talman's own flag for it, here
`--wipe-labels`. Use `-v` to see the full command.

`validate`, `patches`, `schematic id`, `image url` and `init` have no such
flag: none of them drives a `talosctl` operation. `ctl` has no need of one,
since everything after its own flags goes to talosctl already. Neither does
`status`, for the opposite
reason — it composes several calls per node rather than driving one, so there
is no single invocation for a forwarded flag to land on.

`render` validates every generated config with `talosctl validate` before
writing; `--no-validate` skips it, `--dry-run` writes nothing, `--stdout`
prints instead.

### apply renders first

`apply` re-renders the targeted nodes before doing anything, so what
reaches a node is what the config and patches currently say. Applying whatever
happened to be left in the output directory meant a patch added since the last
render was silently not applied — the change looked like it landed and hadn't —
and it made `--dry-run` compare live state against a stale artefact and report
agreement. `--no-render` applies the existing files if you specifically want
that.

### Rolling changes out safely

`apply` treats one node at a time as the unit of risk. After each node it waits
for that node to answer the API again and hold steady for `--stabilize` (30s),
and stops the roll-out if it does not — leaving the remaining nodes untouched,
rather than rebooting the next control plane while the last is still away.
The wait asks only the node itself, so a cluster that is already in trouble
does not hold it up. Disable it with `--wait=false` — which talman refuses for
a run that reaches more than one control plane, since without the wait the
next one would go down while the last is still away.

`--health` adds a cluster health check between nodes, and stops the roll-out
if the cluster is unhealthy. It is off by default: it asks the whole cluster,
so on one that is unhealthy before the run starts it fails after the first
node, and that includes the apply meant to fix it. It runs between nodes and
not after the last, since by then there is nothing left for it to protect.

Neither runs for `--dry-run` or `--mode=staged`, where nothing was enacted.
The wait says what it is waiting for while it waits — a node installing Talos
for the first time is away for minutes, and silence is indistinguishable from
a hang — and the health gate is bounded by the same `--timeout`, because
`talosctl` otherwise takes twenty minutes to report that a gate will not pass.
The health gate also stands down while any configured node is still outside
the cluster — see below — which covers a node being onboarded.

Both `apply`'s gate and `talman health` run the check from one control plane —
the first in the config that answers the Talos API, or `--node` — and report on
every node the config lists. Not from the VIP: a Talos VIP is only held by a
control plane in etcd quorum, so it disappears during exactly the outage worth
checking, and as a node address it means "whichever machine holds it right
now". The VIP's place is `--endpoints`, where the talosconfig already lists
every control plane.

`upgrade` first asks each node what it is running, and skips it when the Talos
version and schematic already match what the config resolves to:

```console
$ talman upgrade
== development-control-01 (10.0.0.11) already runs v1.14.0 with schematic 079113ce0508; skipping
== development-worker-01 (10.0.0.21): v1.13.5 (schematic 079113ce0508) -> v1.14.0
   image factory.talos.dev/openstack-installer/079113ce0508…:v1.14.0
```

The check is against *running* state, so it ignores the registry and
repository half of the installer reference: those decide where the next image
is pulled from, not what is running, and switching mirrors is not a reason to
reboot a cluster. Anything talman cannot determine counts as out of date — an
unknown version is never read as agreement. `--force` upgrades regardless, and
`--dry-run` stops after the asking and names the nodes an upgrade would reach.

talosctl waits for each node to come back on its new version, up to
`--timeout` (30m), before talman moves on to the next. It always does: it
drains each node before upgrading it, and a drain waits. `--health` gates
between nodes as it does for `apply`.

`reboot` is the roll-out for anything only a reboot applies, above all a
`talman apply --mode=staged`, which says so when it is done:

```console
$ talman apply --mode=staged
...
configs staged; they take effect on the next reboot → talman reboot
$ talman reboot --health
```

It goes one node at a time in config order, control planes always alone, and
talosctl waits for each to come back before the next, up to `--timeout`
(30m). `--parallel` and `--health` mean what they mean for `upgrade`;
`--wait=false` only starts each reboot, which talman refuses for more than one
control plane at a time, and `--mode powercycle` bypasses kexec.

`upgrade-k8s` does for Kubernetes what `upgrade` does for Talos: it asks every node which version
its kubelet runs, and does nothing when they are all already on
`kubernetesVersion`.

```console
$ talman upgrade-k8s
== development-control-01 (10.0.0.11) already runs Kubernetes v1.37.0
== development-worker-01 (10.0.0.21) already runs Kubernetes v1.37.0
nothing to upgrade: every node already runs Kubernetes v1.37.0 (use --force to upgrade anyway)
```

The kubelet is the signal because it is the one Kubernetes component every node
runs, and the last one `talosctl upgrade-k8s` moves — an upgrade that failed
part way through leaves it behind, so a kubelet on the target version means the
control plane components got there first.

`--dry-run` answers for talman, not for `talosctl`: on an up-to-date cluster it
reports nothing to upgrade, because that is what running the command would do.
A dry run that printed a component-by-component plan the real command would
never carry out is worse than no dry run at all. `--force --dry-run` reaches
`talosctl`'s own plan.

`reset` returns a node to maintenance mode. It wipes the EPHEMERAL and STATE
partitions — all data, and the machine config with it — and reboots the node
with Talos still installed, waiting for a config:

```console
$ talman reset --graceful=false
About to reset 6 node(s) in cluster "sto1-com":
  talos-w01 (10.164.0.32)
  ...
This wipes EPHEMERAL and STATE, destroying all data on them and their machine
configs, then reboots them into maintenance mode. Type the cluster name to continue:
```

Those are `talosctl`'s `--system-labels-to-wipe` and `--reboot`, and neither is
its default: left to itself `talosctl` wipes the system disk whole, bootloader
included, and powers the machine off — a reinstall, not a reset. `--wipe-disk`
asks for that state deliberately. `--wipe-labels` narrows what goes:
`--wipe-labels=EPHEMERAL` keeps STATE, so the node reboots back into the
cluster as itself rather than into maintenance mode. `--reboot=false` shuts the
node down instead. The prompt says which of these you are about to do, because
"destroys all data" reads the same whether a node comes back or stops booting.

Nodes are wiped workers first, control planes last, and each is reached at its
own address rather than through the talosconfig endpoints:

```console
$ talman reset --graceful=false
== resetting talos-w01 (10.164.0.32)
...
== resetting talos-c01 (10.164.0.27)
```

The endpoints *are* the control planes, so config order — control planes first
— cut the reset's own path partway through: a worker's reset would be proxied
through a control plane wiped three steps earlier, and time out. It is also the
order a graceful reset needs, since leaving etcd and the Kubernetes API takes a
control plane that is still serving. `--direct=false` restores endpoint routing
for a network where node addresses are not reachable from where talman runs.

Selecting *every* control plane means the cluster does not survive the run, so
those nodes skip the graceful leave. Graceful asks etcd to remove the node from
its member list, and etcd only agrees while enough members remain to agree on
anything — so the last control plane of a teardown asks a cluster that cannot
answer:

```
failed to leave cluster: failed to remove member 7443577814378962433:
etcdserver: re-configuration failed due to not enough started members
```

That fails the reset in its fifth phase with the wipe undone, on the last node,
after the rest of the cluster is already gone. Workers still leave gracefully —
they go first, while the cluster is serving — and resetting a subset of the
control planes keeps the graceful leave, because then there is a cluster to
leave. An explicit `--graceful` is honoured, with a warning saying where it
will fail.

A reset node comes back with no config and no identity, so everything that
needs one waits: containerd, the CRI, and any extension service behind them.
`iscsi-tools` parks on `waiting for file /etc/iscsi/initiatorname.iscsi to
exist` — the initiator name is derived from the node identity, which lives in
STATE — and a console showing that looks like a boot that never finishes. It is
not: the node is in maintenance mode, and `talman apply --onboard-new-nodes -n <node>` onboards it
and clears the wait.

A reset still stops at the first failure, and names what it did not get to:

```console
error: talosctl reset exited with status 1
  2 node(s) were not reset: talos-w02, talos-w03
  continue with: talman reset -n talos-w02 -n talos-w03 --graceful=false
```

### Rolling out in waves

By default a roll-out takes the nodes in config order. `rollout` in the config
orders them by group instead, so a canary goes first and the rest follow it:

```yaml
rollout:
  soak: 10m               # wait after each wave, then gate on health
  waves:
    - controlplane        # a role or a group
    - blue                # the canary
    - [green, pink]       # several groups make one wave
```

`upgrade`, `reboot` and `apply` go wave by wave, and inside a wave by the
usual rules: control planes alone, workers up to `--parallel`. A node belongs
to the first wave that names its role or one of its groups, and nodes no wave
names form a last wave, `rest`, so a list that leaves someone out still
reaches them — after everything it does name.

Between waves that changed something, talman soaks for `soak`, if one is set,
and then runs the health gate if `--health` asked for it. A canary is only
worth something if the gate looks after it has had time to go wrong.

A run never stops at a wave by itself: where to stop is the operator's call,
made run by run. `--until <group>` stops after the wave holding it and prints
the command that carries on, `--from <group>` starts at it, and `--wave
<group>` runs only that one; `rest` names the last wave, so no node may call a
group that.

```console
$ talman upgrade --health --until blue     # the canary, and everything before it
== wave 1/3: controlplane, 1 node(s)
...
== wave 2/3: blue, 3 node(s)
...
stopped before wave green, as --until asked
  continue with: talman upgrade --from green --health
```

These select from the config's waves and never reorder them, so the order is
the one that was reviewed, and a selection that reaches no node is an error
rather than a run that quietly did nothing.

Carrying on is safe to repeat: `upgrade` skips a node already on its target,
and `apply` to a node that already has its config changes nothing.

### Backing up etcd

```console
$ talman etcd snapshot
== snapshotting etcd on talos-c01 (10.164.0.27)
wrote clusterconfig/etcd-sto1-com-20260924T101500Z.db
```

The snapshot comes from the first control plane that answers, or `--node`,
and lands in the output directory unless you name a path: an etcd snapshot
holds every Kubernetes Secret in the cluster, so it belongs beside the other
credentials, gitignored and `0600`. talman never overwrites one.

`upgrade --snapshot` and `upgrade-k8s --snapshot` take one before they change
anything, which is the moment a snapshot is most worth having.

### Rotating the CAs

```console
$ talman rotate-ca --dry-run     # what talosctl would do
$ talman rotate-ca               # asks for the cluster name
...
wrote secrets.sops.yaml, extracted from talos-c01 (the old one is clusterconfig/secrets-pre-rotate-20260924T101500Z.yaml)
wrote clusterconfig/talosconfig, signed by the new Talos CA
```

`talosctl rotate-ca` rolls new Talos API and Kubernetes API CAs out to every
node gracefully. What it leaves behind is a secrets bundle holding the old
ones, which the next `apply` would put back. So once the rotation is done,
talman extracts the bundle from a control plane's new machine config — as
`secrets generate --from-controlplane-config` does — and writes it over the
old one, encrypted if the old one was. The old one is kept in the gitignored
output directory, since it still holds every key a rotation leaves alone, and
the talosconfig is replaced with the one the new CA signed.

Before anything rotates, talman checks it will be able to finish: that the
bundle can be read back out of a control plane's running config, and that the
new one can be encrypted the way the old one is. A cluster it cannot read, a
missing `sops`, or a `.sops.yaml` rule that no longer matches refuses the run
instead of stranding it halfway.

If a rotation does stop after the nodes have changed, one command finishes
it, reading the bundle back and putting the rotated talosconfig in place. It
asks for the cluster name first, as a rotation does (`-y` skips it):

```console
$ talman rotate-ca --finish
``` A `talosconfig.rotated` left by a rotation
that did not finish is never deleted — it may be the only talosconfig the
cluster still accepts — and rotate-ca refuses to start until you have dealt
with it.

`--talos=false` or `--kubernetes=false` rotates only the other. Afterwards,
commit the new bundle, `talman render`, and fetch a new `talman kubeconfig` if
the Kubernetes CA changed. Should anything fail after the rotation itself, the
error says so and spells out the two commands that finish the job by hand.

## Compatibility

talman follows [semantic versioning](https://semver.org/). Within 1.x, these
do not change in a way that breaks what already works:

- **The config schema, `talman.dev/v1`.** Keys may be added; none is renamed,
  removed or given a new meaning. A schema that has to break gets a new
  `apiVersion`, and talman says so rather than misreading it.
- **The template scope** patches see: `.Cluster`, `.Node`, `.Values` and
  their fields, and the patch order.
- **Commands and flags.** Flags may be added. None is removed or renamed, and
  no default changes what a command does to a cluster.
- **Exit codes**, including `--detailed-exit-code`'s 0/2/1.
- **`-o json`**, which gains fields and keeps the ones it has.
- **Metric names and labels.**
- **The rendered output layout**: `<outputDir>/<hostname>.yaml`,
  `talosconfig` and `kubeconfig`.

Not covered: the text commands print for people, which may be reworded, and
anything `talosctl` itself prints, which talman passes through. The talosctl
floor (currently v1.14.0) may rise in a minor release, when a Talos release
talman supports needs a newer one, and the release notes say so.

## Tests

`go test ./...` covers the parts that can be reasoned about without a cluster:
patch resolution, template rendering, schematic IDs, every decision talman
makes about what a node is and whether it needs anything done to it. The render
tests shell out to a real `talosctl` and skip without one.

`hack/e2e.sh` drives a real cluster. It builds one with `talosctl`'s docker
provisioner, adopts its secrets, and then runs talman against it: render,
validate, status and its JSON, kubeconfig, health, an apply with its wait, the
decisions `upgrade` and `upgrade-k8s` make about whether there is anything to
do, `ctl` by hostname, an `etcd snapshot`, and a `rotate-ca` with the render,
kubeconfig and apply that follow it. CI runs it on every pull request.

What it cannot cover is what a docker node cannot do: it has no disk, so
nothing installs, nothing reboots, nothing sits in maintenance mode and Talos
cannot be upgraded.

`hack/e2e-qemu.sh` covers exactly those, on real virtual machines, and needs
KVM — which hosted runners do not reliably offer. It builds a cluster one patch
release behind, has talman upgrade a worker and then the control plane, stages
an apply and lands it with a rolling `reboot`, resets the worker back to
maintenance mode, and onboards it into the cluster it just left. On a fresh Ubuntu 24.04 machine:

```console
$ sudo ./hack/e2e-qemu.sh --install-deps   # qemu, the CNI plugins, talosctl
$ sudo -E ./hack/e2e-qemu.sh               # builds talman from this checkout
```

`--install-deps` installs everything that is not this repository: qemu, the CNI
plugins, talosctl, and the Go toolchain `go.mod` asks for — Ubuntu's own Go is
years older than that, and a build that fails on the version line is a worse
first run than a download. Binaries go to `/usr/local` where sudo will find
them, which is the first thing that goes wrong otherwise: a talosctl in your
home directory is not on root's PATH.

talman is then built from the checkout, unless one is installed already or
`TALMAN` points at one. No sops: the bundle this generates is plaintext, and
talman only reaches for sops when a bundle is encrypted.

Root because the provisioner creates bridges and tap devices. CI runs it
nightly on a self-hosted runner labelled `kvm`, and never as a gate on a pull
request.

## Upgrading from a 1.0.0 prerelease

1.0.0 settled the schema and the command line, so a cluster directory written
for an alpha or a beta may need these changes. None of them is read
differently: a renamed key is refused with an error naming its new spelling,
and a removed flag is refused as an unknown flag.

| before | 1.0.0 |
| --- | --- |
| `apiVersion` optional | required: `apiVersion: talman.dev/v1` |
| `talosMode:` | `validationMode:`, and usually not needed: it now follows `imageFactory.platform` |
| `imageFactory.secureboot` | `imageFactory.secureBoot` |
| any `hostname` | a lower-case RFC 1123 name, since it is also a file name |
| `endpoint` without a port | `https://…:6443` |
| `upgrade --stage`, `--skip-etcd-check` | removed: Talos 1.14's upgrade API ignores both; `--extra-flags` reaches them on a legacy node |
| `upgrade --wait=false` | removed: talosctl waits whenever it drains, which is by default |
| `apply` configuring a node in maintenance mode by itself | `apply --onboard-new-nodes`, or `--only-new-nodes`: the maintenance service authenticates nothing |
| `apply --only-new` | `apply --only-new-nodes` |
| `talman bootstrap`, and `apply` on a cluster not bootstrapped | `talman apply --bootstrap` builds a new cluster in one run |

## Migrating from talhelper

| talhelper | talman |
| --- | --- |
| `patches: ["@./p.yaml"]` | `patches.all: [./p.yaml]` — no `@` |
| `controlPlane.patches` / `worker.patches` | `patches.controlplane` / `patches.worker` |
| `nodes[].patches` | unchanged |
| `controlPlane: true` | `role: controlplane` |
| `installDisk`, `nodeLabels`, `networkInterfaces`, … | patches templated from `nodes[].values` |
| `talsecret.sops.yaml` | `secrets.sops.yaml`, or point `secretFile` at the old name |
| `talenv.yaml` + `${VAR}` | `values:` and sprig's `env` |
| `genconfig` | `render` |
| `gencommand apply \| bash` | `apply` |
| RFC6902 patches | rewrite as strategic merge (Talos dropped them for multi-doc) |
| `imageFactory.installerURLTmpl` | unchanged, and `.Mode` still resolves |

The bundle itself needs no conversion — the format is talosctl's, and talman
reads exactly what talhelper wrote. Only the filename talman looks for differs,
so either rename the file or keep the old name and say so:

```yaml
secretFile: talsecret.sops.yaml
```

Your `.sops.yaml` needs no changes: a `path_regex` ending in `\.sops\.yaml$`
matches both names.

## Migrating from topf

`all/` becomes `patches.all`, `control-plane/` becomes `patches.controlplane`,
`node/<host>/` becomes that node's `patches`, each listed by path in the order
you want. `topf.yaml`'s `data:` splits into cluster-wide `values:` and per-node
`values:`. Drop the `.tpl` suffix — every patch is a template.

## Name

Short for *Talos manager*, and in Swedish the *talman* is the Speaker of the
Riksdag: the presiding officer who does not legislate, but runs the procedure
and recognises speakers in order. Which is the job — talman orders the patch
chain and leaves the actual work to `talosctl`.

## Contributing

Pull requests are welcome, and any help at all is appreciated — including bug
reports, and telling me when an error message or a sentence in these docs sent
you the wrong way. See [CONTRIBUTING.md](CONTRIBUTING.md), and the
[Code of Conduct](CODE_OF_CONDUCT.md) that everyone taking part follows.
Security issues go through [SECURITY.md](SECURITY.md) rather than the issue
tracker.

## License

MIT
