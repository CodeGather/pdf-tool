#!/bin/bash
# build-mutool.sh — 从源码编译 mutool (MuPDF) 跨平台二进制
#
# 用法：
#   ./scripts/build-mutool.sh              # 编译当前平台
#   ./scripts/build-mutool.sh all          # 编译所有平台（需交叉编译器）
#   ./scripts/build-mutool.sh darwin-arm64 # 仅编译指定平台
#
# 目标平台：
#   darwin-arm64  — macOS Apple Silicon  ✅ 本机编译
#   darwin-amd64  — macOS Intel          ✅ 本机交叉编译 (Xcode)
#   linux-amd64   — Linux x86_64         ❌ 需在 Linux 上编译
#   windows-amd64 — Windows x86_64       ✅ 本机交叉编译 (mingw-w64)
#
# 前置条件：
#   - macOS: Xcode Command Line Tools (xcode-select --install)
#   - Windows 交叉编译: mingw-w64 (brew install mingw-w64)
#   - Linux: 需在 Linux 机器上运行此脚本
#   - curl, tar

set -euo pipefail

VERSION="${1:-1.27.2}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BUND_DIR="$PROJECT_DIR/bund"

# ———————————————————————————————————————
# 编译 darwin-arm64 — macOS Apple Silicon
# ———————————————————————————————————————
build_darwin_arm64() {
    local work="/tmp/mupdf-build-darwin-arm64"
    echo "=== [darwin-arm64] 编译中 ==="
    prepare_source "$work"
    cd "$work"
    rm -rf build
    make apps build=release brotli=no HAVE_LIBCRYPTO=no HAVE_GLUT=no 2>&1 | tail -3
    install_binary "$work/build/release/mutool" "darwin-arm64/mutool"
    rm -rf "$work"
}

# ———————————————————————————————————————
# 编译 darwin-amd64 — macOS Intel（在 ARM 上交叉编译）
# ———————————————————————————————————————
build_darwin_amd64() {
    local work="/tmp/mupdf-build-darwin-amd64"
    echo "=== [darwin-amd64] 交叉编译中 ==="
    prepare_source "$work"
    cd "$work"
    rm -rf build
    make apps build=release brotli=no HAVE_LIBCRYPTO=no \
        HAVE_GLUT=no \
        CC="clang -arch x86_64" \
        CXX="clang++ -arch x86_64" \
        2>&1 | tail -3
    install_binary "$work/build/release/mutool" "darwin-amd64/mutool"
    rm -rf "$work"
}

# ———————————————————————————————————————
# 编译 linux-amd64 — Linux x86_64
# （需在 Linux 机器上运行）
# ———————————————————————————————————————
build_linux_amd64() {
    if [[ "$(uname)" != "Linux" ]]; then
        echo "⚠ [linux-amd64] 跳过：需要在 Linux 上编译"
        echo "  在 Linux 上运行："
        echo "    curl -sL 'https://github.com/ArtifexSoftware/mupdf-downloads/releases/download/${VERSION}/mupdf-${VERSION}-source.tar.gz' | tar xz"
        echo "    cd mupdf-${VERSION}"
        echo "    make apps build=release brotli=no"
        echo "    cp build/release/mutool <pdf-tool>/bund/linux-amd64/"
        return
    fi
    local work="/tmp/mupdf-build-linux-amd64"
    echo "=== [linux-amd64] 编译中 ==="
    prepare_source "$work"
    cd "$work"
    rm -rf build
    make apps build=release brotli=no 2>&1 | tail -3
    install_binary "$work/build/release/mutool" "linux-amd64/mutool"
    rm -rf "$work"
}

# ———————————————————————————————————————
# 编译 windows-amd64 — Windows x86_64（mingw 交叉编译）
# ———————————————————————————————————————
build_windows_amd64() {
    if ! command -v x86_64-w64-mingw32-gcc &>/dev/null; then
        echo "⚠ [windows-amd64] 跳过：缺少 mingw-w64 交叉编译器"
        echo "  安装：brew install mingw-w64"
        return
    fi
    local work="/tmp/mupdf-build-windows-amd64"
    echo "=== [windows-amd64] 交叉编译中 ==="
    prepare_source "$work"
    cd "$work"
    rm -rf build
    make apps build=release brotli=no HAVE_LIBCRYPTO=no HAVE_GLUT=no \
        CC="x86_64-w64-mingw32-gcc -msse4.1" \
        CXX="x86_64-w64-mingw32-g++ -msse4.1" \
        AR=x86_64-w64-mingw32-ar \
        RANLIB=x86_64-w64-mingw32-ranlib \
        LDREMOVEUNREACH="" \
        EXE=.exe \
        2>&1 | tail -3
    install_binary "$work/build/release/mutool.exe" "windows-amd64/mutool.exe"
    rm -rf "$work"
}

# ———————————————————————————————————————
# 辅助函数
# ———————————————————————————————————————

prepare_source() {
    local dest="$1"
    if [ -d "$dest" ]; then
        echo "  源码已存在: $dest"
        return
    fi
    local src_url="https://github.com/ArtifexSoftware/mupdf-downloads/releases/download/${VERSION}/mupdf-${VERSION}-source.tar.gz"
    echo "  下载源码: $src_url"
    mkdir -p "$dest"
    curl -sL "$src_url" -o "/tmp/mupdf-${VERSION}-source.tar.gz"
    tar xzf "/tmp/mupdf-${VERSION}-source.tar.gz" -C "$dest" --strip-components=1
    echo "  解压完成"
}

install_binary() {
    local src="$1"
    local dest_rel="$2"
    local dest="$BUND_DIR/$dest_rel"
    mkdir -p "$(dirname "$dest")"
    cp "$src" "$dest"
    chmod +x "$dest"
    echo "  ✓ $dest_rel ($(file "$dest" | awk -F: '{print $2}'))"
}

# ———————————————————————————————————————
# 主逻辑
# ———————————————————————————————————————

mkdir -p "$BUND_DIR"

case "${2:-}" in
    all)
        build_darwin_arm64
        build_darwin_amd64
        build_linux_amd64
        build_windows_amd64
        ;;
    darwin-arm64) build_darwin_arm64 ;;
    darwin-amd64) build_darwin_amd64 ;;
    linux-amd64)  build_linux_amd64 ;;
    windows-amd64) build_windows_amd64 ;;
    *)
        # 自动检测当前平台
        case "$(uname -s)/$(uname -m)" in
            Darwin/arm64)  build_darwin_arm64 ;;
            Darwin/x86_64) build_darwin_amd64 ;;
            Linux/x86_64)  build_linux_amd64 ;;
            *) echo "⚠ 未知平台: $(uname -s)/$(uname -m)，请指定平台" ;;
        esac
        ;;
esac

echo ""
echo "=== bund/ 目录内容 ==="
find "$BUND_DIR" -type f -not -name '*.md' -exec file {} \;
echo ""
echo "=== 完成 ==="
echo "运行 ./pdf-tool 时会自动检测 bund/ 中的 mutool"
echo "如需测试捆绑版本，临时移除系统 mutool："
echo "  PATH=/usr/bin:/bin ./pdf-tool -i input.pdf -o output -l"