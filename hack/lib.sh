# Shared plumbing for the end-to-end scripts.
#
# Sourced, not run: each script sets up a cluster of its own and then drives
# talman at it, and only the shape of the assertions is common.

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
fail() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
pass() { printf '  \033[32mok\033[0m: %s\n' "$*"; }

# expect_exit <want> <label> <command...>
expect_exit() {
	local want=$1 label=$2
	shift 2

	local got=0
	"$@" || got=$?

	[ "$got" = "$want" ] || fail "$label: exit $got, want $want"
	pass "$label (exit $want)"
}

# contains <haystack> <needle> <label>
contains() {
	grep -q -- "$2" <<<"$1" || fail "$3"
	pass "$3"
}

# absent <haystack> <needle> <label>
absent() {
	grep -q -- "$2" <<<"$1" && fail "$3"
	pass "$3"
}

# await <seconds> <label> <command...> -- retries until the command succeeds.
#
# Nodes reboot, install and rejoin on their own schedule, and a test that
# asserts immediately after asking for any of those is testing the speed of the
# hardware rather than talman.
await() {
	local timeout=$1 label=$2
	shift 2

	local deadline=$((SECONDS + timeout))

	until "$@"; do
		if [ "$SECONDS" -ge "$deadline" ]; then
			fail "$label: still not true after ${timeout}s"
		fi

		sleep 10
	done

	pass "$label"
}
