FROM --platform=$BUILDPLATFORM node:24-bookworm AS frontend-build

WORKDIR /src/frontend

COPY frontend/package.json frontend/package-lock.json ./

# @kenn-io/kit-ui is a private GitHub git dependency
# (github:kenn-io/kit-ui#<commit> in frontend/package.json), so npm ci needs
# git credentials for kenn-io/kit-ui. Pass a token with contents:read as a
# BuildKit secret so it never lands in an image layer:
#   docker buildx build --secret id=kit_ui_token,env=KIT_UI_TOKEN .
# The GIT_CONFIG_* variables rewrite only that repository's ssh URL to
# token-authenticated HTTPS for this single RUN.
RUN --mount=type=secret,id=kit_ui_token \
    GIT_CONFIG_COUNT=2 \
    GIT_CONFIG_KEY_0="url.https://x-access-token:$(cat /run/secrets/kit_ui_token)@github.com/kenn-io/kit-ui.insteadOf" \
    GIT_CONFIG_VALUE_0="ssh://git@github.com/kenn-io/kit-ui" \
    GIT_CONFIG_KEY_1="url.https://x-access-token:$(cat /run/secrets/kit_ui_token)@github.com/kenn-io/kit-ui.insteadOf" \
    GIT_CONFIG_VALUE_1="git@github.com:kenn-io/kit-ui" \
    npm ci

COPY frontend/ ./
RUN npm run build

FROM golang:1.26.3-bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
COPY --from=frontend-build /src/frontend/dist ./internal/web/dist

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=

RUN go run ./internal/pricing/cmd/litellm-snapshot -restore

RUN CGO_ENABLED=1 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -tags fts5 -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/agentsview ./cmd/agentsview

RUN /out/agentsview --version

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /data /agents

ENV AGENTSVIEW_DATA_DIR=/data

COPY --from=build /out/agentsview /usr/local/bin/agentsview
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod +x /usr/local/bin/agentsview /usr/local/bin/docker-entrypoint.sh

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["--host", "0.0.0.0", "--no-browser"]
