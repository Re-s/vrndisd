#!/bin/bash
# vrndis-go ARM 构建脚本
# 支持构建 Linux ARM64、ARMv6、ARMv7 静态二进制

set -e

BUILD_DIR="./build/bin"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
VERSION=${VERSION:-"1.0.0"}

# 支持的架构
ARCHS=("arm64" "armv6" "armv7")
CLEAN=false

# 解析命令行参数
while [[ $# -gt 0 ]]; do
    case $1 in
        arm64)
            ARCHS=("arm64")
            shift
            ;;
        armv6)
            ARCHS=("armv6")
            shift
            ;;
        armv7)
            ARCHS=("armv7")
            shift
            ;;
        all)
            shift
            ;;
        clean)
            CLEAN=true
            shift
            ;;
        package)
            shift
            ;;
        *)
            echo "用法: $0 [arm64|armv6|armv7|all|clean|package]"
            exit 1
            ;;
    esac
done

compile_arm() {
    local arch=$1
    local GOOS=linux
    local GOARCH
    local GOARM=""
    local CC=""
    
    case $arch in
        arm64)
            GOARCH=arm64
            ;;
        armv6)
            GOARCH=arm
            GOARM=6
            CC="arm-linux-gnueabihf-gcc"
            ;;
        armv7)
            GOARCH=arm
            GOARM=7
            CC="arm-linux-gnueabihf-gcc"
            ;;
    esac

    echo "🔧 编译 arm/$arch (GOARCH=$GOARCH, GOARM=${GOARM:-'N/A'})..."
    
    export GOOS=$GOOS
    export GOARCH=$GOARCH
    [ -n "$GOARM" ] && export GOARM=$GOARM
    [ -n "$CC" ] && export CC=$CC
    
    export CGO_ENABLED=1
    
    mkdir -p "$BUILD_DIR/$arch"
    
    go build -o "$BUILD_DIR/$arch/vrndisd" -ldflags "-s -w -X main.Version=$VERSION" ./cmd/vrndisd
    
    echo "✅ arm/$arch 构建完成 -> $BUILD_DIR/$arch/vrndisd"
    
    unset GOOS GOARCH GOARM CC CGO_ENABLED
}

if [ "$CLEAN" = true ]; then
    echo "🧹 清理构建目录..."
    rm -rf "$BUILD_DIR"/*
    echo "✅ 清理完成"
    exit 0
fi

for arch in "${ARCHS[@]}"; do
    compile_arm "$arch"
    echo ""
done

echo "📋 构建报告"
echo "=========="
ls -lh "$BUILD_DIR"/* 2>/dev/null || echo "无构建产物"
echo "版本: $VERSION"
echo "时间: $(date)"

if [ "$1" == "package" ] || [ "$2" == "package" ]; then
    echo "📦 打包构建产物..."
    tar -czvf "./vrndis-go-arm-build-${TIMESTAMP}.tar.gz" -C "$BUILD_DIR" .
    echo "✅ 压缩包生成: ./vrndis-go-arm-build-${TIMESTAMP}.tar.gz"
fi

echo "🎉 所有构建完成！"
