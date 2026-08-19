# syntax=docker/dockerfile:1

# Build with the owned Go builder (override BUILDER_IMAGE in CI to pin a tag).
ARG BUILDER_IMAGE=ghcr.io/bradfordwagner/go-builder:latest
# Final base image. The CI matrix sets this to "scratch" or "alpine:3.xx".
ARG BASE_IMAGE=scratch

# --- build stage -----------------------------------------------------------
FROM ${BUILDER_IMAGE} AS build
WORKDIR /src
# CGO off for a static binary; GOTOOLCHAIN=auto fetches the go.mod toolchain if
# the builder ships a different minor.
ENV CGO_ENABLED=0 GOTOOLCHAIN=auto
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/vault-plugin-manager ./cmd/vault-plugin-manager

# --- CA certificates -------------------------------------------------------
# Provides an up-to-date CA bundle for the scratch variant (and refreshes the
# alpine variant), so outbound TLS (e.g. to Vault) works.
FROM alpine:3.22 AS certs
RUN apk add --no-cache ca-certificates

# --- final image -----------------------------------------------------------
FROM ${BASE_IMAGE}
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/vault-plugin-manager /usr/local/bin/vault-plugin-manager
# Non-root numeric uid so Kubernetes runAsNonRoot is satisfied on scratch too.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/vault-plugin-manager"]
CMD ["serve"]
