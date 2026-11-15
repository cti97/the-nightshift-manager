#!/bin/sh
# Build nsm for the six supported targets into dist/, checksum what was built,
# and refuse to call a partial build a release.
#
#   dist/nsm-<version>-<os>-<arch>[.exe]
#   dist/SHA256SUMS
#   dist/BUILD-INFO
#
# Rules:
#   - dist/ starts empty and is swapped in only after EVERYTHING succeeds.
#   - The artifact count must equal the target count.
#   - Every artifact's architecture is asserted with file(1), so "compiled"
#     cannot mean "host build renamed".
#   - CGO is off: pure-Go SQLite is what makes one-command cross-compiling work.
set -eu

version="${1:-dev}"
root="$(cd "$(dirname "$0")/.." && pwd)"
stage="$root/dist.new"
dist="$root/dist"

targets="darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"
count=0
for t in $targets; do count=$((count + 1)); done

# A failed run must not leave a half-built staging directory behind to be
# mistaken for a release. dist/ itself is only touched at the very end.
trap 'rm -rf "$stage"' EXIT

rm -rf "$stage"
mkdir -p "$stage"

build_one() {
	os="$1"; arch="$2"
	out="$stage/nsm-$version-$os-$arch"
	if [ "$os" = "windows" ]; then
		out="$out.exe"
	fi
	( cd "$root" && env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		go build -trimpath \
			-ldflags "-s -w -X github.com/cti97/the-nightshift-manager/internal/version.Version=$version" \
			-o "$out" ./cmd/nsm )
}

for t in $targets; do
	os="${t%/*}"; arch="${t#*/}"
	build_one "$os" "$arch"
done

built=$(find "$stage" -type f -name 'nsm-*' | wc -l | tr -d ' ')
[ "$built" -eq "$count" ] || { echo "built $built artifacts, expected $count" >&2; exit 1; }

# The host artifact must actually run and name its version.
host="$stage/nsm-$version-$(go env GOOS)-$(go env GOARCH)"
[ -x "$host" ] || { echo "host artifact missing: $host" >&2; exit 1; }
"$host" version | grep -q "$version" || { echo "host artifact does not report $version" >&2; exit 1; }

# Architecture assertion: format and arch tokens, in any order (Apple's file(1)
# and libmagic word the same Mach-O differently).
for t in $targets; do
	os="${t%/*}"; arch="${t#*/}"
	f="$stage/nsm-$version-$os-$arch"
	if [ "$os" = "windows" ]; then
		f="$f.exe"
	fi
	desc=$(file -b "$f" 2>&1) || { echo "cannot inspect $f: $desc" >&2; exit 1; }
	case "$os/$arch" in
		darwin/amd64)  echo "$desc" | grep -q 'Mach-O' && echo "$desc" | grep -qiE 'x86[-_]64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		darwin/arm64)  echo "$desc" | grep -q 'Mach-O' && echo "$desc" | grep -qiE 'arm64|aarch64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		linux/amd64)   echo "$desc" | grep -q 'ELF'    && echo "$desc" | grep -qiE 'x86[-_]64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		linux/arm64)   echo "$desc" | grep -q 'ELF'    && echo "$desc" | grep -qiE 'arm64|aarch64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		windows/amd64) echo "$desc" | grep -q 'PE32+'  && echo "$desc" | grep -qiE 'x86[-_]64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		windows/arm64) echo "$desc" | grep -q 'PE32+'  && echo "$desc" | grep -qiE 'arm64|aarch64' || { echo "bad arch: $f: $desc" >&2; exit 1; } ;;
		*)             echo "no architecture assertion for target $t" >&2; exit 1 ;;
	esac
done

# shasum is macOS, sha256sum is Linux; the CI runner and the laptop disagree.
if command -v shasum >/dev/null 2>&1; then
	( cd "$stage" && shasum -a 256 nsm-* > SHA256SUMS )
else
	( cd "$stage" && sha256sum nsm-* > SHA256SUMS )
fi
( cd "$stage" && { echo "version: $version"; go version; echo "targets: $targets"; } > BUILD-INFO )

rm -rf "$dist"
mv "$stage" "$dist"
echo "released $version: $count artifacts in dist/"
