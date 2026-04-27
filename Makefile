.PHONY: help bootstrap tidy build plugins proto proto-lint proto-check-drift \
	test test-conformance lint-imports validate ci clean

# Default target: list available targets.
help:
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap: ## Install / sync Go workspace dependencies
	go work sync

tidy: ## Run go mod tidy across all modules
	go mod tidy
	cd sdk      && go mod tidy
	cd workflow && go mod tidy

build: ## Build the overseer binary (output: bin/overseer)
	mkdir -p bin
	go build -o bin/overseer ./cmd/overseer

plugins: ## Build adapter plugin binaries (output: bin/overseer-adapter-*)
	mkdir -p bin
	@for d in ./cmd/overseer-adapter-*; do \
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

test-conformance: ## Run SDK conformance suite (in-memory Subject)
	cd sdk && go test -race -run TestConformance ./conformance/...

lint-imports: ## Enforce import-graph boundaries (see tools/import-lint/)
	go run ./tools/import-lint .
	@echo "Import boundaries OK."

validate: build ## Validate all standalone example workflows
	@for f in examples/*.hcl; do \
		echo "Validating $$f..."; \
		./bin/overseer validate "$$f" || exit 1; \
	done
	@echo "All examples validated."

ci: build lint-imports test validate ## Run all CI checks (build, lint-imports, test, validate)

clean: ## Remove build artifacts
	rm -rf bin conformance.test
