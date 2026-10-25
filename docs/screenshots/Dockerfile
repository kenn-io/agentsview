# Environment preparation only. Applications and corpora are mounted per run.
ARG GO_IMAGE=golang@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG NODE_IMAGE=node@sha256:6449e19d8184d84062525b6a7524def03146c2cb04a7f08b076680d0e8e4240a
ARG PYTHON_IMAGE=ghcr.io/astral-sh/uv@sha256:531f855bda2c73cd6ef67d56b733b357cea384185b3022bd09f05e002cd144ca
FROM ${GO_IMAGE} AS go
FROM ${NODE_IMAGE} AS node

FROM ${PYTHON_IMAGE} AS builder
ARG DEBIAN_SNAPSHOT=20261023T000000Z
COPY --from=go /usr/local/go /usr/local/go
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
ENV PATH="/usr/local/go/bin:/usr/local/bin:${PATH}" GOTOOLCHAIN=local
COPY snapshot-apt.sh /snapshot-apt.sh
RUN bash /snapshot-apt.sh "$DEBIAN_SNAPSHOT" && \
    apt-get install -y --no-install-recommends gcc g++ make git curl ca-certificates libsqlite3-dev && \
    ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx
WORKDIR /src

FROM ${NODE_IMAGE} AS runtime
ARG DEBIAN_SNAPSHOT=20261023T000000Z
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/browsers
COPY snapshot-apt.sh /snapshot-apt.sh
RUN bash /snapshot-apt.sh "$DEBIAN_SNAPSHOT" && \
    apt-get install -y --no-install-recommends postgresql postgresql-client curl python3
WORKDIR /opt/playwright
COPY package.json package-lock.json ./
RUN npm ci && npx playwright install --with-deps chromium && \
    sha256sum package-lock.json > /opt/playwright/lock.sha256
RUN PG_HBA=$(find /etc/postgresql -name pg_hba.conf -print -quit) && \
    test -n "$PG_HBA" && \
    printf 'local all all trust\nhost all all 127.0.0.1/32 trust\nhost all all ::1/128 trust\n' > "$PG_HBA"
WORKDIR /work
