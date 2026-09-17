# talman

Manage [Talos Linux](https://www.talos.dev/) clusters and, above all, their
configuration patches.

> [!WARNING]
> **talman is early in development.** It is pre-1.0 and ships as alpha
> releases: the config schema, the commands and their flags can still change in
> ways that break your cluster directory, and an upgrade may need a manual
> migration. It also drives real clusters — read what `render` writes and what
> `diff` reports before you `apply`. Pin a tagged version rather than tracking
> `main`.

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
go install github.com/ludwighansson/talman/cmd/talman@v1.0.0-alpha.1
```

Prebuilt archives for Linux, macOS, Windows and FreeBSD are on the
[releases page](https://github.com/ludwighansson/talman/releases). Pin the tag
rather than reaching for `@latest`: while talman is pre-1.0, `@latest` follows
every alpha, breaking changes included.

You also need [`talosctl`](https://docs.siderolabs.com/talos/v1.14/talosctl) on
`PATH`, at least as new as the Talos version you target, and
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

See [`example/`](example/) for a working tree.

## Configuration

```yaml
clusterName: development
endpoint: https://10.0.0.10:6443
talosVersion: v1.14.0            # required: pinning it is what makes renders reproducible
kubernetesVersion: v1.37.0

values:                          # free-form, available to templates as .Values
  harbor: harbor.example.net

imageFactory:
  installerURLTmpl: "{{.RegistryURL}}/openstack-installer/{{.ID}}:{{.Version}}"

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
(`clusterconfig`), `secretFile` (`secrets.sops.yaml`), `talosMode` (`metal`),
`schematicID`. Per-node: `talosVersion`, `schematic`, `schematicID`.

Unknown keys are an error. In a config whose job is to route patch files, a
mistyped key that got ignored would mean a machine config quietly missing a
patch.

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
already exists, use `--from-controlplane-config`.

Rendered machine configs *do* contain secrets, so `render` writes them `0600`
and drops a `.gitignore` that excludes the whole output directory.

## Commands

| Command | |
| --- | --- |
| `talman validate` | check keys, paths and templates; needs no secrets or network |
| `talman patches` | the resolved patch chain per node, in application order |
| `talman nodes` | the node table (`--status` asks each node what it is) |
| `talman secrets generate` | create the encrypted secrets bundle |
| `talman render` | write machine configs and a talosconfig |
| `talman schematic id` | the resolved schematic ID per node |
| `talman image url` | the installer image reference per node |
| `talman diff` | re-render, then show what applying would change |
| `talman apply` | re-render, then apply, adopting nodes in maintenance mode |
| `talman bootstrap` | initialise etcd, once |
| `talman kubeconfig` | fetch the kubeconfig into the output directory |
| `talman upgrade` | upgrade Talos to each node's configured installer image |
| `talman upgrade-k8s` | upgrade Kubernetes to `kubernetesVersion` (a noop when every node is already there) |
| `talman health` | cluster health |
| `talman reset` | wipe nodes (requires typing the cluster name) |
| `talman version` | the talman and talosctl versions |

`-n/--node` restricts most commands to named nodes (hostname or IP,
repeatable). `-v` echoes every `talosctl` invocation. `-c` points at a config
other than `./talman.yaml`.

### Adopting nodes

A node that has never been configured — or that has just been `reset` — answers
only the maintenance service, while nodes already in the cluster answer with
cluster PKI. `apply` asks each node which it is, immediately before sending its
config, so a half-adopted cluster needs no flag:

```console
$ talman apply
== talos-c01 (10.164.0.27)
== talos-w01 (10.164.0.32) maintenance mode; adopting it
```

The probe pins `--endpoints` to the node itself. Left to its own devices
`talosctl` routes `--nodes` through the talosconfig's endpoints, which are the
control planes, so the answer would describe whichever control plane proxied —
and a node in maintenance mode has no proxy path at all.

A fresh cluster:

```console
$ talman apply -n talos-c01   # adopted: installs and reboots into its config
$ talman bootstrap            # once, ever: initialises etcd
$ talman apply                # the rest, control planes and workers alike
```

Adding a machine to a live cluster is `talman apply -n <new node>`, and
re-adopting one after `reset` is the same command. `--only-new` does the whole
lot at once, restricting the run to the nodes currently in maintenance mode:

```console
$ talman apply --only-new
   talos-c01 (10.164.0.27) is running; not new, skipping
== talos-w01 (10.164.0.32) maintenance mode; adopting it
```

`talman nodes --status` is the view of the same question, and the only form of
`nodes` that talks to the cluster:

```console
$ talman nodes --status
HOSTNAME    ADDRESS       ROLE           STATUS             GROUPS   PATCHES   TALOS
talos-c01   10.164.0.27   controlplane   running            -        5         v1.14.0
talos-w01   10.164.0.32   worker         maintenance mode   -        4         v1.14.0
talos-w02   10.164.0.29   worker         unreachable        -        4         v1.14.0
```

`-i` still forces the maintenance service for every node and `--insecure=false`
forces cluster PKI, for when the answer is known better than the probe can tell
— but neither is needed for a mixed cluster any more, which is what they used
to be reached for and what they were never able to do.

A node that answers neither API stops the run and names what is left, since
that is a fault rather than a state:

```console
error: talos-w02 (10.164.0.29) answers neither the Talos API nor the maintenance service
  2 node(s) were not applied: talos-w02, talos-w03
  continue with: talman apply -n talos-w02 -n talos-w03
```

The health gate stands down while any node in the config is still outside the
cluster, naming the ones that are:

```console
   not gating on cluster health: talos-c02 is maintenance mode, talos-c03 is unreachable (pass --health to check anyway)
```

The check covers the cluster the config describes, so during a build-out it
checks machines that have not joined yet — and an unadopted node answers with a
self-signed maintenance certificate, which `talosctl` reports as `certificate
signed by unknown authority`. That is a build-out step, not a broken cluster.
Once every node has joined, the gate runs between nodes as it should, which is
the roll-out it exists for. `--health` gates regardless.

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

`apply`, `diff`, `bootstrap`, `kubeconfig`, `upgrade`, `upgrade-k8s`, `health`
and `reset` all authenticate with `clusterconfig/talosconfig`, and generate it
when it is not there:

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

### Reaching talosctl flags talman does not model

Every command that shells out takes `--extra-flags`, appended verbatim to the
underlying invocation:

```console
$ talman render --extra-flags=--with-docs=true
$ talman apply --extra-flags=--timeout=5m
$ talman upgrade --extra-flags=--reboot-mode=powercycle
$ talman reset --extra-flags=--system-labels-to-wipe=STATE --extra-flags=--system-labels-to-wipe=EPHEMERAL
```

It is repeatable rather than one string talman splits: values contain commas,
spaces and quotes, and splitting them here would mangle exactly the arguments
most worth passing through.

Flags land last on the command line, so they override what talman set — which
is the point, and also the caveat. `--extra-flags=--output=/tmp/x` on `render`
will send a machine config somewhere talman then cannot find. Use `-v` to see
the full command.

`validate`, `patches`, `nodes`, `schematic id` and `image url` have no such
flag: none of them invokes `talosctl`.

`render` validates every generated config with `talosctl validate` before
writing; `--no-validate` skips it, `--dry-run` writes nothing, `--stdout`
prints instead.

### apply renders first

`apply` and `diff` re-render the targeted nodes before doing anything, so what
reaches a node is what the config and patches currently say. Applying whatever
happened to be left in the output directory meant a patch added since the last
render was silently not applied — the change looked like it landed and hadn't —
and it made `diff` compare live state against a stale artefact and report
agreement. `--no-render` applies the existing files if you specifically want
that.

### Rolling changes out safely

`apply` treats one node at a time as the unit of risk. After each node it waits
for that node to answer the API again and hold steady for `--stabilize` (30s),
then runs a cluster health check, and stops the roll-out if either fails —
leaving the remaining nodes untouched. That is the difference between a bad
patch costing you one machine and costing you the control plane. Disable with
`--wait=false` / `--health=false`; neither runs for `--dry-run` or
`--mode=staged`, where nothing was enacted, and the health gate also stands
down for `--insecure` — installing a node that had no config to authenticate
with is the one case where there need not be a cluster yet. Pass `--health`
explicitly to gate anyway.

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
unknown version is never read as agreement. `--force` upgrades regardless;
`--skip-etcd-check` is the separate flag that passes `--force` to `talosctl`.

`upgrade-k8s` does the same for Kubernetes: it asks every node which version
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

A reset still stops at the first failure, and names what it did not get to:

```console
error: talosctl reset exited with status 1
  2 node(s) were not reset: talos-w02, talos-w03
  continue with: talman reset -n talos-w02 -n talos-w03 --graceful=false
```

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

## License

MIT
