#!/bin/sh
# Install ssh-agent-relay.
#
# Downloads the prebuilt binary for this OS/arch from the GitHub release, verifies
# its checksum, and installs it. Falls back to `go install` when the download is
# unavailable and a Go toolchain is present.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/clamoriniere/ssh-agent-relay/main/install.sh | sh
#   sh install.sh --version v0.1.0 --bin-dir ~/.local/bin
#
# Env overrides:
#   SSH_AGENT_RELAY_VERSION   release tag, or "latest" (default)
#   SSH_AGENT_RELAY_BIN_DIR   install dir (default: ~/.local/bin)
#   GITHUB_TOKEN / GH_TOKEN   used for API + asset download while the repo is private
set -eu

REPO="clamoriniere/ssh-agent-relay"
VERSION="${SSH_AGENT_RELAY_VERSION:-latest}"
BIN_DIR="${SSH_AGENT_RELAY_BIN_DIR:-${HOME}/.local/bin}"
NAME="ssh-agent-relay"

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version) VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--bin-dir) BIN_DIR="$2"; shift 2 ;;
	--bin-dir=*) BIN_DIR="${1#*=}"; shift ;;
	-h | --help)
		sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "install.sh: unknown argument: $1" >&2
		exit 2
		;;
	esac
done

log() { echo "install.sh: $*" >&2; }
die() { log "$*"; exit 1; }

have() { command -v "$1" >/dev/null 2>&1; }

# --- platform -------------------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
darwin | linux) ;;
*) die "unsupported OS: $os" ;;
esac

arch="$(uname -m)"
case "$arch" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported arch: $arch" ;;
esac

TOKEN="${GITHUB_TOKEN:-${GH_TOKEN:-}}"

auth_header() {
	[ -n "$TOKEN" ] && printf 'Authorization: Bearer %s' "$TOKEN"
}

fetch() {
	# fetch <url> <dest>  (dest "-" => stdout)
	_url="$1"
	_dest="$2"
	_hdr="$(auth_header || true)"
	if have curl; then
		if [ "$_dest" = "-" ]; then
			curl -fsSL ${_hdr:+-H "$_hdr"} "$_url"
		else
			curl -fsSL ${_hdr:+-H "$_hdr"} -o "$_dest" "$_url"
		fi
	elif have wget; then
		if [ "$_dest" = "-" ]; then
			wget -qO- ${_hdr:+--header="$_hdr"} "$_url"
		else
			wget -qO "$_dest" ${_hdr:+--header="$_hdr"} "$_url"
		fi
	else
		die "need curl or wget"
	fi
}

sha256() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}';
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}';
	else die "need sha256sum or shasum"; fi
}

resolve_tag() {
	[ "$VERSION" != "latest" ] && { echo "$VERSION"; return; }
	_api="https://api.github.com/repos/${REPO}/releases/latest"
	_json="$(fetch "$_api" - 2>/dev/null || true)"
	echo "$_json" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1
}

go_install() {
	have go || die "download failed and 'go' is not available for a source build"
	_ref="$VERSION"
	[ "$_ref" = "latest" ] && _ref="latest"
	log "building from source: go install ${REPO}@${_ref}"
	GOBIN="$BIN_DIR" GOFLAGS=-mod=mod go install "github.com/${REPO}@${_ref}"
}

mkdir -p "$BIN_DIR"

TAG="$(resolve_tag || true)"
if [ -z "$TAG" ]; then
	log "could not resolve a release tag (private repo without a token, or no releases yet)"
	go_install
	log "installed $("$BIN_DIR/$NAME" version 2>/dev/null || echo "$NAME") to $BIN_DIR"
	exit 0
fi

ARCHIVE="${NAME}_${os}_${arch}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${TAG}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

log "downloading ${ARCHIVE} @ ${TAG}"
if ! fetch "${BASE}/${ARCHIVE}" "${TMP}/${ARCHIVE}" 2>/dev/null; then
	log "asset download failed"
	go_install
	log "installed $("$BIN_DIR/$NAME" version 2>/dev/null || echo "$NAME") to $BIN_DIR"
	exit 0
fi

if fetch "${BASE}/checksums.txt" "${TMP}/checksums.txt" 2>/dev/null; then
	want="$(grep " ${ARCHIVE}\$" "${TMP}/checksums.txt" | awk '{print $1}')"
	got="$(sha256 "${TMP}/${ARCHIVE}")"
	[ -n "$want" ] || die "checksums.txt has no entry for ${ARCHIVE}"
	[ "$want" = "$got" ] || die "checksum mismatch: want $want, got $got"
	log "checksum ok"
else
	log "warning: checksums.txt not found, skipping verification"
fi

tar -xzf "${TMP}/${ARCHIVE}" -C "$TMP" "$NAME"
install -m 0755 "${TMP}/${NAME}" "${BIN_DIR}/${NAME}"

log "installed $("${BIN_DIR}/${NAME}" version) to ${BIN_DIR}"
case ":${PATH}:" in
*":${BIN_DIR}:"*) ;;
*) log "note: ${BIN_DIR} is not on your PATH" ;;
esac
