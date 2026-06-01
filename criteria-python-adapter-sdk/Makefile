.PHONY: help test proto build-binaries

help:
	@echo "Available targets:"
	@echo "  test          - Run pytest suite"
	@echo "  proto         - Regenerate Python bindings from vendored .proto files"
	@echo "  build-binaries - Build Nuitka onefile binaries (requires nuitka)"

test:
	uv run pytest -v

proto:
	./scripts/generate_proto.sh

# Build matrix: linux-x64, linux-arm64, darwin-arm64, windows-x64 (future-ready)
# Requires: pip install nuitka
build-binaries:
	@echo "Building Nuitka onefile binaries..."
	mkdir -p dist
	# linux-x64 (native on Linux x86_64)
	python -m nuitka --standalone --onefile \
		--output-filename=criteria-adapter-sdk-linux-x64 \
		--output-dir=dist \
		src/criteria_adapter_sdk/__main__.py
	# linux-arm64 (cross-compilation or run on arm64 host)
	@echo "linux-arm64: build on an arm64 host or use cross-compilation flags"
	# darwin-arm64 (requires macOS arm64 host)
	@echo "darwin-arm64: build on an Apple Silicon macOS host"
	# windows-x64 (future-ready; requires Windows host or MinGW cross toolchain)
	@echo "windows-x64: future-ready target (requires Windows host or cross toolchain)"
