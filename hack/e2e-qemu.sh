#!/usr/bin/env bash
# Drives talman against real Talos virtual machines.
#
# The docker end-to-end (hack/e2e.sh) runs anywhere and covers most of talman,
# but docker nodes have no disks: nothing installs, nothing reboots, nothing
# ever sits in maintenance mode, and a Talos upgrade is impossible. Those are
# the paths that reboot machines and rewrite disks, which makes them the ones
# worth testing on hardware that can actually do it.
#
# So this one uses talosctl's qemu provisioner, which needs KVM. It is written
# to run on an Ubuntu 24.04 (noble) virtual machine with nested virtualisation,
# whether that machine is a self-hosted CI runner or a laptop's VM:
#
#	sudo ./hack/e2e-qemu.sh --install-deps    # once, on a fresh VM
#	sudo -E ./hack/e2e-qemu.sh
#
# Root is not a style choice: the provisioner creates bridges and taps.
#
# What it proves, in order: a cluster comes up, talman adopts it, an upgrade
# actually replaces Talos on a node, a reset returns that node to maintenance
# mode, and talman adopts it back into the cluster it just left.
set -euo pipefail

# shellcheck source=hack/lib.sh
. "$(dirname "${BASH_SOURCE[0]:-$0}")/lib.sh"

cluster=${CLUSTER_NAME:-talman-e2e-qemu}
workdir=${WORKDIR:-$(mktemp -d)}
talman=
keep=${KEEP_CLUSTER:-}

# The cluster is built one patch release behind what talman will be told to
# want, so `talman upgrade` has somewhere real to go.
from_version=${FROM_VERSION:-v1.14.0}
to_version=${TO_VERSION:-v1.14.1}

cni_version=${CNI_VERSION:-v1.9.1}
talosctl_version=${TALOSCTL_VERSION:-v1.14.1}
state_dir="$workdir/state"

controlplane=10.5.0.2
worker=10.5.0.3

install_deps() {
	step "installing everything this needs on a fresh Ubuntu machine"

	export DEBIAN_FRONTEND=noninteractive
	apt-get update
	apt-get install -y --no-install-recommends \
		qemu-system-x86 qemu-utils bridge-utils iproute2 iptables dnsmasq-base curl ca-certificates tar

	# The provisioner wires the cluster network with CNI plugins and expects
	# them where CNI puts them.
	mkdir -p /opt/cni/bin
	curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${cni_version}/cni-plugins-linux-amd64-${cni_version}.tgz" |
		tar -xz -C /opt/cni/bin

	# talosctl builds the cluster and talman drives it. Installed where sudo
	# will find it, which is the whole reason this step exists: a talosctl in
	# somebody's home directory is not on root's PATH.
	curl -fsSL -o /usr/local/bin/talosctl \
		"https://github.com/siderolabs/talos/releases/download/${talosctl_version}/talosctl-linux-amd64"
	chmod +x /usr/local/bin/talosctl

	# sops is deliberately absent: the bundle this script generates is
	# plaintext, and talman only reaches for sops when a bundle is encrypted.

	pass "qemu $(qemu-system-x86_64 --version | head -1 | awk '{print $4}')"
	pass "cni plugins $cni_version in /opt/cni/bin"
	pass "talosctl $(talosctl version --client --short 2>/dev/null || talosctl version --client | awk '/Tag/ {print $2; exit}')"
	note "talman itself is built from this repository; run the script again without --install-deps"
}

# talman_binary finds talman, or builds it from the repository this script
# lives in.
#
# Under sudo the invoking user's PATH is gone, so a talman built into a home
# directory is not there any more; building it into the work directory is
# cheaper than explaining that.
talman_binary() {
	if [ -n "${TALMAN:-}" ]; then
		printf '%s' "$TALMAN"
		return 0
	fi

	if command -v talman >/dev/null; then
		command -v talman
		return 0
	fi

	local repo go
	repo=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." && pwd)

	go=$(command -v go || true)
	[ -z "$go" ] && [ -x /usr/local/go/bin/go ] && go=/usr/local/go/bin/go

	if [ -n "$go" ] && [ -f "$repo/go.mod" ]; then
		mkdir -p "$workdir"
		"$go" build -o "$workdir/talman" "$repo/cmd/talman" >&2 || return 1
		printf '%s' "$workdir/talman"
		return 0
	fi

	return 1
}

preflight() {
	step "preflight"

	[ "$(uname -s)" = Linux ] || fail "this needs Linux: qemu, KVM and bridges"

	if [ -r /etc/os-release ]; then
		# shellcheck disable=SC1091
		. /etc/os-release
		note "host: ${PRETTY_NAME:-unknown}"

		[ "${VERSION_CODENAME:-}" = noble ] ||
			note "written for Ubuntu 24.04 (noble); carrying on regardless"
	fi

	[ -w /dev/kvm ] || fail "/dev/kvm is missing or not writable: this host cannot nest virtual machines"
	[ "$(id -u)" = 0 ] || fail "run as root: the provisioner creates bridges and tap devices"

	for tool in qemu-system-x86_64 talosctl; do
		command -v "$tool" >/dev/null ||
			fail "$tool is not on PATH -- run \`sudo $0 --install-deps\` first (note that sudo has its own PATH)"
	done

	[ -x /opt/cni/bin/bridge ] ||
		fail "the CNI plugins are not in /opt/cni/bin -- run \`sudo $0 --install-deps\` first"

	talman=$(talman_binary) ||
		fail "no talman: build one with \`go build -o /usr/local/bin/talman ./cmd/talman\`, or set TALMAN to its path"

	note "talman:   $talman ($("$talman" version | head -1 | awk '{print $2}'))"
	note "talosctl: $(command -v talosctl)"

	pass "kvm, qemu, the CNI plugins, talosctl and talman are all here"
}

