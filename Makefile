.PHONY: help bootstrap tidy build plugins install proto proto-lint proto-check-drift \
	test test-cover test-conformance test-flake-watch lint-imports lint-go lint-baseline-check lint validate example-plugin bench docker-runtime docker-runtime-smoke ci clean

# Default target: list available targets.
help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap: ## Install / sync Go workspace dependencies
	go work sync

tidy: ## Run go mod tidy across all modules
	go mod tidy
	cd sdk      && go mod tidy
	cd workflow && go mod tidy

build: ## Build the criteria binary (output: bin/criteria)
	mkdir -p bin
	go build -o bin/criteria ./cmd/criteria

plugins: ## Build adapter plugin binaries (output: bin/criteria-adapter-*)
	mkdir -p bin
	@for d in ./cmd/criteria-adapter-*; do \
		if [ -d "$$d" ]; then \
			name=$${d##*/}; \
			go build -o bin/$$name $$d; \
		fi; \
	done

install: build plugins ## Install criteria to ~/.criteria (binary → ~/.criteria/bin, plugins → ~/.criteria/plugins)
	@install -d "$$HOME/.criteria/bin" "$$HOME/.criteria/plugins"
	@install -m 755 bin/criteria "$$HOME/.criteria/bin/criteria"
	@for f in bin/criteria-adapter-*; do \
		[ -f "$$f" ] && install -m 755 "$$f" "$$HOME/.criteria/plugins/"; \
	done
	@echo ""
	@echo "criteria installed to $$HOME/.criteria"
	@echo ""
	@echo "Add the following to your shell config to use it:"
	@echo ""
	@echo "  bash  (~/.bashrc or ~/.bash_profile):"
	@echo '    export PATH="$$HOME/.criteria/bin:$$PATH"'
	@echo '    export CRITERIA_PLUGINS="$$HOME/.criteria/plugins"'
	@echo ""
	@echo "  zsh   (~/.zshrc):"
	@echo '    export PATH="$$HOME/.criteria/bin:$$PATH"'
	@echo '    export CRITERIA_PLUGINS="$$HOME/.criteria/plugins"'
	@echo ""
	@echo "  fish  (~/.config/fish/config.fish):"
	@echo '    fish_add_path $$HOME/.criteria/bin'
	@echo '    set -gx CRITERIA_PLUGINS $$HOME/.criteria/plugins'
	@echo ""

docker-runtime-smoke: docker-runtime ## Run a workflow inside the runtime image
	docker build -t criteria/runtime:dev -f Dockerfile.runtime .
	docker run --rm -v "$$PWD/examples:/workspace/examples:ro" \
		criteria/runtime:dev apply /workspace/examples/hello.hcl

proto: ## Regenerate Go bindings from proto files (requires buf)
	buf generate
	@echo "Generated SDK proto bindings."

proto-lint: ## Lint proto files
	buf lint

proto-check-drift: ## Fail if generated proto code is out of sync with proto sources
	buf generate
	@git diff --exit-code sdk/pb/ || \
		(echo "ERROR: Generated proto files are out of sync. Run 'make proto' and commit."; exit 1)

test: ## Run all unit tests
	go test -race ./...
	cd sdk      && go test -race ./...
	cd workflow && go test -race ./...

test-cover: ## Run tests with race detector and coverage; outputs cover.out per module
	go test -race -coverprofile=cover.out -covermode=atomic ./...
	cd sdk      && go test -race -coverprofile=cover-sdk.out -covermode=atomic ./...
	cd workflow && go test -race -coverprofile=cover-workflow.out -covermode=atomic ./...
	go tool cover -func=cover.out | grep -E "^total|internal/cli|internal/run|criteria-adapter-mcp"
	@echo "See cover.out, cover-sdk.out, cover-workflow.out for full details."

bench: ## Run benchmarks for workflow, engine, and plugin packages (targeted; see notes)
	go test -run='^$$' -bench=. -benchmem ./workflow/...
	go test -run='^$$' -bench=. -benchmem ./internal/engine/...
	go test -run='^$$' -bench=. -benchmem ./internal/plugin/...

test-flake-watch: ## Re-run previously flaky packages under -count=20 -race (not a CI gate; use for local regression checks)
	go test -race -count=20 ./internal/engine/... ./internal/plugin/...

test-conformance: ## Run SDK conformance suite (in-memory Subject)
	cd sdk && go test -race -run TestConformance ./conformance/...

lint-imports: ## Enforce import-graph boundaries (see tools/import-lint/)
	go run ./tools/import-lint .
	@echo "Import boundaries OK."

lint-go: ## Run golangci-lint across all modules with the baseline allowlist
	@# Merge configs: .golangci.yml ends with exclude-rules:; strip the
	@# "issues:\n  exclude-rules:\n" header from .golangci.baseline.yml and
	@# append the remaining items so they extend the exclude-rules list.
	@cat .golangci.yml > .golangci.merged.yml
	@tail -n +3 .golangci.baseline.yml >> .golangci.merged.yml
	go tool golangci-lint run --config .golangci.merged.yml ./...             || { rm -f .golangci.merged.yml; exit 1; }
	(cd sdk      && go tool golangci-lint run --config ../.golangci.merged.yml ./...) || { rm -f .golangci.merged.yml; exit 1; }
	(cd workflow && go tool golangci-lint run --config ../.golangci.merged.yml ./...) || { rm -f .golangci.merged.yml; exit 1; }
	@rm -f .golangci.merged.yml

lint-baseline-check: ## Fail if .golangci.baseline.yml exceeds the cap in tools/lint-baseline/cap.txt
	@cap_file=tools/lint-baseline/cap.txt; \
	if [ ! -r "$$cap_file" ]; then \
		echo "ERROR: Cannot read $$cap_file"; \
		exit 1; \
	fi; \
	cap=$$(cat "$$cap_file"); \
	if ! printf '%s\n' "$$cap" | grep -qE '^[0-9]+$$'; then \
		echo "ERROR: $$cap_file must contain a single integer; got: $$cap"; \
		exit 1; \
	fi; \
	count=$$(go run ./tools/lint-baseline -count .golangci.baseline.yml); \
	if [ "$$count" -gt "$$cap" ]; then \
		echo "ERROR: .golangci.baseline.yml has $$count entries; cap is $$cap ($$cap_file)."; \
		echo "       Either fix the new findings or, with explicit reviewer agreement, raise the cap."; \
		exit 1; \
	fi; \
	echo "Lint baseline within cap ($$count / $$cap)."

lint: lint-imports lint-go ## Run all linters

validate: build ## Validate all standalone example workflows
	@# Some examples (e.g. workstream_review_loop.hcl) reference files outside
	@# their own directory via file() in agent.config; allow the repo root so
	@# compile-time file() resolution succeeds.
	@for f in examples/*.hcl examples/plugins/*/*.hcl examples/phase3-fold/*.hcl; do \
		echo "Validating $$f..."; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS="$(CURDIR)" ./bin/criteria validate "$$f" || exit 1; \
	done
	@echo "All examples validated."

example-plugin: build ## Build and run the greeter example plugin end-to-end
	@echo "Building greeter example plugin..."
	cd examples/plugins/greeter && GOWORK=off go build -o ../../../bin/criteria-adapter-greeter .
	@tmpdir=$$(mktemp -d); \
	cp bin/criteria-adapter-greeter "$$tmpdir/"; \
	chmod +x "$$tmpdir/criteria-adapter-greeter"; \
	eventsfile=$$(mktemp); \
	CRITERIA_PLUGINS="$$tmpdir" ./bin/criteria apply examples/plugins/greeter/example.hcl \
		--events-file "$$eventsfile" 2>&1; \
	rc=$$?; \
	if [ $$rc -ne 0 ]; then \
		echo "ERROR: criteria apply failed"; \
		rm -rf "$$tmpdir" "$$eventsfile"; exit 1; \
	fi; \
	if ! grep -q '"hello, world"' "$$eventsfile"; then \
		echo "ERROR: expected greeting not found in events"; \
		cat "$$eventsfile"; \
		rm -rf "$$tmpdir" "$$eventsfile"; exit 1; \
	fi; \
	rm -rf "$$tmpdir" "$$eventsfile"; \
	echo "example-plugin: OK"

ci: build test lint-imports lint-go lint-baseline-check validate example-plugin ## Run all CI gates (build, test, lint-imports, lint-go, lint-baseline-check, validate, example-plugin)

clean: ## Remove build artifacts
	rm -rf bin conformance.test
