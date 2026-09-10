# example

A two-directory layout: shared patches in `base/`, one directory per cluster.

First put your own age recipient in `.sops.yaml` -- the one committed here is
a placeholder.

```console
$ cd development
$ talman validate          # checks paths, keys and templates -- no secrets needed
$ talman patches           # what each node gets, in application order
$ talman secrets generate  # once per cluster
$ talman render            # -> clusterconfig/
```

Note the `storage` group on `development-worker-01`: it is declared but has no
`patches.storage` entry, which is fine. The reverse is not -- a `patches` key
naming no declared group is an error, so renaming a group cannot silently
orphan its patches.
