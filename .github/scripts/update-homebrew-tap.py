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
import http.client
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

REPO = "brokenbots/criteria"

# Transient-download budget: downloads are retried for real upstream outages
# (upstream 504s have made this check fail), but the budget is bounded so a
# stalled or broken upstream can never hang a job indefinitely.
#
# backoff_seconds must hold at least DOWNLOAD_MAX_ATTEMPTS entries (the loop
# indexes by attempt - 1); its total sleep is ~3 minutes. The wall-clock
# deadline is the binding ceiling: control always returns from download()
# within it plus one request timeout.
DOWNLOAD_MAX_ATTEMPTS = 10
DOWNLOAD_BACKOFF_SECONDS = [1, 2, 4, 8, 16, 30, 30, 30, 30, 30]
DOWNLOAD_SLEEP_CAP_SECONDS = 30
DOWNLOAD_DEADLINE_SECONDS = 300
DOWNLOAD_REQUEST_TIMEOUT_SECONDS = 30


def error(msg: str) -> None:
    print(f"ERROR: {msg}", file=sys.stderr, flush=True)
    sys.exit(1)


def download(tag: str, name: str, dest: Path) -> None:
    url = f"https://github.com/{REPO}/releases/download/{tag}/{name}"
    print(f"Downloading {url} ...", flush=True)

    version = tag.lstrip("v")
    started = time.monotonic()
    deadline = started + DOWNLOAD_DEADLINE_SECONDS
    req = urllib.request.Request(
        url,
        headers={"User-Agent": f"criteria-ci-update-homebrew-tap/{version}"},
    )

    # last_error carries the most recent attempt's failure so every budget
    # exhaustion path can re-raise a real cause and exit non-zero.
    last_error = None
    for attempt in range(1, DOWNLOAD_MAX_ATTEMPTS + 1):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            if last_error is not None:
                raise last_error
            raise TimeoutError(
                f"{name} not downloaded within the "
                f"{DOWNLOAD_DEADLINE_SECONDS}s overall deadline"
            )
        # urllib's timeout= is a per-read socket timeout; clamping it to the
        # remaining deadline keeps one hung request from eating the budget.
        attempt_timeout = max(1, min(DOWNLOAD_REQUEST_TIMEOUT_SECONDS, int(remaining)))
        try:
            with urllib.request.urlopen(req, timeout=attempt_timeout) as resp:
                if resp.status >= 400:
                    raise urllib.error.HTTPError(
                        url, resp.status, resp.reason, resp.headers, None
                    )
                # Copy through a temp file and move into place only after the
                # copy completed, so dest is never a truncated download.
                fd, tmp_name = tempfile.mkstemp(prefix=name + ".", dir=str(dest.parent))
                try:
                    with os.fdopen(fd, "wb") as f:
                        shutil.copyfileobj(resp, f, length=65536)
                    os.replace(tmp_name, dest)
                except Exception:
                    try:
                        os.unlink(tmp_name)
                    except OSError:
                        pass
                    raise
            return
        except urllib.error.HTTPError as e:
            last_error = e
            if 500 <= e.code < 600 and attempt < DOWNLOAD_MAX_ATTEMPTS:
                delay = min(DOWNLOAD_BACKOFF_SECONDS[attempt - 1], DOWNLOAD_SLEEP_CAP_SECONDS)
                remaining = deadline - time.monotonic()
                if remaining <= delay:
                    print(
                        f"Overall download deadline exceeded before retry after "
                        f"server error {e.code} on {name} "
                        f"({time.monotonic() - started:.1f}s elapsed); {attempt} attempt(s) spent",
                        flush=True,
                    )
                    raise last_error
                print(
                    f"Server error {e.code} downloading {name}, "
                    f"attempt {attempt}/{DOWNLOAD_MAX_ATTEMPTS}; "
                    f"retrying in {delay}s ... ({time.monotonic() - started:.1f}s elapsed)",
                    flush=True,
                )
                time.sleep(delay)
                continue
            raise
        except (
            urllib.error.URLError,
            ConnectionResetError,
            ConnectionRefusedError,
            http.client.RemoteDisconnected,
            TimeoutError,
            socket.timeout,
        ) as e:
            last_error = e
            if attempt < DOWNLOAD_MAX_ATTEMPTS:
                delay = min(DOWNLOAD_BACKOFF_SECONDS[attempt - 1], DOWNLOAD_SLEEP_CAP_SECONDS)
                remaining = deadline - time.monotonic()
                if remaining <= delay:
                    print(
                        f"Overall download deadline exceeded before retry after "
                        f"transient error on {name} "
                        f"({time.monotonic() - started:.1f}s elapsed); {attempt} attempt(s) spent",
                        flush=True,
                    )
                    raise last_error
                print(
                    f"Transient error downloading {name}, "
                    f"attempt {attempt}/{DOWNLOAD_MAX_ATTEMPTS}; "
                    f"retrying in {delay}s ... ({time.monotonic() - started:.1f}s elapsed) ({e})",
                    flush=True,
                )
                time.sleep(delay)
                continue
            raise


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
    system "#{{bin}}/criteria", "--help"
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
