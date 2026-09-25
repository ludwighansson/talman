# Security policy

talman handles a cluster's most sensitive material: it decrypts the secrets
bundle holding the machine CA and the cluster's identity, renders machine
configs carrying them, and reaches clusters with admin credentials. Reports
about any of that are very welcome.

## Reporting a vulnerability

Please report privately, through GitHub's
[private vulnerability reporting](https://github.com/ludwighansson/talman/security/advisories/new),
rather than in a public issue. Include what you ran, what happened, and what
you expected; a config that reproduces it helps most.

## Scope

In scope, for example:

- plaintext secrets reaching disk outside talman's private temporary
  directory, or surviving the run
- rendered configs or credentials written with permissions wider than `0600`,
  or outside the gitignored output directory
- secrets printed by `--diff` or `--dry-run` while redaction is on
- a release artefact whose signature or bill of materials does not hold up

Out of scope: vulnerabilities in `talosctl`, `sops` or Talos itself, which
belong with [Sidero Labs](https://github.com/siderolabs/talos/security) and
[SOPS](https://github.com/getsops/sops/security); and anything that requires
an attacker who can already edit your cluster directory, since that directory
is trusted input by design.

Release artefacts are signed; verifying them is described under
[Install](README.md#install).
