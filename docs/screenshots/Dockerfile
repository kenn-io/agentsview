# Stage 1: Build agentsview binary
FROM golang:1.26-bookworm AS builder

ARG AV_VERSION=dev
ARG AV_COMMIT=unknown

# Download the nodesource setup script to a file before running
# it. `curl | bash` hides curl failures (bash exits 0 on empty
# stdin) and the resulting layer gets cached as a "success",
# leaving apt-get to install Debian's stock nodejs without npm.
RUN apt-get update && \
    apt-get install -y curl ca-certificates && \
    curl -fsSL https://deb.nodesource.com/setup_22.x \
      -o /tmp/nodesource_setup.sh && \
    bash /tmp/nodesource_setup.sh && \
    apt-get install -y nodejs && \
    rm /tmp/nodesource_setup.sh

WORKDIR /src
COPY agentsview/ .

# Build frontend and Go binary. Pass version info resolved on
# the host (the .git dir is excluded from the build context).
RUN cd frontend && npm ci
RUN make build VERSION="$AV_VERSION" COMMIT="$AV_COMMIT"
RUN cp agentsview /bin/agentsview

# Stage 2: Run Playwright screenshots
FROM node:22-bookworm

WORKDIR /work

# Install PostgreSQL, curl (for health checks), and Playwright
RUN apt-get update && \
    apt-get install -y postgresql postgresql-client curl && \
    rm -rf /var/lib/apt/lists/*

# Allow local TCP connections without password (trust auth).
# The cluster is auto-created by the Debian package. Write
# a trust-only pg_hba.conf for the auto-created cluster.
RUN PG_HBA=$(find /etc/postgresql -name pg_hba.conf | head -1) && \
    test -n "$PG_HBA" && \
    printf 'local all all trust\nhost all all 127.0.0.1/32 trust\nhost all all ::1/128 trust\n' > "$PG_HBA"

COPY screenshots/package.json screenshots/package-lock.json ./
RUN npm ci
RUN npx playwright install --with-deps chromium

# Copy agentsview binary and test database
COPY --from=builder /bin/agentsview /usr/local/bin/agentsview
COPY test-sessions.db /data/sessions.db

# Copy test files
COPY screenshots/playwright.config.ts .
COPY screenshots/tests/ ./tests/
COPY screenshots/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
