#!/usr/bin/env python3
"""Update the brokenbots/homebrew-criteria formula from a signed release manifest.

Inputs:
  TAG      - release tag, e.g. v0.5.7
  TAP_DIR  - path to a checkout of brokenbots/homebrew-criteria

The script downloads SHA256SUMS and SHA256SUMS.bundle from the release, verifies
the bundle with cosign, then writes Formula/criteria.rb using the tarball hashes
from the signed manifest.
"""
import hashlib
import os
import re
import subprocess
import sys
import urllib.request
from pathlib import Path

REPO = "brokenbots/criteria"


def error(msg: str) -> None:
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(1)


def download(tag: str, name: str, dest: Path) -> None:
    url = f"https://github.com/{REPO}/releases/download/{tag}/{name}"
    print(f"Downloading {url} ...")
    urllib.request.urlretrieve(url, dest)


def verify_bundle(tag: str, sums: Path, bundle: Path) -> None:
    identity = f"https://github.com/{REPO}/.github/workflows/release.yml@refs/tags/{tag}"
    issuer = "https://token.actions.githubusercontent.com"
    subprocess.run(
        [
            "cosign", "verify-blob",
            "--bundle", str(bundle),
            "--certificate-identity", identity,
            "--certificate-oidc-issuer", issuer,
            str(sums),
        ],
        check=True,
    )


def parse_sums(path: Path) -> dict[str, str]:
    out: dict[str, str] = {}
    for line in path.read_text().splitlines():
        parts = line.strip().split()
        if len(parts) == 2:
            out[parts[1]] = parts[0]
    return out


def formula(tag: str, sums: dict[str, str]) -> str:
    version = tag.lstrip("v")
    platforms = [
        ("darwin", "arm64"),
        ("linux", "amd64"),
        ("linux", "arm64"),
    ]
    blocks: list[str] = []
    for os, arch in platforms:
        asset = f"criteria-{tag}-{os}-{arch}.tar.gz"
        sha = sums.get(asset)
        if not sha:
            error(f"missing checksum for {asset} in signed manifest")
        blocks.append(
            f"      url \"https://github.com/{REPO}/releases/download/{tag}/{asset}\"\n"
            f"      sha256 \"{sha}\""
        )

    return f'''class Criteria < Formula
  desc "Standalone workflow execution engine"
  homepage "https://github.com/{REPO}"
  license "MIT"

  on_macos do
    on_arm do
{blocks[0]}
    end
  end

  on_linux do
    on_intel do
{blocks[1]}
    end
    on_arm do
{blocks[2]}
    end
  end

  def install
    libexec.mkpath
    adapters = libexec/"adapters"
    adapters.mkpath

    libexec.install "criteria"
    adapters.install Dir["criteria-adapter-*"]
    libexec.install "LICENSE"
    libexec.install "README.md"

    (bin/"criteria").write_env_script libexec/"criteria", CRITERIA_ADAPTERS: adapters
  end

  test do
    list = shell_output("#{{bin}}/criteria adapter list")
    assert_match "noop", list
    assert_match "mcp", list
  end
end
'''


def main() -> None:
    if len(sys.argv) != 3:
        error(f"usage: {sys.argv[0]} <tag> <tap-dir>")
    tag = sys.argv[1]
    tap_dir = Path(sys.argv[2])
    if not re.fullmatch(r"v\d+\.\d+\.\d+(-rc\d+|-beta\d+)?", tag):
        error(f"unexpected tag format: {tag}")

    tmp = Path(os.environ.get("RUNNER_TEMP", "/tmp")) / "homebrew-tap-update"
    tmp.mkdir(parents=True, exist_ok=True)
    sums_path = tmp / "SHA256SUMS"
    bundle_path = tmp / "SHA256SUMS.bundle"

    download(tag, "SHA256SUMS", sums_path)
    download(tag, "SHA256SUMS.bundle", bundle_path)
    verify_bundle(tag, sums_path, bundle_path)

    sums = parse_sums(sums_path)
    formula_path = tap_dir / "Formula" / "criteria.rb"
    formula_path.parent.mkdir(parents=True, exist_ok=True)
    formula_path.write_text(formula(tag, sums))
    print(f"Wrote {formula_path}")


if __name__ == "__main__":
    main()
