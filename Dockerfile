# syntax=docker/dockerfile:1

# talman drives talosctl and sops rather than linking them, so an image with
# only talman in it can lint a config and nothing else: every command that
# touches a cluster fails at "talosctl not found on PATH".
#
# Both come from their own projects' images rather than being curled into a
# build stage, so they arrive with the provenance their publishers gave them
# and this file has nothing to verify.
ARG TALOSCTL_VERSION=v1.14.0
ARG SOPS_VERSION=v3.13.3

FROM ghcr.io/siderolabs/talosctl:${TALOSCTL_VERSION} AS talosctl
FROM ghcr.io/getsops/sops:${SOPS_VERSION} AS sops

# Alpine rather than distroless, for the shell. GitLab CI runs a job's script
# through the image's shell and GitHub Actions runs `run:` steps the same way,
# so a shell is what makes this usable as a CI job image rather than only as
# `docker run`. It also makes the thing debuggable during an incident, which is
# when a cluster toolbox earns its keep.
#
# It costs about 17 MB against distroless/static, on an image where talosctl
# alone is 117 MB. All three binaries are static Go, so musl never enters into
# it.
FROM alpine:3.22

# For the Image Factory, and for any KMS sops talks to.
RUN apk add --no-cache ca-certificates \
    && adduser -D -u 65532 talman

COPY --from=talosctl /talosctl /usr/local/bin/talosctl
COPY --from=sops /usr/local/bin/sops /usr/local/bin/sops

# goreleaser stages the binaries by platform; buildx sets TARGETPLATFORM.
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/talman /usr/local/bin/talman

USER talman

# Rendering decrypts the secrets bundle into a temp directory. Run the
# container read-only with --tmpfs /tmp and the plaintext never reaches a disk.
ENV TMPDIR=/tmp

# Where the cluster directory is mounted: talman.yaml, the patches, and the
# output directory talman writes.
WORKDIR /cluster

ENTRYPOINT ["/usr/local/bin/talman"]
