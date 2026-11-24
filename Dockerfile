# syntax=docker/dockerfile:1.7

FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
ARG SUBROUTER_BUILD_VERSION=devel
ARG SUBROUTER_BUILD_COMMIT=unknown
ARG SUBROUTER_BUILD_DATE=unknown
ARG SUBROUTER_BUILD_MAINLINE=local-build
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/discordwell/subrouter/internal/buildversion.version=${SUBROUTER_BUILD_VERSION} -X github.com/discordwell/subrouter/internal/buildversion.commit=${SUBROUTER_BUILD_COMMIT} -X github.com/discordwell/subrouter/internal/buildversion.buildDate=${SUBROUTER_BUILD_DATE} -X github.com/discordwell/subrouter/internal/buildversion.mainline=${SUBROUTER_BUILD_MAINLINE}" -o /out/subrouter ./cmd/subrouter && \
    mkdir -p /out/state

# Test harness stage: `deploy/docker/test.sh` (or `make docker-test`) runs the
# suite in here with the repository bind-mounted read-only at /src, so agents
# can iterate on this codebase with zero reach into the host's live agent
# configs. Every config surface an agent framework resolves at runtime is
# pinned under the throwaway /home/agent, never a developer's real home.
FROM golang:1.26-bookworm AS test

ENV HOME=/home/agent \
    CLAUDE_CONFIG_DIR=/home/agent/.claude \
    CODEX_HOME=/home/agent/.codex \
    SUBROUTER_STATE_DIR=/home/agent/.subrouter \
    SUBROUTER_CLOUD_CONFIG=/home/agent/.config/subrouter/cloud.json \
    SUBROUTER_DOCKER_TEST_HARNESS=1 \
    GOCACHE=/root/.cache/go-build \
    GOTOOLCHAIN=local

RUN mkdir -p /home/agent/.claude \
    /home/agent/.codex \
    /home/agent/.subrouter \
    /home/agent/.config/subrouter

# The board page tests in internal/proxy run its script in node. With
# SUBROUTER_TEST_REQUIRE_NODE=1 they fail when node is missing instead of
# skipping, which is how they once went unrun in this image.
COPY --from=node:26.13.1-bookworm-slim /usr/local/bin/node /usr/local/bin/node
ENV SUBROUTER_TEST_REQUIRE_NODE=1

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# The env -u wrapper mirrors deploy/docker/test.sh: many existing tests
# isolate themselves with t.Setenv("HOME") alone, and an exported override for
# a HOME-derived path defeats that isolation (27 spurious failures). Clearing
# the three here keeps a bare `docker run` of this stage consistent with the
# script; every path still resolves under /home/agent either way.
CMD ["env", "-u", "SUBROUTER_STATE_DIR", "-u", "SUBROUTER_CLOUD_CONFIG", "-u", "CODEX_HOME", "go", "test", "./..."]

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/subrouter /usr/local/bin/subrouter
COPY --from=build --chown=65532:65532 /out/state /var/lib/subrouter

ENV HOME=/var/lib/subrouter \
    SUBROUTER_STATE_DIR=/var/lib/subrouter \
    SUBROUTER_PROXY_TOKEN_FILE=/run/secrets/proxy_token \
    SUBROUTER_ADMIN_TOKEN_FILE=/run/secrets/admin_token \
    SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE=/run/secrets/account_import_token \
    GOMEMLIMIT=192MiB \
    GOMAXPROCS=2

USER 65532:65532
EXPOSE 31415
VOLUME ["/var/lib/subrouter"]
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/subrouter", "probe", "--url", "http://127.0.0.1:31415"]
ENTRYPOINT ["/usr/local/bin/subrouter"]
CMD ["serve", "--addr", "0.0.0.0:31415", "--sessions", "/var/lib/subrouter/sessions.json"]
