.PHONY: help bootstrap tidy build plugins proto proto-lint proto-check-drift \
	test validate clean

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

plugins: ## Build adapter plugin binaries (output: bin/overlord-adapter-*)
	mkdir -p bin
	@for d in ./cmd/overlord-adapter-*; do \
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

validate: build ## Validate all standalone example workflows
	@for f in examples/*.hcl; do \
		echo "Validating $$f..."; \
		./bin/overseer validate "$$f" || exit 1; \
	done
	@echo "All examples validated."

clean: ## Remove build artifacts
	rm -rf bin conformance.test
