#!/bin/sh
# Builds the bangdb CLI with a version that grows with every commit:
#   <VERSION file>.<number of commits>[-dirty]   e.g. 0.6.43
#
#   scripts/build.sh                 build for this machine into dist/
#   scripts/build.sh all             also cross-build Windows, macOS, Linux
#   scripts/build.sh install         go install with the version stamped
set -eu
cd "$(dirname "$0")/.."

base=$(tr -d ' \r\n' < VERSION)
count=$(git rev-list --count HEAD)
commit=$(git rev-parse --short HEAD)
version="$base.$count"
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	version="$version-dirty" # uncommitted changes: not a reproducible build
fi
built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
pkg=github.com/knoxxr/bangdb
ldflags="-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit -X $pkg.BuildTime=$built"

build() { # os arch
	ext=""
	[ "$1" = windows ] && ext=.exe
	out="dist/bangdb-$version-$1-$2$ext"
	GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/bangdb
	echo "$out"
}

mkdir -p dist
case "${1:-}" in
install)
	go install -trimpath -ldflags "$ldflags" ./cmd/bangdb
	echo "installed bangdb $version"
	;;
all)
	for t in windows/amd64 windows/arm64 darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
		build "${t%/*}" "${t#*/}"
	done
	;;
*)
	build "$(go env GOOS)" "$(go env GOARCH)"
	;;
esac
echo "version $version ($commit, $built)"
