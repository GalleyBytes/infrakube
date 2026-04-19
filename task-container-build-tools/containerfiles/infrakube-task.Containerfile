# infrakube-task.Containerfile
#
# Unified task image for infrakube. Combines setup, terraform, and tofu task capabilities.
# Terraform and OpenTofu are downloaded on demand at runtime and then cached by the controller.

# ---------------------------------------------------------------------------
# Stage: Kubernetes CLI
# ---------------------------------------------------------------------------
FROM docker.io/library/alpine:3.18 AS k8s
ARG TARGETARCH
RUN apk add --no-cache curl && \
    KUBECTL_VERSION=$(curl -sSL https://dl.k8s.io/release/stable.txt) && \
    curl -sSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" -o /usr/local/bin/kubectl && \
    chmod +x /usr/local/bin/kubectl

# ---------------------------------------------------------------------------
# Stage: IRSA Token Generator
# ---------------------------------------------------------------------------
FROM docker.io/library/alpine:3.18 AS irsa-tokengen
ARG TARGETARCH
RUN apk add --no-cache wget && \
    wget -q "https://github.com/isaaguilar/irsa-tokengen/releases/download/v1.0.0/irsa-tokengen-v1.0.0-linux-${TARGETARCH}.tgz" && \
    tar xzf "irsa-tokengen-v1.0.0-linux-${TARGETARCH}.tgz" && \
    mv irsa-tokengen /usr/local/bin/irsa-tokengen && \
    chmod +x /usr/local/bin/irsa-tokengen

# ---------------------------------------------------------------------------
# Stage: Compile Rust entrypoint
# ---------------------------------------------------------------------------
FROM docker.io/library/rust:alpine AS entrypoint
RUN apk add --no-cache musl-dev
WORKDIR /workdir
COPY scripts/entrypoint/Cargo.toml scripts/entrypoint/Cargo.lock /workdir/
RUN mkdir -p /workdir/src && printf 'fn main() {}\n' > /workdir/src/main.rs
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/workdir/target \
    cargo build --release --locked
COPY scripts/entrypoint/src /workdir/src
# Touch source files so the real entrypoint rebuilds after the warm-cache dummy main.rs build.
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/workdir/target \
    find /workdir/src -type f -exec touch {} + && \
    cargo build --release --locked && cp target/release/entrypoint /workdir/entrypoint

# ---------------------------------------------------------------------------
# Final runtime image
# ---------------------------------------------------------------------------
FROM docker.io/library/alpine:3.18

RUN apk add --no-cache bash jq git openssh curl gettext xz file unzip

# Copy tools
COPY --from=k8s /usr/local/bin/kubectl /usr/local/bin/kubectl
COPY --from=irsa-tokengen /usr/local/bin/irsa-tokengen /usr/local/bin/irsa-tokengen
COPY --from=entrypoint /workdir/entrypoint /usr/local/bin/entrypoint-bin

# Copy extraction and wrapper scripts
COPY scripts/extract-terraform.sh /opt/terraform/extract-terraform.sh
COPY scripts/extract-tofu.sh /opt/tofu/extract-tofu.sh
COPY scripts/entrypoint-wrapper.sh /usr/local/bin/entrypoint
RUN chmod +x /opt/terraform/extract-terraform.sh /opt/tofu/extract-tofu.sh /usr/local/bin/entrypoint

# Prepare extraction directories (writable by runtime user)
RUN mkdir -p /opt/terraform/bin /opt/tofu/bin && chmod 777 /opt/terraform/bin /opt/tofu/bin
ENV PATH="/opt/tofu/bin:/opt/terraform/bin:${PATH}"

# User setup
ENV USER_UID=2000 \
    USER_NAME=infrakube-runner \
    HOME=/home/infrakube-runner
COPY scripts/usersetup /usersetup
RUN /usersetup

USER 2000
ENTRYPOINT ["/usr/local/bin/entrypoint"]
LABEL org.opencontainers.image.source=https://github.com/galleybytes/infrakube
