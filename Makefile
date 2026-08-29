.PHONY: help bootstrap tidy build plugins install proto proto-lint proto-check-drift \
	test test-cover coverage-check test-conformance test-flake-watch lint-imports lint-go lint-baseline-check lint-no-todos lint lint-sh vuln-scan deps-outdated deps-majors validate validate-docs example-plugin bench docker-runtime docker-runtime-smoke ci clean

# Default target: list available targets.
help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap: ## Install / sync Go workspace dependencies
	go work sync

tidy: ## Run go mod tidy across all modules
	go mod tidy
	cd sdk      && go mod tidy
	cd tools    && go mod tidy
	cd workflow && go mod tidy

build: ## Build the criteria binary (output: bin/criteria)
	mkdir -p bin
	go build -ldflags "-X github.com/brokenbots/criteria/workflow/version.Version=$(shell git describe --tags --match 'v*' --always --dirty)" -o bin/criteria ./cmd/criteria

plugins: ## Build adapter plugin binaries (output: bin/criteria-adapter-*)
	mkdir -p bin
	@for d in ./cmd/criteria-adapter-*; do \
		if [ -d "$$d" ]; then \
			name=$${d##*/}; \
			go build -o bin/$$name $$d; \
		fi; \
	done
	@# noop was extracted to its own repo (ghcr.io/brokenbots/criteria-adapter-noop);
	@# build the in-tree conformance fixture as bin/criteria-adapter-noop so the
	@# examples and the CLI smoke test stay self-contained (no external OCI pull).
	go build -o bin/criteria-adapter-noop ./internal/adapter/conformance/testdata/noop

install: build plugins ## Install criteria to ~/.local/criteria (binary → ~/.local/criteria/bin, plugins → ~/.local/criteria/adapters)
	@install -d "$$HOME/.local/criteria/bin" "$$HOME/.local/criteria/adapters"
	@install -m 755 bin/criteria "$$HOME/.local/criteria/bin/criteria"
	@for f in bin/criteria-adapter-*; do \
		[ -f "$$f" ] && install -m 755 "$$f" "$$HOME/.local/criteria/adapters/"; \
	done
	@echo ""
	@echo "criteria installed to $$HOME/.local/criteria"
	@echo ""
	@echo "Add the following to your shell config to use it:"
	@echo ""
	@echo "  bash  (~/.bashrc or ~/.bash_profile):"
	@echo '    export PATH="$$HOME/.local/criteria/bin:$$PATH"'
	@echo '    export CRITERIA_ADAPTERS="$$HOME/.local/criteria/adapters"'
	@echo ""
	@echo "  zsh   (~/.zshrc):"
	@echo '    export PATH="$$HOME/.local/criteria/bin:$$PATH"'
	@echo '    export CRITERIA_ADAPTERS="$$HOME/.local/criteria/adapters"'
	@echo ""
	@echo "  fish  (~/.config/fish/config.fish):"
	@echo '    fish_add_path $$HOME/.local/criteria/bin'
	@echo '    set -gx CRITERIA_ADAPTERS $$HOME/.local/criteria/adapters'
	@echo ""

docker-runtime: ## Build the runtime Docker image (criteria/runtime:dev)
	docker build -t criteria/runtime:dev -f Dockerfile.runtime .

docker-runtime-smoke: docker-runtime ## Run a workflow inside the runtime image
	docker run --rm -v "$$PWD/examples:/workspace/examples:ro" \
		criteria/runtime:dev apply /workspace/examples/hello

proto: ## Regenerate Go bindings from in-tree proto files (v1 server API; adapter protocol v2 lives in the criteria-adapter-proto module)
	buf generate --template buf.gen.yaml --path proto/criteria/v1
	@echo "Generated v1 server-API proto bindings."

proto-lint: ## Lint proto files
	buf lint

proto-check-drift: ## Fail if generated proto code is out of sync with proto sources
	buf generate --template buf.gen.yaml --path proto/criteria/v1
	@git diff --exit-code sdk/pb/criteria/v1/ || \
		(echo "ERROR: Generated proto files are out of sync. Run 'make proto' and commit."; exit 1)

test: ## Run all unit tests
	go test -race ./...
	cd sdk      && go test -race ./...
	cd tools    && go test -race ./...
	cd workflow && go test -race ./...

test-cover: ## Run tests with race detector and coverage; outputs cover.out per module
	go test -race -coverprofile=cover.out -covermode=atomic ./...
	cd sdk      && go test -race -coverprofile=cover-sdk.out -covermode=atomic ./...
	cd tools    && go test -race -coverprofile=cover-tools.out -covermode=atomic ./...
	cd workflow && go test -race -coverprofile=cover-workflow.out -covermode=atomic ./...
	go tool cover -func=cover.out | grep -E "^total|internal/cli|internal/run|criteria-adapter-mcp"
	@echo "See cover.out, cover-sdk.out, cover-workflow.out for full details."

coverage-check: test-cover ## Enforce the per-package coverage ratchet (WS44); floors in tools/coverage-floors.txt
	go run ./tools/coverage-check -floors tools/coverage-floors.txt cover.out sdk/cover-sdk.out workflow/cover-workflow.out

bench: ## Run benchmarks for workflow, engine, and plugin packages (targeted; see notes)
	go test -run='^$$' -bench=. -benchmem ./workflow/...
	go test -run='^$$' -bench=. -benchmem ./internal/engine/...
	go test -run='^$$' -bench=. -benchmem ./internal/plugin/...

test-flake-watch: ## Re-run previously flaky packages under -count=20 -race (not a CI gate; use for local regression checks)
	go test -race -count=20 ./internal/engine/... ./internal/plugin/...

test-conformance: ## Run SDK conformance suite (in-memory Subject)
	cd sdk && go test -race -run TestConformance ./conformance/...

lint-imports: ## Enforce import-graph boundaries (see tools/import-lint/)
	go run github.com/brokenbots/criteria/tools/import-lint .
	@echo "Import boundaries OK."

bin/golangci-lint: tools/go.mod tools/go.sum
	(cd tools && go build -o ../bin/golangci-lint github.com/golangci/golangci-lint/cmd/golangci-lint)

lint-go: bin/golangci-lint ## Run golangci-lint across all modules with the baseline allowlist
	@# Merge configs: .golangci.yml ends with exclude-rules:; strip the
	@# "issues:\n  exclude-rules:\n" header from .golangci.baseline.yml and
	@# append the remaining items so they extend the exclude-rules list.
	@cat .golangci.yml > .golangci.merged.yml
	@tail -n +3 .golangci.baseline.yml >> .golangci.merged.yml
	./bin/golangci-lint run --config .golangci.merged.yml ./...             || { rm -f .golangci.merged.yml; exit 1; }
	(cd sdk      && ../bin/golangci-lint run --config ../.golangci.merged.yml ./...) || { rm -f .golangci.merged.yml; exit 1; }
	(cd workflow && ../bin/golangci-lint run --config ../.golangci.merged.yml ./...) || { rm -f .golangci.merged.yml; exit 1; }
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
	count=$$(go run github.com/brokenbots/criteria/tools/lint-baseline -count .golangci.baseline.yml); \
	if [ "$$count" -gt "$$cap" ]; then \
		echo "ERROR: .golangci.baseline.yml has $$count entries; cap is $$cap ($$cap_file)."; \
		echo "       Either fix the new findings or, with explicit reviewer agreement, raise the cap."; \
		exit 1; \
	fi; \
	echo "Lint baseline within cap ($$count / $$cap)."

.PHONY: spec-gen spec-check
spec-gen: ## Regenerate the generated sections in docs/LANGUAGE-SPEC.md
	go run github.com/brokenbots/criteria/tools/spec-gen -out docs/LANGUAGE-SPEC.md

spec-check: ## Check that docs/LANGUAGE-SPEC.md is up to date with schema sources
	go run github.com/brokenbots/criteria/tools/spec-gen -check -out docs/LANGUAGE-SPEC.md

lint-no-todos: ## Fail if any TODO/FIXME/XXX marker appears in non-test production Go source
	@if grep -rn 'TODO\|FIXME\|XXX' --include='*.go' \
	    --exclude-dir=vendor --exclude-dir=testdata \
	    cmd/ internal/ workflow/ sdk/ 2>/dev/null \
	    | grep -v '_test\.go' \
	    | grep -E .; then \
	    echo "FAIL: TODO/FIXME/XXX markers found in production code"; \
	    exit 1; \
	fi
	@echo "OK: no TODO/FIXME/XXX markers in production code"

lint-sh: ## Check POSIX shell syntax of install.sh
	@sh -n install.sh
	@echo "install.sh: POSIX syntax OK"

lint: lint-imports lint-go lint-baseline-check spec-check lint-no-todos lint-sh ## Run all linters

# Pinned osv-scanner version — keep in sync with the osv-scan CI job. Run via
# `go run pkg@version` so it does not touch any module's go.mod/go.sum (the build
# is module-aware but ignores the main module).
OSV_SCANNER_VERSION := v2.3.8
vuln-scan: ## Scan all workspace modules for known vulnerabilities (osv-scanner; local parity with CI osv-scan)
	go run github.com/google/osv-scanner/v2/cmd/osv-scanner@$(OSV_SCANNER_VERSION) scan source -r .

# Dependency-freshness tooling (WS50). gomajor + go-mod-outdated are pinned in
# tools/go.mod, so these `go run` invocations resolve via the workspace (no
# floating @latest). This is the source of truth for "are we on latest
# major.minor", not Dependabot. See docs/dependency-policy.md.
MODULES := . sdk tools workflow
deps-outdated: ## Report direct deps behind their latest minor/patch (workspace-wide; go-mod-outdated)
	@# The go.work graph unifies all four modules (., sdk, tools, workflow), so a
	@# single workspace-wide listing covers them all. Per-module GOWORK=off does
	@# not work here: the modules require each other by local path, not a tag.
	go list -u -m -json all | go run github.com/psampaz/go-mod-outdated -update -direct

deps-majors: ## List available major-version (/vN) upgrades per module (gomajor); WS51 applies them
	@for m in $(MODULES); do \
		echo "== $$m =="; \
		( cd $$m && go run github.com/icholy/gomajor list ) ; \
	done

validate: build ## Validate all example workflow directories
	@for d in examples/hello examples/tour examples/subworkflow \
		examples/build_and_test examples/copilot_planning_then_execution \
		examples/llm-pack/01-linear \
		examples/llm-pack/02-branching-switch \
		examples/llm-pack/03-iteration-for-each \
		examples/llm-pack/04-iteration-parallel \
		examples/llm-pack/05-subworkflow \
		examples/llm-pack/06-approval-and-wait \
		examples/llm-pack/07-shared-variable \
		examples/llm-pack/08-fileset-template; do \
		echo "Validating $$d..."; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS="$(CURDIR)" ./bin/criteria validate "$$d" || exit 1; \
	done
	@for f in examples/plugins/*/*.hcl; do \
		echo "Validating $$f..."; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS="$(CURDIR)" ./bin/criteria validate "$$f" || exit 1; \
	done
	@echo "All examples validated."

validate-docs: ## Validate HCL fenced blocks in docs/LANGUAGE-SPEC.md
	@mkdir -p bin
	@go build -ldflags "-X github.com/brokenbots/criteria/workflow/version.Version=dev" -o bin/criteria-validate-docs ./cmd/criteria
	@CRITERIA_BIN=./bin/criteria-validate-docs ./tools/validate-docs.sh

example-plugin: build ## Build and run the greeter example plugin end-to-end
	@echo "Building greeter example plugin..."
	cd examples/plugins/greeter && GOWORK=off go build -o ../../../bin/criteria-adapter-greeter .
	@tmpdir=$$(mktemp -d); \
	cp bin/criteria-adapter-greeter "$$tmpdir/"; \
	chmod +x "$$tmpdir/criteria-adapter-greeter"; \
	eventsfile=$$(mktemp); \
	CRITERIA_ADAPTERS="$$tmpdir" ./bin/criteria apply examples/plugins/greeter/example.hcl \
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

ci: build test lint validate example-plugin ## Run all CI gates (build, test, lint, validate, example-plugin)

clean: ## Remove build artifacts
	rm -rf bin conformance.test
