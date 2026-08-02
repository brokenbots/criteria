#!/bin/sh
# POSIX one-line installer for the Criteria CLI.
# Usage: curl -fsSL https://raw.githubusercontent.com/brokenbots/criteria/main/install.sh | sh
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
curl -fsSL -o "${tmp}/SHA256SUMS.bundle" "${base_url}/SHA256SUMS.bundle" || true

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
    if [ -f "${tmp}/SHA256SUMS.bundle" ]; then
        printf 'Verifying SHA256SUMS signature with cosign...\n'
        cosign verify-blob \
            --bundle "${tmp}/SHA256SUMS.bundle" \
            --certificate-identity "https://github.com/brokenbots/criteria/.github/workflows/release.yml@refs/tags/${tag}" \
            --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
            "${tmp}/SHA256SUMS" || {
                printf 'ERROR: SHA256SUMS signature verification failed for %s\n' "$tag" >&2
                exit 1
            }
    else
        printf 'WARNING: cosign is available but SHA256SUMS.bundle was not downloaded; skipping signature verification\n' >&2
    fi
else
    printf 'WARNING: cosign not found in PATH. Transport integrity was verified, but signature authenticity was not checked.\n' >&2
fi

# Install the binary and bundled adapters.
stage=$(mktemp -d "${TMPDIR:-/tmp}/criteria-extract.XXXXXX")
trap 'rm -rf "${tmp}" "${stage}"' EXIT

tar -xzf "${tmp}/${tarball}" -C "$stage"

# Resolve the binary install directory.
install_dir=""
install_with_sudo=0

# Option 1: $HOME/.local/bin if it exists and is already on PATH.
if [ -d "${HOME}/.local/bin" ]; then
    case ":${PATH}:" in
        *:"${HOME}/.local/bin":*)
            install_dir="${HOME}/.local/bin"
            ;;
    esac
fi

# Option 2: /usr/local/bin if it exists and is writable directly or via passwordless sudo.
if [ -z "$install_dir" ] && [ -d /usr/local/bin ]; then
    if [ -w /usr/local/bin ]; then
        install_dir=/usr/local/bin
    elif command -v sudo >/dev/null 2>&1 && sudo -n test -w /usr/local/bin >/dev/null 2>&1; then
        install_dir=/usr/local/bin
        install_with_sudo=1
    fi
fi

# Option 3: fallback to $HOME/.local/criteria/bin. Only this case may edit a startup file.
if [ -z "$install_dir" ]; then
    install_dir="${HOME}/.local/criteria/bin"
fi

case "$install_dir" in
    "${HOME}/.local/bin"|"${HOME}/.local/criteria/bin")
        install -d "$install_dir"
        ;;
esac

if [ "$install_with_sudo" -eq 1 ]; then
    sudo install -m 755 "${stage}/criteria" "${install_dir}/criteria"
else
    install -m 755 "${stage}/criteria" "${install_dir}/criteria"
fi

# Adapters always install to ${HOME}/.local/criteria/adapters.
install -d "${HOME}/.local/criteria/adapters"
for f in "${stage}"/criteria-adapter-*; do
    if [ -f "$f" ]; then
        install -m 755 "$f" "${HOME}/.local/criteria/adapters/"
    fi
done

# Update exactly one shell startup file, only for the fallback directory.
if [ "$install_dir" = "${HOME}/.local/criteria/bin" ]; then
    shell_name=$(basename "${SHELL:-}")
    case "$shell_name" in
        bash)
            if [ -f "${HOME}/.bashrc" ]; then
                profile_file="${HOME}/.bashrc"
            elif [ -f "${HOME}/.bash_profile" ]; then
                profile_file="${HOME}/.bash_profile"
            else
                profile_file="${HOME}/.bashrc"
            fi
            ;;
        zsh)
            profile_file="${HOME}/.zshrc"
            ;;
        *)
            profile_file="${HOME}/.profile"
            ;;
    esac

    path_line='export PATH="$HOME/.local/criteria/bin:$PATH"'
    if ! grep -qxF "$path_line" "$profile_file" 2>/dev/null; then
        printf '\n%s\n' "$path_line" >> "$profile_file"
    fi
fi

printf '\ncriteria %s installed to %s\n' "$tag" "$install_dir"
printf 'Adapters installed to %s/.local/criteria/adapters\n' "$HOME"

if [ "$install_dir" = "${HOME}/.local/criteria/bin" ]; then
    printf 'Updated %s for future shells.\n' "$profile_file"
    printf 'Run this command to use criteria in the current shell:\n'
    printf '  %s\n' "$path_line"
else
    printf 'criteria is installed and ready to use.\n'
fi
