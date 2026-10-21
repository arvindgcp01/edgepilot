# EdgePilot server image.
#
# Multi-stage, and the final stage is `scratch` plus a CA bundle. The server is a
# static Go binary that speaks TLS to PostgreSQL, Redis, and Azure Blob; it needs
# root certificates and a passwd entry, and nothing else. A distro base would add
# a package manager, a shell, and a monthly CVE feed to the process that holds the
# device certificate authority's signing key.

FROM golang:1.26-alpine AS build

# git is needed for the commit stamp; the rest of the toolchain is in the image.
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# The workspace and every module's manifest first, so a change to source does not
# invalidate the dependency layer.
COPY go.work go.work.sum* ./
COPY apps/server/go.mod apps/server/go.sum apps/server/
COPY apps/agent/go.mod apps/agent/go.sum apps/agent/
COPY packages/proto/go.mod packages/proto/go.sum packages/proto/
RUN go mod download all

COPY . .

ARG VERSION=dev
ARG COMMIT=none

# CGO off: a static binary is what makes the scratch stage possible.
# -trimpath so the build is reproducible and the image carries no build paths.
RUN --mount=type=cache,target=/root/.cache/go-build \
    cd apps/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/edgepilot-server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/epctl ./cmd/epctl

# ---------------------------------------------------------------------------

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/edgepilot-server /usr/local/bin/edgepilot-server
COPY --from=build /out/epctl /usr/local/bin/epctl

# A non-root user, declared numerically because scratch has no /etc/passwd to
# resolve a name against. Nothing here needs root: the ports are above 1024 and
# every path the server writes is a mounted volume.
USER 65532:65532

EXPOSE 8080 9443 9090

ENTRYPOINT ["/usr/local/bin/edgepilot-server"]
