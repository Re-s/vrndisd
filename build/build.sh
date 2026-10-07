#!/bin/bash
# vrndis-go 构建脚本 (支持 Git 版本嵌入)

set -e

BUILD_DIR="./build/bin"

# 获取版本信息
if [ -d ".git" ]; then
    VERSION_SHORT=$(git rev-parse --short HEAD 2>/dev/null)
    VERSION_DATE=$(git log -1 --format=%ai 2>/dev/null | cut -d' ' -f1)
else
    VERSION_SHORT="unknown"
    VERSION_DATE="unknown"
fi

VERSION_IMPORT="vrndis/main"
VERSION_LONG="${VERSION_SHORT}-${VERSION_DATE}"

default_vrndisd_version="1.0.0+${VERSION_SHORT}"
gitversion="$default_vrndisd_version"

# 支持的架构
ARCHS="arm64 armv6 armv7 amd64"
RELEASE=false
CLEAN=false

# 解析参数
for arg in "$@"; do
    case $arg in
        arm64) ARCHS="arm64" ;;
        armv6) ARCHS="armv6" ;;
        armv7) ARCHS="armv7" ;;
        amd64) ARCHS="amd64" ;;
        all) ARCHS="arm64 armv6 armv7 amd64" ;;
        release) RELEASE=true ;;
        clean) 
            rm -rf "$BUILD_DIR"/*
            echo "🧹 清理构建目录完成"
            exit 0
            ;;
    esac
done

mkdir -p "$BUILD_DIR"

# 编译函数
build_arch() {
    local arch="$1"
    local output_dir="$BUILD_DIR/$arch"
    mkdir -p "$output_dir"
    
    echo "🔧 编译 vrndis-go $arch..."
    
    case $arch in
        arm64)
            GOOS=linux GOARCH=arm64 ;;
        armv6)
            GOOS=linux GOARCH=arm GOARM=6 ;;
        armv7)
            GOOS=linux GOARCH=arm GOARM=7 ;;
        amd64)
            GOOS=linux GOARCH=amd64 ;;
    esac

    # 设置 LDFLAGS 嵌入版本信息
    local ldflags="-s -w -X $VERSION_IMPORT.Version=$gitversion"
    [ "$RELEASE" = true ] && ldflags="$ldflags -s -w"

    # 执行构建
    go build -o "$output_dir/vrndisd" -ldflags "$ldflags" ./cmd/vrndisd
    
    # 验证构建产物
    if [ -x "$output_dir/vrndisd" ]; then
        echo "✅ $arch 构建完成 → $output_dir/vrndisd"
        # 在输出中显示版本信息
        version_output=$("$output_dir/vrndisd" version 2>&1 | grep "vrndisd")
        echo "   版本: $version_output"
    else
        echo "❌ $arch 构建失败!"
        return 1
    fi
}

# 执行所有架构构建
for arch in $ARCHS; do
    build_arch "$arch"
    echo ""
done

# 显示构建信息
echo "📋 构建统计:"
echo "   Git 哈希: $VERSION_SHORT"
echo "   构建日期: $VERSION_DATE"
echo "   版本格式: $gitversion"
echo "   产物目录: $BUILD_DIR"

# 压缩输出
echo "📦 生成构建压缩文件..."
tar -czf "./vrndis-go-build-${VERSION_SHORT}.tar.gz" -C "$BUILD_DIR" .
echo "✅ 压缩包: ./vrndis-go-build-${VERSION_SHORT}.tar.gz"
echo ""
echo "🎉 所有构建完成!"
