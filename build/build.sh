#!/bin/bash
# vrndis-go cross-compile build script.
# Targets: linux amd64/arm64/armv6/armv7, windows amd64, android arm64 (all 默认包含)。
# STUN server is integrated into vrndisd: use "vrndisd stun-server" command.

set -e

BUILD_DIR="./build/bin"

# ---- version + date: always dynamic, never hardcoded literals ----
if [ -d ".git" ] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    VERSION_SHORT=$(git rev-parse --short HEAD 2>/dev/null)
    COMMIT_TS=$(git show -s --format=%ct HEAD 2>/dev/null)
    if [ -n "$COMMIT_TS" ]; then
        VERSION_DATE=$(date -u -d "@$COMMIT_TS" +%Y-%m-%dT%H:%M:%SZ)
    else
        VERSION_DATE="unknown"
    fi
else
    VERSION_SHORT="unknown"
    VERSION_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
fi

VERSION_IMPORT="main"

# 版本号只来自 git tag，不在源码里硬编码：
#   已打 tag：v1.1.0；tag 后又有提交：v1.1.0-2-g379ded7；dirty：追加 -dirty
#   非 git 工作树（如解包的 tarball）：回退提交短哈希/unknown
gitversion=$(git describe --tags --always --dirty 2>/dev/null)
[ -z "$gitversion" ] && gitversion="${VERSION_SHORT:-unknown}"

# ---- target matrix ----
TARGETS="amd64 arm64 armv6 armv7 windows"
RELEASE=false

for arg in "$@"; do
    case $arg in
        amd64|arm64|armv6|armv7|windows) TARGETS="$arg" ;;
        android) TARGETS="android-arm64" ;;
        all) TARGETS="amd64 arm64 armv6 armv7 windows android-arm64" ;;
        release) RELEASE=true ;;
        clean)
            rm -rf "$BUILD_DIR"/*
            echo "build output cleaned"
            exit 0
            ;;
        *)
            echo "unknown argument: $arg" >&2
            echo "usage: $0 [amd64|arm64|armv6|armv7|windows|android|all] [release] [clean]" >&2
            exit 2
            ;;
    esac
done

mkdir -p "$BUILD_DIR"

# target_props <name> -> prints: GOOS GOARCH GOARM BINARY_NAME
target_props() {
    case "$1" in
        amd64)   echo "linux   amd64 0 vrndisd" ;;
        arm64)   echo "linux   arm64 0 vrndisd" ;;
        armv6)   echo "linux   arm   6 vrndisd" ;;
        armv7)   echo "linux   arm   7 vrndisd" ;;
        android-arm64) echo "android arm64 0 vrndisd" ;;
        windows) echo "windows amd64 0 vrndisd.exe" ;;
        *) return 1 ;;
    esac
}

build_one() {
    GOOS="$3" GOARCH="$4" GOARM="$5" CGO_ENABLED=0 \
        go build -o "$2" -ldflags "$LDFLAGS" "$1"
}

build_target() {
    local name="$1"
    set -- $(target_props "$name")
    local goos="$1" goarch="$2" goarm="$3" bin="$4"
    local output_dir="$BUILD_DIR/$name"
    mkdir -p "$output_dir"

    local ldflags="-s -w -X $VERSION_IMPORT.Version=$gitversion -X vrndis/internal/server.Version=$gitversion -X vrndis/internal/client.Version=$gitversion"
    [ "$RELEASE" = true ] && ldflags="$ldflags -s -w"
    LDFLAGS="$ldflags"

    local tag="$goos/$goarch"
    [ "$goarm" != "0" ] && tag="$tag/goarm$goarm"

    local runnable=false
    if [ "$goos" = "$(go env GOOS)" ] && [ "$goarch" = "$(go env GOARCH)" ]; then
        runnable=true
    fi

    # --- vrndisd ---
    echo "== building vrndisd ($tag) -> $output_dir/$bin"
    build_one ./cmd/vrndisd "$output_dir/$bin" "$goos" "$goarch" "$goarm"
    if [ "$runnable" = true ]; then
        [ -x "$output_dir/$bin" ] || { echo "ERROR: build failed for $name"; return 1; }
        echo "   version: $( "$output_dir/$bin" version 2>&1 | grep "vrndisd" || true )"
    elif [ "$goos" = "windows" ]; then
        file "$output_dir/$bin" | grep -q "PE32" \
            || { echo "ERROR: $output_dir/$bin is not a PE binary"; return 1; }
        echo "   ok: $(file -b "$output_dir/$bin") (cross-built, version check skipped)"
    else
        file "$output_dir/$bin" | grep -q "ELF" \
            || { echo "ERROR: $output_dir/$bin is not an ELF binary"; return 1; }
        echo "   ok: $(file -b "$output_dir/$bin" | cut -c1-70) (cross-built, version check skipped)"
    fi
}

for name in $TARGETS; do
    build_target "$name"
    echo ""
done

echo "build summary:"
echo "  git commit : $VERSION_SHORT"
echo "  commit date: $VERSION_DATE"
echo "  version    : $gitversion"
echo "  output dir : $BUILD_DIR"
echo ""
echo "artifacts:"
( cd "$BUILD_DIR" && for d in */; do
      [ -d "$d" ] || continue
      for f in "$d"*; do
          [ -f "$f" ] || continue
          printf '  %-28s %s\n' "$f" "$(file -b "$f" | cut -c1-60)"
      done
  done )
echo ""

echo "packing..."
tar -czf "./vrndis-go-build-${gitversion}.tar.gz" -C "$BUILD_DIR" .
echo "tarball: ./vrndis-go-build-${gitversion}.tar.gz"
echo ""
echo "done!"
