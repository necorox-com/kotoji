# Backend image for kotojid. Multi-stage: compile with the Go toolchain, ship on
# a small Alpine base that INCLUDES the git binary (git.GitService shells out to
# it for writes — see docs/architecture.md §4.1). Runs as a non-root user.
#
# Build context is the backend/ directory (see docker-compose.yml `context: ../backend`).

# ---- build stage ----
FROM golang:1.25-alpine AS build

# git is needed by the Go toolchain for any VCS-stamped builds; ca-certs for TLS.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Prime the module cache first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Then the source.
COPY . .

# Static build (CGO off) so it runs on the minimal runtime image.
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/kotojid ./cmd/kotojid

# ---- runtime stage ----
FROM alpine:3.21 AS runtime

# git is a RUNTIME dependency of the backend. ca-certs for outbound TLS (OIDC,
# GitHub mirror). tini reaps zombies from any git child processes.
RUN apk add --no-cache git ca-certificates tini \
    && adduser -D -u 10001 -h /home/kotoji kotoji \
    && mkdir -p /data \
    && chown -R kotoji:kotoji /data

COPY --from=build /out/kotojid /usr/local/bin/kotojid

USER kotoji
WORKDIR /home/kotoji

# Control plane :8080, data plane :8081.
EXPOSE 8080 8081

# Make GIT_TERMINAL_PROMPT=0 the default so git never blocks on a prompt.
ENV GIT_TERMINAL_PROMPT=0

ENTRYPOINT ["/sbin/tini", "--", "kotojid"]
