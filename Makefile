.PHONY: help bootstrap tidy build plugins install proto proto-lint proto-check-drift \
	test test-cover test-conformance test-flake-watch lint-imports lint-go lint-baseline-check lint validate validate-self-workflows example-plugin bench docker-runtime docker-runtime-smoke ci self self-loop clean

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
		criteria/runtime:dev apply /workspace/examples/hello

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

.PHONY: spec-gen spec-check
spec-gen: ## Regenerate the generated sections in docs/LANGUAGE-SPEC.md
	go run ./tools/spec-gen -out docs/LANGUAGE-SPEC.md

spec-check: ## Check that docs/LANGUAGE-SPEC.md is up to date with schema sources
	go run ./tools/spec-gen -check -out docs/LANGUAGE-SPEC.md

lint: lint-imports lint-go lint-baseline-check spec-check ## Run all linters

validate: build ## Validate all example workflow directories
	@for d in examples/build_and_test examples/copilot_planning_then_execution \
		examples/demo_tour_local examples/file_function examples/hello \
		examples/perf_1000_logs \
		examples/phase3-environment examples/phase3-fold examples/phase3-multi-file \
		examples/phase3-output examples/phase3-subworkflow examples/phase3-shared-variable \
		examples/phase3-parallel; do \
		echo "Validating $$d..."; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS="$(CURDIR)" ./bin/criteria validate "$$d" || exit 1; \
	done
	@for f in examples/plugins/*/*.hcl; do \
		echo "Validating $$f..."; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS="$(CURDIR)" ./bin/criteria validate "$$f" || exit 1; \
	done
	@echo "All examples validated."

validate-self-workflows: build ## Validate + compile all .criteria/workflows/* trees
	@for d in .criteria/workflows/*/; do \
		echo "Validating $$d..."; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS=".criteria/workflows" \
			./bin/criteria validate "$$d" || exit 1; \
		CRITERIA_WORKFLOW_ALLOWED_PATHS=".criteria/workflows" \
			./bin/criteria compile "$$d" >/dev/null || exit 1; \
	done
	@echo "All self-development workflows validated."

self: build plugins ## Pick the next pending workstream and run the full self-development cycle (interactive: pauses on operator approval gates)
	@mkdir -p .criteria/tmp; \
	lock=.criteria/tmp/self.lock; \
	if [ -f "$$lock" ]; then \
		pid=$$(cat "$$lock" 2>/dev/null || echo); \
		if [ -n "$$pid" ] && kill -0 "$$pid" 2>/dev/null; then \
			echo "[self] another run is in progress (pid=$$pid); refusing to start"; \
			echo "[self] if you are sure no run is active: rm $$lock"; \
			exit 1; \
		fi; \
		echo "[self] removing stale lock (no live pid=$$pid)"; \
		rm -f "$$lock"; \
	fi; \
	echo $$$$ > "$$lock"; \
	trap 'rm -f "$$lock"' EXIT INT TERM; \
	ws=$$(sh .criteria/workflows/bootstrap/scripts/pick-next-workstream.sh); \
	if [ -z "$$ws" ]; then \
		echo "[self] no pending workstreams — main is up to date."; \
		exit 0; \
	fi; \
	echo "[self] processing $$ws"; \
	CRITERIA_LOCAL_APPROVAL="$${CRITERIA_LOCAL_APPROVAL:-stdin}" \
	CRITERIA_PLUGINS="$(CURDIR)/bin" \
	CRITERIA_WORKFLOW_ALLOWED_PATHS=".criteria/workflows" \
		./bin/criteria apply .criteria/workflows/bootstrap \
			--var workstream_file=$$ws \
			--var project_dir=$(CURDIR)

self-loop: build plugins ## Drain the workstream backlog: run `make self` repeatedly until the picker returns empty
	@while :; do \
		ws=$$(sh .criteria/workflows/bootstrap/scripts/pick-next-workstream.sh); \
		if [ -z "$$ws" ]; then \
			echo "[self-loop] backlog empty — exiting clean."; \
			exit 0; \
		fi; \
		echo "[self-loop] next workstream: $$ws"; \
		$(MAKE) self || { echo "[self-loop] make self failed; stopping"; exit 1; }; \
	done

workflow_%: build plugins ## Run a single subworkflow by name (.criteria/workflows/<name>); pass vars via WORKFLOW_VARS="--var k=v ..."
	@CRITERIA_PLUGINS="$(CURDIR)/bin" \
	CRITERIA_WORKFLOW_ALLOWED_PATHS=".criteria/workflows" \
		./bin/criteria apply .criteria/workflows/$* \
			--var project_dir=$(CURDIR) \
			$(WORKFLOW_VARS)

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

ci: build test lint-imports lint-go lint-baseline-check spec-check validate validate-self-workflows example-plugin ## Run all CI gates (build, test, lint-imports, lint-go, lint-baseline-check, spec-check, validate, validate-self-workflows, example-plugin)

clean: ## Remove build artifacts
	rm -rf bin conformance.test
