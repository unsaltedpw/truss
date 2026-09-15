# The truss image: the binary, plus exactly the tools it shells out to.
#
# ⚠️ IT IS BUILT AND PUSHED BY CI, NOT BY HAND ON A BOX. The applier's image
# used to be built with podman on the one machine that runs it, which made the
# image the single thing in the system not delivered by merging -- so a config
# change could outrun its runtime and only an SSH session could close the gap.
# On 2026-09-08 that took a consumer's applier down for hours, and the fix for
# it could not be shipped BY the applier. See .github/workflows/release.yml.
#
# ⚠️ WHAT IS IN HERE IS DECIDED BY WHAT TRUSS EXECUTES, and nothing else.
# Verified by grepping every exec.Command in non-test code: `tofu` (plan and
# apply), `git` (clone and read the approved commit), `op` (the publisher's
# 1Password reads). Truss talks to GitHub and to S3 over HTTP in Go, so it
# needs no `gh` and no `aws` -- both of which the hand-built image carried.
#
# ⚠️ NO PROVIDER MIRROR. The old image baked one, built from the CONSUMER's
# terraform config, which is what coupled this image to somebody else's repo
# and made "add a provider" mean "rebuild the applier". Providers belong in a
# plugin cache the consumer owns.
ARG BASE_IMAGE=debian:trixie-slim
FROM ${BASE_IMAGE}

# Set by buildx, one value per --platform. Everything below that differs by
# architecture reads it, so a multi-arch build needs no per-arch Dockerfile
# and no `uname` guessing at runtime.
ARG TARGETARCH

# ⚠️ NO DEFAULT, AND A LITERAL HERE WOULD BE WRONG RATHER THAN MERELY UNTIDY.
# The consumer's CI plans with one OpenTofu version and the applier re-plans
# with this one; if they differ, every plan digest mismatches and every apply
# is refused. The version is part of the image TAG for exactly that reason, so
# a consumer pins the image whose tofu matches the version it plans with.
ARG OPENTOFU_VERSION
RUN test -n "$OPENTOFU_VERSION" || { echo "OPENTOFU_VERSION build-arg is required" >&2; exit 1; }

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl unzip gnupg git \
 && rm -rf /var/lib/apt/lists/*

# OpenTofu, from the release zip rather than an apt repo: one binary, one
# checksum, no extra archive to trust for a single file.
RUN set -eux; \
    curl -fsSL -o /tmp/tofu.zip \
      "https://github.com/opentofu/opentofu/releases/download/v${OPENTOFU_VERSION}/tofu_${OPENTOFU_VERSION}_linux_${TARGETARCH}.zip"; \
    curl -fsSL -o /tmp/tofu.sums \
      "https://github.com/opentofu/opentofu/releases/download/v${OPENTOFU_VERSION}/tofu_${OPENTOFU_VERSION}_SHA256SUMS"; \
    grep " tofu_${OPENTOFU_VERSION}_linux_${TARGETARCH}.zip\$" /tmp/tofu.sums \
      | sed "s|tofu_${OPENTOFU_VERSION}_linux_${TARGETARCH}.zip|/tmp/tofu.zip|" \
      | sha256sum -c -; \
    unzip -q /tmp/tofu.zip -d /usr/local/bin tofu; \
    rm -f /tmp/tofu.zip /tmp/tofu.sums; \
    chmod 0755 /usr/local/bin/tofu

# The 1Password CLI, used only by the publisher. Its apt repo is per-arch, so
# the component below is TARGETARCH rather than a hardcoded amd64 -- which is
# the bug that made the hand-built image unbuildable on an arm64 laptop.
RUN set -eux; \
    curl -fsSL https://downloads.1password.com/linux/keys/1password.asc \
      | gpg --dearmor -o /usr/share/keyrings/1password-archive-keyring.gpg; \
    echo "deb [arch=${TARGETARCH} signed-by=/usr/share/keyrings/1password-archive-keyring.gpg] https://downloads.1password.com/linux/debian/${TARGETARCH} stable main" \
      > /etc/apt/sources.list.d/1password.list; \
    apt-get update; \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends 1password-cli; \
    rm -rf /var/lib/apt/lists/*

# ⚠️ THE BINARY IS BUILT OUTSIDE THIS FILE, still deliberately. Truss has zero
# dependencies, so `CGO_ENABLED=0 go build` produces the identical static
# artefact anywhere -- release.yml builds one per architecture and proves it
# reproduces before publishing. A builder stage here would download the module
# inside every image build for no benefit.
COPY dist/truss-linux-${TARGETARCH} /usr/local/bin/truss
RUN chmod 0755 /usr/local/bin/truss

# Apache-2.0 §4(a): anyone redistributing the binary gives recipients a copy
# of the licence, and this image is a redistribution.
COPY LICENSE /usr/share/licenses/truss/LICENSE

# 10001 matches the `applier` user the platform image used, so a consumer's
# volume ownership does not change under them.
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin applier
WORKDIR /work
RUN chown 10001:10001 /work
USER 10001

# No default subcommand: `truss` with no arguments prints usage and exits 2,
# so a CronJob that forgets its argument fails loudly rather than doing
# something plausible.
ENTRYPOINT ["/usr/local/bin/truss"]