cleanup() {
	if [ -n "$keep" ]; then
		note "keeping cluster $cluster (KEEP_CLUSTER set); destroy it with:"
		note "  talosctl cluster destroy --name $cluster --state $state_dir --provisioner qemu"
		return
	fi

	step "destroying the cluster"
	talosctl cluster destroy --name "$cluster" --state "$state_dir" >/dev/null 2>&1 || true
}

# One node's row, for the awaits below.
status_row() {
	"$talman" status -n "$1" 2>/dev/null | grep -- "$1"
}

# settled <node> -- running, and agreeing with the config.
#
# Not a match on the version string: while a node is still on the old one its
# row reads "v1.14.0 → v1.14.1", which contains the version being waited for.
# The absence of the arrow is what says the drift is gone.
settled() {
	local row
	row=$(status_row "$1") || return 1

	grep -q "running" <<<"$row" || return 1
	! grep -q "→" <<<"$row"
}

in_maintenance() {
	status_row "$1" 2>/dev/null | grep -q "maintenance mode"
}

main() {
	preflight
	trap cleanup EXIT

	step "creating a cluster of virtual machines running Talos $from_version"
	talosctl cluster create qemu \
		--name "$cluster" \
		--state "$state_dir" \
		--talos-version "$from_version" \
		--controlplanes 1 \
		--workers 1 \
		--cidr 10.5.0.0/24 \
		--talosconfig-destination "$workdir/provisioner-talosconfig"

	export TALOSCONFIG="$workdir/provisioner-talosconfig"

	kubernetes_version=$(talosctl --nodes "$controlplane" get kubeletspec -o yaml |
		grep -m1 'image:.*kubelet:' | sed 's/.*kubelet://')
	note "the cluster runs Talos $from_version, Kubernetes $kubernetes_version"

	step "adopting the cluster's secrets"
	cd "$workdir"
	talosctl --nodes "$controlplane" read /system/state/config.yaml > controlplane.yaml

	cat > talman.yaml <<-EOF
		apiVersion: talman.dev/v1
		clusterName: $cluster
		endpoint: https://$controlplane:6443
		talosVersion: $to_version
		kubernetesVersion: $kubernetes_version

		nodes:
		  - hostname: $cluster-controlplane-1
		    ipAddress: $controlplane
		    role: controlplane
		  - hostname: $cluster-worker-1
		    ipAddress: $worker
		    role: worker
	EOF

	expect_exit 0 "secrets generate --from-controlplane-config" \
		"$talman" secrets generate --plaintext --from-controlplane-config controlplane.yaml
	expect_exit 0 "validate" "$talman" validate
	expect_exit 0 "render" "$talman" render
	expect_exit 0 "health" "$talman" health

	step "status sees the version the cluster is on, and the drift to the one it is not"
	status=$("$talman" status)
	printf '%s\n' "$status"
	contains "$status" "$from_version → $to_version" "status reports the pending Talos upgrade as drift"

	step "upgrade replaces Talos on the worker"
	# The thing docker cannot do: pull an installer, write a disk, reboot into
	# the result. --detailed-exit-code says 2 because something changed.
	expect_exit 2 "upgrade --detailed-exit-code" \
		"$talman" upgrade -n "$cluster-worker-1" --detailed-exit-code

	await 600 "the worker is running $to_version, with no drift left" settled "$cluster-worker-1"
	expect_exit 0 "health after the upgrade" "$talman" health

	step "upgrade has nothing left to do on that node"
	expect_exit 0 "upgrade --detailed-exit-code, again" \
		"$talman" upgrade -n "$cluster-worker-1" --detailed-exit-code

	step "upgrade the rest, which is the control plane"
	# A control plane upgrade takes etcd down and brings it back, and talman
	# does them one at a time for that reason. On a single-control-plane
	# cluster it is also the least forgiving thing in the suite.
	expect_exit 2 "upgrade --detailed-exit-code (the control plane)" \
		"$talman" upgrade --detailed-exit-code

	await 900 "the control plane is running $to_version" settled "$cluster-controlplane-1"
	expect_exit 0 "health after the control plane upgrade" "$talman" health

	step "reset returns the worker to maintenance mode"
	# EPHEMERAL and STATE, and a reboot: the node keeps Talos and loses its
	# config, which is what "maintenance mode" means and what the old default
	# of wiping the whole disk never produced.
	expect_exit 0 "reset --graceful=false" \
		"$talman" reset -n "$cluster-worker-1" --graceful=false --yes

	await 600 "the worker is in maintenance mode" in_maintenance "$cluster-worker-1"

	step "apply adopts it back into the cluster it just left"
	expect_exit 0 "apply -n worker" \
		"$talman" apply -n "$cluster-worker-1" --timeout=10m

	await 600 "the worker is running again" settled "$cluster-worker-1"
	expect_exit 0 "health after the adoption" "$talman" health

	step "and the whole cluster agrees with the config"
	status=$("$talman" status)
	printf '%s\n' "$status"
	absent "$status" "unreachable" "no node is unreachable"
	absent "$status" "→" "nothing is left drifting from the config"

	printf '\n\033[32mall end-to-end checks passed against real machines\033[0m\n'
}

# Sourced rather than run when something wants one of these functions on its
# own -- checking that talman can be found, say, without building a cluster to
# find out.
if [ "${BASH_SOURCE[0]:-$0}" = "$0" ]; then
	case "${1:-}" in
	--install-deps) install_deps ;;
	"") main ;;
	*) fail "usage: $0 [--install-deps]" ;;
	esac
fi
