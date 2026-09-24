#!/usr/bin/env bash
# Drives talman against a real Talos cluster.
#
# The cluster comes from talosctl's docker provisioner, which is the only one
# that runs on a hosted CI runner: the qemu provisioner needs KVM, and KVM is
# not reliably available on GitHub's Linux runners. What that costs is real and
# worth naming -- docker nodes have no disks, so there is no install, no
# reboot, no maintenance mode and no Talos upgrade. Those paths need a runner
# with KVM and the qemu provisioner.
#
# What is left is still most of what breaks: rendering against a real talosctl,
# adopting an existing cluster's secrets, the per-node probing, apply with its
# wait and its health gate, the exit codes, the decisions upgrade and
# upgrade-k8s make about whether there is anything to do, talosctl passed
# through by hostname, an etcd snapshot, and a CA rotation with the bundle and
# talosconfig that follow it.
set -euo pipefail

cluster=${CLUSTER_NAME:-talman-e2e}
workdir=${WORKDIR:-$(mktemp -d)}
talman=${TALMAN:-talman}
keep=${KEEP_CLUSTER:-}

# The provisioner's subnet: the first control plane takes .2, workers follow.
subnet=${SUBNET:-10.5.0.0/24}
controlplane=10.5.0.2
worker=10.5.0.3

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]:-$0}")/lib.sh"

cleanup() {
	if [ -n "$keep" ]; then
		printf '\nkeeping cluster %s (KEEP_CLUSTER set)\n' "$cluster"
		return
	fi

	step "destroying the cluster"
	talosctl cluster destroy --name "$cluster" >/dev/null 2>&1 || true
}
trap cleanup EXIT

step "creating a cluster with the docker provisioner"
talosctl cluster create docker \
	--name "$cluster" \
	--subnet "$subnet" \
	--workers 1 \
	--talosconfig-destination "$workdir/provisioner-talosconfig"

export TALOSCONFIG="$workdir/provisioner-talosconfig"

# What the cluster actually is, rather than what this script assumes: the
# versions talman is told to want have to match, or every command it runs
# would report drift that is really a mismatch in this file.
# `version` has no --short, so the server's Tag is read out of the block.
talos_version=$(talosctl --nodes "$controlplane" version |
	awk '/Server:/ {found=1} found && /Tag:/ {print $2; exit}')
kubernetes_version=$(talosctl --nodes "$controlplane" get kubeletspec -o yaml |
	grep -m1 'image:.*kubelet:' | sed 's/.*kubelet://')

printf 'cluster runs Talos %s, Kubernetes %s\n' "$talos_version" "$kubernetes_version"

step "adopting the cluster's secrets"
cd "$workdir"
talosctl --nodes "$controlplane" read /system/state/config.yaml > controlplane.yaml

cat > talman.yaml <<EOF
apiVersion: talman.dev/v1
clusterName: $cluster
endpoint: https://$controlplane:6443
talosVersion: $talos_version
kubernetesVersion: $kubernetes_version

patches:
  all:
    - ./patches/label.yaml

nodes:
  - hostname: $cluster-controlplane-1
    ipAddress: $controlplane
    role: controlplane
  - hostname: $cluster-worker-1
    ipAddress: $worker
    role: worker
EOF

mkdir -p patches
# Something small, real and rebootless: a node label, which is exactly the
# kind of change an apply is asked for and exactly the kind docker nodes can
# take without an install behind them.
cat > patches/label.yaml <<'EOF'
apiVersion: v1alpha1
kind: KubeNodeConfig
labels:
  talman.dev/e2e: "true"
EOF

expect_exit 0 "secrets generate --from-controlplane-config" \
	"$talman" secrets generate --plaintext --from-controlplane-config controlplane.yaml

step "validate and render"
expect_exit 0 "validate" "$talman" validate
expect_exit 0 "render" "$talman" render

[ -s "clusterconfig/$cluster-controlplane-1.yaml" ] || fail "no machine config was written"
[ -s clusterconfig/talosconfig ] || fail "no talosconfig was written"
contains "$(cat clusterconfig/$cluster-worker-1.yaml)" "talman.dev/e2e" "the patch reached the rendered config"

step "status against the running cluster"
status=$("$talman" status)
printf '%s\n' "$status"
contains "$status" "running" "status sees a running node"
contains "$status" "$talos_version" "status reports the Talos version the cluster runs"
grep -q "unreachable" <<<"$status" && fail "status reports a node as unreachable"
grep -q "not bootstrapped" <<<"$status" && fail "status claims a bootstrapped cluster is not bootstrapped"

step "kubeconfig, and a cluster that answers kubectl"
expect_exit 0 "kubeconfig" "$talman" kubeconfig
[ -s clusterconfig/kubeconfig ] || fail "no kubeconfig was written"
# talman's own copy is overwritten, which talosctl only does with --force.
expect_exit 0 "kubeconfig, again" "$talman" kubeconfig

