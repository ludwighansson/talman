# Contributing

Pull requests are welcome, and any help at all is genuinely appreciated.

That includes the kinds that never touch code. A bug report with a config that
reproduces it, a note that an error message sent you down the wrong path, a
correction to a sentence in the README that reads clearly to me and confusingly
to everyone else — all of that is real contribution, and often more valuable
than a patch, because it tells me something I could not have noticed alone.

If you are unsure whether something is worth raising: it is. Open the issue.

Everyone taking part is expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md). It is the Contributor Covenant, and it
amounts to the obvious: assume good faith, take criticism of your work without
taking it personally, and give it the same way.

## Getting set up

You need Go (the version in `go.mod`),
[`talosctl`](https://docs.siderolabs.com/talos/v1.14/talosctl) at least as new
as the Talos version you are testing against, and
[`sops`](https://github.com/getsops/sops).

```console
$ go build ./...
$ go test ./...
```

The tests in `internal/render` shell out to `talosctl`, and those in
`internal/sopsx` to `sops`; both skip themselves if the binary is missing.
They are the ones that actually prove talman works, so please have both
installed rather than trusting a green run without them.

To exercise the whole thing by hand:

```console
$ cp -a example/. /tmp/try && cd /tmp/try
$ age-keygen -o key.txt                 # then put the public key in .sops.yaml
$ cd development
$ SOPS_AGE_KEY_FILE=../key.txt talman secrets generate
$ SOPS_AGE_KEY_FILE=../key.txt talman render
```

## The one hard rule

**talman must never import the Talos API.** No
`github.com/siderolabs/talos/pkg/machinery`, and nothing that depends on it.

This is the entire premise of the project rather than a stylistic preference.
talhelper embedded upstream `v1alpha1` structs in its own schema and hand-wrote
a generator for each new Talos document kind, so every Talos minor release
meant a talhelper release, and eventually meant no talhelper at all. Everything
Talos-shaped goes through the `talosctl` binary, which means a new Talos
release needs no talman release.

There is exactly one place this bites: `internal/factory` re-declares the Image
Factory schematic type instead of importing `image-factory/pkg/schematic`,
which would pull machinery in for the sake of one string enum. If you touch
that file, note that **field order is a wire format** — the schematic ID is the
sha256 of the marshalled struct, so reordering a field silently changes every
installer image URL in every cluster. `schematic_test.go` pins the IDs against
upstream; if those tests fail, the struct is wrong, not the tests.

## Design principles

Worth knowing before proposing a feature, since these are the reasons most
likely to make me push back on an otherwise good PR.

**The node schema stays tiny.** A node carries only what talman needs to route
patches and reach the host. It deliberately has no `installDisk`, no
`nodeLabels`, no `networkInterfaces` — those are Talos' shapes, and mirroring
them is the mistake described above. If something feels missing, the answer is
usually a templated patch reading `.Node.Values`, not a new field.

**Patches are addressed by path.** No directory conventions, no globs, no
implicit discovery. A patch key must name a real group and a patch value must
resolve to a real file, both enforced at load time. The point is that
`talman patches` can be the complete and authoritative answer to "why does this
node have that value".

**Errors name the file and say the fix.** A good part of talman's value over
calling `talosctl` yourself is that its failures are actionable. `talosctl`
reports a rejected patch by naming the offending *document*, so talman re-runs
generation with growing prefixes of the chain to work out which of your files
is responsible. When you add an error path, compare it against that bar: which
file, and what should the reader do now?

**Configuration is explicit over clever.** Unknown keys are a hard error.
Missing template keys are a hard error. In a tool whose job is routing patch
files, something silently ignored means a machine config quietly missing a
patch, which is worse than a failed run.

## Conventions

- `gofmt` and `go vet` must be clean; CI fails on either.
- Tests run with `-race`.
- Comments explain *why*, not what. The code says what it does; a comment earns
  its place by recording the reasoning or constraint that is not visible from
  reading it.
- Commit messages follow [Conventional
  Commits](https://www.conventionalcommits.org/) — `feat:`, `fix:`, `docs:`,
  `chore:`. goreleaser builds the release changelog from them, so `feat:` and
  `fix:` become the user-facing notes.
- If you change behaviour, update the README in the same commit. There are
  tests in `internal/template/docs_test.go` that check the documented template
  scopes actually exist, but they cannot tell whether a sentence became untrue.

## Opening a pull request

Branch, commit, open it. A draft PR with a rough idea is a perfectly good way
to start a conversation — you do not need it finished, or even working, to ask
whether an approach is worth pursuing.

CI runs the formatting and vet checks, the test suite on Linux and macOS,
renders `example/` end to end, and cross-compiles every released target. If
something fails there and the reason is not obvious, say so in the PR rather
than fighting it alone.

Small, focused commits are easier to review than one large one, but I would
much rather have your contribution in an awkward shape than not at all. I am
happy to help clean it up.

## Reporting a bug

The most useful report includes:

- the output of `talman version` (it reports the `talosctl` version too, which
  matters more often than you would expect)
- the smallest `talman.yaml` and patch files that reproduce it
- what you expected, and what happened

Please redact hostnames, addresses and anything else that identifies your
infrastructure. talman configs tend to describe real clusters, and no bug
report needs that detail to be useful.

## Security

If you find something with security impact, please report it privately through
GitHub's security advisory tab rather than in a public issue, and I will
respond as quickly as I am able.

## License

talman is MIT licensed. By contributing you agree that your contribution is
licensed under the same terms.
