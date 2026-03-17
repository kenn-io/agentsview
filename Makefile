.DEFAULT_GOAL := help

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.buildDate=$(BUILD_DATE)

LDFLAGS_RELEASE := $(LDFLAGS) -s -w
DESKTOP_DIST_DIR := dist/desktop

.PHONY: build build-release install frontend frontend-dev dev desktop-dev desktop-build desktop-macos-app desktop-macos-dmg desktop-windows-installer desktop-linux-appimage desktop-app test test-short test-postgres test-postgres-ci postgres-up postgres-down e2e vet lint tidy clean release release-darwin-arm64 release-darwin-amd64 release-linux-amd64 install-hooks ensure-embed-dir help

# Ensure go:embed has at least one file (no-op if frontend is built)
ensure-embed-dir:
	@mkdir -p internal/web/dist
	@test -n "$$(ls internal/web/dist/ 2>/dev/null)" \
		|| echo ok > internal/web/dist/stub.html

# Build the binary (debug, with embedded frontend)
build: frontend
	CGO_ENABLED=1 go build -tags fts5 -ldflags="$(LDFLAGS)" -o agentsview ./cmd/agentsview
	@chmod +x agentsview

# Build with optimizations (release)
build-release: frontend
	CGO_ENABLED=1 go build -tags fts5 -ldflags="$(LDFLAGS_RELEASE)" -trimpath -o agentsview ./cmd/agentsview
	@chmod +x agentsview

# Install to ~/.local/bin, $GOBIN, or $GOPATH/bin
install: build-release
	@if [ -d "$(HOME)/.local/bin" ]; then \
		echo "Installing to ~/.local/bin/agentsview"; \
		cp agentsview "$(HOME)/.local/bin/agentsview"; \
	else \
		INSTALL_DIR="$${GOBIN:-$$(go env GOBIN)}"; \
		if [ -z "$$INSTALL_DIR" ]; then \
			GOPATH_FIRST="$$(go env GOPATH | cut -d: -f1)"; \
			INSTALL_DIR="$$GOPATH_FIRST/bin"; \
		fi; \
		mkdir -p "$$INSTALL_DIR"; \
		echo "Installing to $$INSTALL_DIR/agentsview"; \
		cp agentsview "$$INSTALL_DIR/agentsview"; \
	fi

# Build frontend SPA and copy into embed directory
frontend:
	cd frontend && npm install && npm run build
	rm -rf internal/web/dist
	cp -r frontend/dist internal/web/dist

# Run Vite dev server (use alongside `make dev`)
frontend-dev:
	cd frontend && npm run dev

# Run Go server in dev mode (no embedded frontend)
dev: ensure-embed-dir
	go run -tags fts5 -ldflags="$(LDFLAGS)" ./cmd/agentsview $(ARGS)

# Run the Tauri desktop wrapper in development mode
desktop-dev:
	cd desktop && npm install && npm run tauri:dev

# Build desktop app bundles via Tauri
desktop-build:
	cd desktop && npm install && npm run tauri:build

# Build only the macOS .app bundle (skip DMG packaging).
# Skips updater artifact signing when TAURI_SIGNING_PRIVATE_KEY
# is not set so local builds succeed without release keys.
desktop-macos-app:
	cd desktop && npm install && npm run tauri:build:macos-app \
		$(if $(TAURI_SIGNING_PRIVATE_KEY),,-- --config '{"bundle":{"createUpdaterArtifacts":false}}')
	mkdir -p $(DESKTOP_DIST_DIR)/macos
	rm -rf $(DESKTOP_DIST_DIR)/macos/AgentsView.app
	cp -R desktop/src-tauri/target/release/bundle/macos/AgentsView.app \
		$(DESKTOP_DIST_DIR)/macos/AgentsView.app
	@echo "macOS app bundle copied to $(DESKTOP_DIST_DIR)/macos/AgentsView.app"

