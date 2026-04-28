.PHONY: help bootstrap tidy build plugins proto proto-lint proto-check-drift \
	test test-conformance test-flake-watch lint-imports lint-go lint validate example-plugin ci clean

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

lint: lint-imports lint-go ## Run all linters

validate: build ## Validate all standalone example workflows
	@for f in examples/*.hcl examples/plugins/*/*.hcl; do \
		echo "Validating $$f..."; \
		./bin/criteria validate "$$f" || exit 1; \
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

ci: build test lint-imports lint-go validate example-plugin ## Run all CI gates (build, test, lint-imports, lint-go, validate, example-plugin)

clean: ## Remove build artifacts
	rm -rf bin conformance.test
