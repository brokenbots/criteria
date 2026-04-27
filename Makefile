.PHONY: help bootstrap tidy build plugins proto proto-lint proto-check-drift \
	test test-conformance lint-imports validate example-plugin ci clean

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
	@for f in examples/*.hcl examples/plugins/*/*.hcl; do \
		echo "Validating $$f..."; \
		./bin/overseer validate "$$f" || exit 1; \
	done
	@echo "All examples validated."

example-plugin: build ## Build and run the greeter example plugin end-to-end
	@echo "Building greeter example plugin..."
	cd examples/plugins/greeter && GOWORK=off go build -o ../../../bin/overseer-adapter-greeter .
	@tmpdir=$$(mktemp -d); \
	cp bin/overseer-adapter-greeter "$$tmpdir/"; \
	chmod +x "$$tmpdir/overseer-adapter-greeter"; \
	eventsfile=$$(mktemp); \
	OVERSEER_PLUGINS="$$tmpdir" ./bin/overseer apply examples/plugins/greeter/example.hcl \
		--events-file "$$eventsfile" 2>&1; \
	rc=$$?; \
	if [ $$rc -ne 0 ]; then \
		echo "ERROR: overseer apply failed"; \
		rm -rf "$$tmpdir" "$$eventsfile"; exit 1; \
	fi; \
	if ! grep -q '"hello, world"' "$$eventsfile"; then \
		echo "ERROR: expected greeting not found in events"; \
		cat "$$eventsfile"; \
		rm -rf "$$tmpdir" "$$eventsfile"; exit 1; \
	fi; \
	rm -rf "$$tmpdir" "$$eventsfile"; \
	echo "example-plugin: OK"

ci: build test lint-imports validate example-plugin ## Run all CI gates (build, test, lint-imports, validate, example-plugin)

clean: ## Remove build artifacts
	rm -rf bin conformance.test