# Build macOS DMG installer
desktop-macos-dmg:
	cd desktop && npm install && npm run tauri:build:macos-dmg
	mkdir -p $(DESKTOP_DIST_DIR)/macos
	rm -f $(DESKTOP_DIST_DIR)/macos/*.dmg
	@dmg_count=$$(find desktop/src-tauri/target/release/bundle/dmg \
		-maxdepth 1 -type f -name '*.dmg' | wc -l | tr -d ' '); \
	if [ "$$dmg_count" -eq 0 ]; then \
		echo "error: no DMG installer found in bundle output" >&2; \
		exit 1; \
	fi; \
	find desktop/src-tauri/target/release/bundle/dmg \
		-maxdepth 1 -type f -name '*.dmg' \
		-exec cp {} $(DESKTOP_DIST_DIR)/macos/ \;; \
	echo "Copied $$dmg_count DMG installer(s) to $(DESKTOP_DIST_DIR)/macos/"

# Build Windows NSIS installer bundle (.exe)
# Run on Windows runner/host.
desktop-windows-installer:
	cd desktop && npm install && npm run tauri:build:windows
	mkdir -p $(DESKTOP_DIST_DIR)/windows
	rm -f $(DESKTOP_DIST_DIR)/windows/*.exe
	@exe_count=$$(find desktop/src-tauri/target/release/bundle/nsis \
		-maxdepth 1 -type f -name '*.exe' | wc -l | tr -d ' '); \
	if [ "$$exe_count" -eq 0 ]; then \
		echo "error: no Windows installer (.exe) found in bundle output" >&2; \
		exit 1; \
	fi; \
	find desktop/src-tauri/target/release/bundle/nsis \
		-maxdepth 1 -type f -name '*.exe' \
		-exec cp {} $(DESKTOP_DIST_DIR)/windows/ \;; \
	echo "Copied $$exe_count Windows installer(s) to $(DESKTOP_DIST_DIR)/windows/"

# Build Linux AppImage bundle
# Run on a Linux host.
desktop-linux-appimage:
	cd desktop && npm install && npm run tauri:build:linux \
		$(if $(TAURI_SIGNING_PRIVATE_KEY),,-- --config '{"bundle":{"createUpdaterArtifacts":false}}')
	mkdir -p $(DESKTOP_DIST_DIR)/linux
	rm -f $(DESKTOP_DIST_DIR)/linux/*.AppImage
	@ai_count=$$(find desktop/src-tauri/target/release/bundle/appimage \
		-maxdepth 1 -type f -name '*.AppImage' | wc -l | tr -d ' '); \
	if [ "$$ai_count" -eq 0 ]; then \
		echo "error: no AppImage found in bundle output" >&2; \
		exit 1; \
	fi; \
	find desktop/src-tauri/target/release/bundle/appimage \
		-maxdepth 1 -type f -name '*.AppImage' \
		-exec cp {} $(DESKTOP_DIST_DIR)/linux/ \;; \
	echo "Copied $$ai_count AppImage(s) to $(DESKTOP_DIST_DIR)/linux/"

# Backward-compatible alias (macOS .app)
desktop-app: desktop-macos-app

# Run tests
test: ensure-embed-dir
	go test -tags fts5 ./... -v -count=1

# Run fast tests only
test-short: ensure-embed-dir
	go test -tags fts5 ./... -short -count=1

# Start test PostgreSQL container
postgres-up:
	docker compose -f docker-compose.test.yml up -d --wait

# Stop test PostgreSQL container
postgres-down:
	docker compose -f docker-compose.test.yml down

# Run PostgreSQL integration tests (starts postgres automatically)
test-postgres: ensure-embed-dir postgres-up
	@echo "Waiting for postgres to be ready..."
	@sleep 2
	TEST_PG_URL="postgres://agentsview_test:agentsview_test_password@localhost:5433/agentsview_test?sslmode=disable" \
		CGO_ENABLED=1 go test -tags "fts5,pgtest" -v ./internal/postgres/... -count=1

# PostgreSQL integration tests for CI (postgres already running as service)
test-postgres-ci: ensure-embed-dir
	CGO_ENABLED=1 go test -tags "fts5,pgtest" -v ./internal/postgres/... -count=1

# Run Playwright E2E tests
e2e:
	cd frontend && npx playwright test

# Vet
vet: ensure-embed-dir
	go vet -tags fts5 ./...

# Lint Go code with project defaults
lint: ensure-embed-dir
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1" >&2; \
		exit 1; \
	fi
	golangci-lint run ./...

# Tidy dependencies
tidy:
	go mod tidy

# Clean build artifacts
clean:
	rm -f agentsview agentsv
	rm -rf internal/web/dist dist/

# Build release binary for current platform (CGO required for sqlite3)
release: frontend
	mkdir -p dist
	CGO_ENABLED=1 go build -tags fts5 \
		-ldflags="$(LDFLAGS_RELEASE)" -trimpath \
		-o dist/agentsview-$$(go env GOOS)-$$(go env GOARCH) ./cmd/agentsview

# Cross-compile targets (require CC set to target cross-compiler)
release-darwin-arm64: frontend
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 go build -tags fts5 \
		-ldflags="$(LDFLAGS_RELEASE)" -trimpath \
		-o dist/agentsview-darwin-arm64 ./cmd/agentsview

release-darwin-amd64: frontend
	mkdir -p dist
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 go build -tags fts5 \
		-ldflags="$(LDFLAGS_RELEASE)" -trimpath \
		-o dist/agentsview-darwin-amd64 ./cmd/agentsview

release-linux-amd64: frontend
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -tags fts5 \
		-ldflags="$(LDFLAGS_RELEASE)" -trimpath \
		-o dist/agentsview-linux-amd64 ./cmd/agentsview

# Install pre-commit hook, resolving the hooks directory via git so
# this works in both normal repos and linked worktrees
install-hooks:
	@hooks_rel=$$(git rev-parse --git-path hooks) && \
		hooks_dir=$$(cd "$$(dirname "$$hooks_rel")" && echo "$$PWD/$$(basename "$$hooks_rel")") && \
		git config --local core.hooksPath "$$hooks_dir" && \
		mkdir -p "$$hooks_dir" && \
		cp .githooks/pre-commit "$$hooks_dir/pre-commit" && \
		chmod +x "$$hooks_dir/pre-commit" && \
		echo "Installed pre-commit hook to $$hooks_dir/pre-commit"

# Show help
help:
	@echo "agentsview build targets:"
	@echo ""
	@echo "  build          - Build with embedded frontend"
	@echo "  build-release  - Release build (optimized, stripped)"
	@echo "  install        - Build and install to ~/.local/bin or GOPATH"
	@echo ""
	@echo "  dev            - Run Go server (use with frontend-dev)"
	@echo "  frontend       - Build frontend SPA"
	@echo "  frontend-dev   - Run Vite dev server"
	@echo "  desktop-dev    - Run Tauri desktop wrapper in dev mode"
	@echo "  desktop-build  - Build Tauri desktop app bundles"
	@echo "  desktop-macos-app - Build macOS .app bundle only"
	@echo "  desktop-macos-dmg - Build macOS DMG installer"
	@echo "  desktop-windows-installer - Build Windows NSIS installer"
	@echo "  desktop-linux-appimage - Build Linux AppImage"
	@echo "  desktop-app    - Alias for desktop-macos-app"
	@echo ""
	@echo "  test           - Run all tests"
	@echo "  test-short     - Run fast tests only"
	@echo "  test-postgres  - Run PostgreSQL integration tests"
	@echo "  postgres-up    - Start test PostgreSQL container"
	@echo "  postgres-down  - Stop test PostgreSQL container"
	@echo "  e2e            - Run Playwright E2E tests"
	@echo "  vet            - Run go vet"
	@echo "  lint           - Run golangci-lint"
	@echo "  tidy           - Tidy go.mod"
	@echo ""
	@echo "  release        - Release build for current platform"
	@echo "  clean          - Remove build artifacts"
	@echo "  install-hooks  - Install pre-commit git hooks"
