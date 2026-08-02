#!/bin/sh
# POSIX one-line installer for the Criteria CLI.
# Usage: curl -fsSL https://raw.githubusercontent.com/brokenbots/criteria/main/install.sh | /bin/sh
# Version can be pinned via CRITERIA_VERSION=v0.5.6.

set -e

# Detect platform and map to the release tarball suffix.
uname_s=$(uname -s)
uname_m=$(uname -m)

os_arch=""
case "$uname_s" in
    Linux)
        case "$uname_m" in
            x86_64) os_arch="linux-amd64" ;;
            aarch64|arm64) os_arch="linux-arm64" ;;
            *)
                printf 'ERROR: unsupported platform: Linux/%s\n' "$uname_m" >&2
                printf 'Supported: Linux/x86_64, Linux/aarch64, Linux/arm64, Darwin/arm64\n' >&2
                exit 1
                ;;
        esac
        ;;
    Darwin)
        case "$uname_m" in
            arm64) os_arch="darwin-arm64" ;;
            *)
                printf 'ERROR: unsupported platform: Darwin/%s\n' "$uname_m" >&2
                printf 'Supported: Linux/x86_64, Linux/aarch64, Linux/arm64, Darwin/arm64\n' >&2
                exit 1
                ;;
        esac
        ;;
    *)
        printf 'ERROR: unsupported platform: %s/%s\n' "$uname_s" "$uname_m" >&2
        printf 'Supported: Linux/x86_64, Linux/aarch64, Linux/arm64, Darwin/arm64\n' >&2
        exit 1
        ;;
esac

if ! command -v curl >/dev/null 2>&1; then
    printf 'ERROR: curl is required but not found in PATH\n' >&2
    exit 1
fi

# Resolve the version to install.
if [ -n "${CRITERIA_VERSION:-}" ]; then
    tag="$CRITERIA_VERSION"
else
    latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
        'https://github.com/brokenbots/criteria/releases/latest')
    tag=$(printf '%s' "$latest_url" | sed 's#.*/##')
fi

if ! printf '%s\n' "$tag" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+|-beta[0-9]+)?$'; then
    printf 'ERROR: invalid release tag: %s\n' "$tag" >&2
    exit 1
fi

base_url="https://github.com/brokenbots/criteria/releases/download/${tag}"
tarball="criteria-${tag}-${os_arch}.tar.gz"

# Prepare a temporary download directory.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/criteria-install.XXXXXX")
trap 'rm -rf "${tmp}"' EXIT

printf 'Downloading %s...\n' "$tarball"
curl -fsSL -o "${tmp}/${tarball}" "${base_url}/${tarball}"
curl -fsSL -o "${tmp}/SHA256SUMS" "${base_url}/SHA256SUMS"
curl -fsSL -o "${tmp}/SHA256SUMS.sig" "${base_url}/SHA256SUMS.sig" || true
curl -fsSL -o "${tmp}/SHA256SUMS.cert" "${base_url}/SHA256SUMS.cert" || true

# Verify the tarball hash.
expected=$(awk -v file="$tarball" '$2 == file { print $1; exit }' "${tmp}/SHA256SUMS")
if [ -z "$expected" ]; then
    printf 'ERROR: no checksum entry found for %s in SHA256SUMS\n' "$tarball" >&2
    exit 1
fi

actual=$(
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "${tmp}/${tarball}"
    else
        shasum -a 256 "${tmp}/${tarball}"
    fi | awk '{ print $1 }'
)

if [ "$expected" != "$actual" ]; then
    printf 'ERROR: checksum mismatch for %s\n' "$tarball" >&2
    printf 'Expected: %s\nActual:   %s\n' "$expected" "$actual" >&2
    exit 1
fi

# When cosign is not installed, this installer proceeds with transport-integrity only. This is a deliberate product choice: the one-line installer must work on stock machines; install cosign to also verify signature authenticity.
if command -v cosign >/dev/null 2>&1; then
    if [ -f "${tmp}/SHA256SUMS.sig" ] && [ -f "${tmp}/SHA256SUMS.cert" ]; then
        printf 'Verifying SHA256SUMS signature with cosign...\n'
        cosign verify-blob \
            --signature "${tmp}/SHA256SUMS.sig" \
            --certificate "${tmp}/SHA256SUMS.cert" \
            --certificate-identity "https://github.com/brokenbots/criteria/.github/workflows/release.yml@refs/tags/${tag}" \
            --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
            "${tmp}/SHA256SUMS" || {
                printf 'ERROR: SHA256SUMS signature verification failed for %s\n' "$tag" >&2
                exit 1
            }
    else
        printf 'WARNING: cosign is available but signature/certificate files were not downloaded; skipping signature verification\n' >&2
    fi
else
    printf 'WARNING: cosign not found in PATH. Transport integrity was verified, but signature authenticity was not checked.\n' >&2
fi

# Install the binary and bundled adapters.
install -d "${HOME}/.criteria/bin" "${HOME}/.criteria/adapters"

stage=$(mktemp -d "${TMPDIR:-/tmp}/criteria-extract.XXXXXX")
trap 'rm -rf "${tmp}" "${stage}"' EXIT

tar -xzf "${tmp}/${tarball}" -C "$stage"

install -m 755 "${stage}/criteria" "${HOME}/.criteria/bin/criteria"

for f in "${stage}"/criteria-adapter-*; do
    if [ -f "$f" ]; then
        install -m 755 "$f" "${HOME}/.criteria/adapters/"
    fi
done

# Update shell startup files so criteria is on PATH.
path_line='export PATH="$HOME/.criteria/bin:$PATH"'
startup_files='.bashrc .bash_profile .zshrc .profile'
found=0
for name in $startup_files; do
    file="${HOME}/${name}"
    if [ -f "$file" ]; then
        if ! grep -qxF "$path_line" "$file"; then
            printf '\n%s\n' "$path_line" >> "$file"
        fi
        found=1
    fi
done

if [ "$found" -eq 0 ]; then
    printf '%s\n' "$path_line" > "${HOME}/.profile"
fi

printf '\ncriteria %s installed to %s/.criteria/bin\n' "$tag" "$HOME"
printf 'Adapters installed to %s/.criteria/adapters\n' "$HOME"
printf 'Open a new shell or source your profile to use the criteria command.\n'
