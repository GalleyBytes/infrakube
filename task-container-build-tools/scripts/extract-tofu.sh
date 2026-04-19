#!/bin/bash
set -euo pipefail

# extract-tofu.sh
#
# OpenTofu binary resolution tiers:
# 1. Controller cache (HTTP GET)
# 2. User-provided INFRAKUBE_TOFU_DOWNLOAD_URL override (always available)
# 3. Auto-download from INFRAKUBE_TOFU_DOWNLOAD_URL_BASE (gated by INFRAKUBE_AUTO_DOWNLOAD)
# 4. Fail with helpful error

RAW_VERSION="${INFRAKUBE_TOFU_VERSION:-latest}"
VERSION="${RAW_VERSION#v}"
BIN_DIR="/opt/tofu/bin"
TOFU_BIN="${BIN_DIR}/tofu"
CACHE_URL="${INFRAKUBE_CACHE_URL:-}"
AUTO_DOWNLOAD="${INFRAKUBE_AUTO_DOWNLOAD:-true}"
DOWNLOAD_BASE_URL="${INFRAKUBE_TOFU_DOWNLOAD_URL_BASE:-https://github.com/opentofu/opentofu/releases/download}"
DOWNLOAD_BASE_URL="${DOWNLOAD_BASE_URL%/}"

map_arch() {
    case "$(uname -m)" in
        x86_64)  echo "amd64" ;;
        aarch64) echo "arm64" ;;
        *)       echo "$(uname -m)" ;;
    esac
}

ARCH=$(map_arch)

if [ "${VERSION}" = "latest" ]; then
    echo "ERROR: tofuVersion must be set explicitly when using the task image." >&2
    exit 1
fi

if [ -f "${TOFU_BIN}" ]; then
    CURRENT=$("${TOFU_BIN}" version 2>/dev/null | head -n1 | sed 's/^.*v//' || true)
    if [ "${CURRENT}" = "${VERSION}" ]; then
        exit 0
    fi
fi

mkdir -p "${BIN_DIR}"

TMPFILE=$(mktemp)
trap 'rm -f "${TMPFILE}"' EXIT

cache_binary() {
    if [ -n "${CACHE_URL}" ]; then
        echo "Caching tofu ${VERSION} for future runs..."
        curl -sSL -X PUT --data-binary @"${TOFU_BIN}" \
            "${CACHE_URL}/api/v1/tofu/${VERSION}?arch=${ARCH}" 2>/dev/null || true
    fi
}

# Tier 1: Try controller cache
if [ -n "${CACHE_URL}" ]; then
    echo "Checking controller cache for tofu ${VERSION} (${ARCH})..."
    if curl -sSL --fail -o "${TOFU_BIN}" "${CACHE_URL}/api/v1/tofu/${VERSION}?arch=${ARCH}" 2>/dev/null; then
        chmod +x "${TOFU_BIN}"
        echo "Tofu ${VERSION} ready (from cache)."
        exit 0
    fi
    echo "Not in controller cache."
fi

# Tier 2: User-provided download URL
if [ -n "${INFRAKUBE_TOFU_DOWNLOAD_URL:-}" ]; then
    echo "Downloading tofu from user-provided URL..."
    curl -sSL --fail "${INFRAKUBE_TOFU_DOWNLOAD_URL}" -o "${TMPFILE}"
    if [ -n "${INFRAKUBE_TOFU_DOWNLOAD_SHA256:-}" ]; then
        ACTUAL_SHA256=$(sha256sum "${TMPFILE}" | cut -d' ' -f1)
        if [ "${ACTUAL_SHA256}" != "${INFRAKUBE_TOFU_DOWNLOAD_SHA256}" ]; then
            echo "ERROR: SHA256 checksum mismatch!" >&2
            echo "  Expected: ${INFRAKUBE_TOFU_DOWNLOAD_SHA256}" >&2
            echo "  Got:      ${ACTUAL_SHA256}" >&2
            exit 1
        fi
    fi
    if file "${TMPFILE}" | grep -q 'Zip archive'; then
        unzip -o "${TMPFILE}" tofu -d "${BIN_DIR}"
    else
        cp "${TMPFILE}" "${TOFU_BIN}"
    fi
    chmod +x "${TOFU_BIN}"
    echo "Tofu ${VERSION} ready (user-provided)."
    cache_binary
    exit 0
fi

# Tier 3: Auto-download from base URL
if [ "${AUTO_DOWNLOAD}" = "true" ]; then
    DOWNLOAD_URL="${DOWNLOAD_BASE_URL}/v${VERSION}/tofu_${VERSION}_linux_${ARCH}.zip"
    echo "Auto-downloading tofu ${VERSION} from ${DOWNLOAD_URL}..."
    if curl -sSL --fail -o "${TMPFILE}" "${DOWNLOAD_URL}" 2>/dev/null; then
        unzip -o "${TMPFILE}" tofu -d "${BIN_DIR}"
        chmod +x "${TOFU_BIN}"
        echo "Tofu ${VERSION} ready (auto-downloaded)."
        cache_binary
        exit 0
    fi
    echo "Auto-download failed for tofu ${VERSION}."
fi

cat >&2 <<EOF
ERROR: OpenTofu version '${VERSION}' is not available.

Checked: controller cache$([ -n "${CACHE_URL}" ] && echo " (${CACHE_URL})"), custom URL$([ "${AUTO_DOWNLOAD}" = "true" ] && echo ", ${DOWNLOAD_BASE_URL}").

To provide a custom binary, set INFRAKUBE_TOFU_DOWNLOAD_URL in your
Tofu resource's taskOptions:

    taskOptions:
    - for: ["init", "init-delete", "plan", "plan-delete", "apply", "apply-delete"]
      env:
      - name: INFRAKUBE_TOFU_DOWNLOAD_URL
        value: "https://github.com/opentofu/opentofu/releases/download/v${VERSION}/tofu_${VERSION}_linux_amd64.zip"
EOF
exit 1