if command -v kubectl >/dev/null; then
	nodes=$(kubectl --kubeconfig clusterconfig/kubeconfig get nodes --no-headers | wc -l)
	[ "$nodes" -eq 2 ] || fail "kubectl sees $nodes nodes, want 2"
	pass 'kubectl sees both nodes'
fi

step "health"
expect_exit 0 "health" "$talman" health

step "apply reports whether anything would change"
# 0 or 2, both meaningful; 1 would mean the run itself failed.
dry=0
"$talman" apply --dry-run --detailed-exit-code >/dev/null || dry=$?
case $dry in
0 | 2) pass "apply --dry-run --detailed-exit-code (exit $dry)" ;;
*) fail "apply --dry-run failed with exit $dry" ;;
esac

step "apply to the worker: probe, apply, wait"
# --mode=try, so Talos rolls the configuration back on its own: a rendered
# config is not guaranteed to suit a node that has no disk behind it, and this
# step is here to exercise talman's machinery -- the probe, the invocation, the
# wait for the node to come back -- rather than Talos's appetite for it.
#
# The health gate is exercised by `talman health` above, on the whole cluster,
# rather than here where a rolled-back config would make it mean something
# else.
expect_exit 0 "apply --mode=try" \
	"$talman" apply -n "$cluster-worker-1" --mode=try --stabilize=5s --timeout=3m

step "upgrade decides there is nothing to do"
# Both nodes already run the configured version, and a docker node reports no
# schematic, which talman reads as agreement with a vanilla one.
expect_exit 0 "upgrade --detailed-exit-code" "$talman" upgrade --detailed-exit-code

step "upgrade-k8s decides there is nothing to do"
expect_exit 0 "upgrade-k8s --detailed-exit-code" "$talman" upgrade-k8s --detailed-exit-code --dry-run

step "status as JSON"
json=$("$talman" status -o json)
contains "$json" '"status": "running"' "status -o json reports running nodes"
contains "$json" "\"running\": \"$talos_version\"" "status -o json reports the Talos version"

step "talosctl through talman, by hostname"
expect_exit 0 "ctl get members" "$talman" ctl -n "$cluster-controlplane-1" get members
out=$("$talman" ctl -n "$cluster-worker-1" -- get machinetype -o yaml)
contains "$out" "worker" "ctl reached the worker it was named"
code=0
"$talman" ctl -- no-such-command >/dev/null 2>&1 || code=$?
[ "$code" -ne 0 ] || fail "a failing talosctl command exited 0 through talman"
pass "talosctl's failure is talman's (exit $code)"

step "etcd snapshot"
expect_exit 0 "etcd snapshot" "$talman" etcd snapshot
snapshot=$(find clusterconfig -name "etcd-$cluster-*.db" -perm 600 | head -1)
[ -s "$snapshot" ] || fail "no 0600 snapshot in the output directory"
pass "wrote $snapshot"

step "rotate-ca, and the bundle and talosconfig that follow it"
cp secrets.sops.yaml secrets-before-rotation.yaml
expect_exit 0 "rotate-ca --dry-run" "$talman" rotate-ca --dry-run
cmp -s secrets.sops.yaml secrets-before-rotation.yaml || fail "a dry run changed the bundle"
expect_exit 0 "rotate-ca" "$talman" rotate-ca -y
cmp -s secrets.sops.yaml secrets-before-rotation.yaml && fail "the bundle still holds the old CAs"
pass "the bundle was replaced"
# Everything from here reaches the cluster through the rotated talosconfig,
# and renders from the bundle extracted after the rotation.
expect_exit 0 "health, after rotating" "$talman" health
expect_exit 0 "render, after rotating" "$talman" render
expect_exit 0 "kubeconfig, after rotating" "$talman" kubeconfig
dry=0
"$talman" apply --dry-run --detailed-exit-code >/dev/null || dry=$?
case $dry in
0 | 2) pass "apply --dry-run after rotating (exit $dry)" ;;
*) fail "apply --dry-run failed after rotating, exit $dry" ;;
esac

step "an old talosctl is refused before it reaches the cluster"
printf '#!/bin/sh\nprintf "Client:\\n\\tTag:\\tv1.9.5\\n"\n' > old-talosctl
chmod +x old-talosctl

cp talman.yaml talman-old.yaml
printf 'talosctl: %s/old-talosctl\n' "$workdir" >> talman-old.yaml

out=$("$talman" -c talman-old.yaml status 2>&1 || true)
contains "$out" "too old" "an old talosctl is refused"

printf '\n\033[32mall end-to-end checks passed\033[0m\n'
