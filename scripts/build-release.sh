#!/bin/bash
# build-release.sh — 编译 pdf-tool + 打包 mutool 到 dist/<platform>/
#
# 用法：
#   ./scripts/build-release.sh           # 编译当前平台
#   ./scripts/build-release.sh all       # 编译所有已就绪的平台
#   ./scripts/build-release.sh darwin    # 仅编译 darwin（通用二进制）
#   ./scripts/build-release.sh win       # 仅编译 Windows
#
# 输出：
#   dist/darwin/
#     pdf-tool           (通用二进制：Apple Silicon + Intel)
#     mutool             (通用二进制：Apple Silicon + Intel)
#   dist/win/
#     pdf-tool.exe
#     mutool.exe
#   dist/linux/
#     pdf-tool
#     mutool
#
# 前置条件：
#   - Go 1.24+
#   - bund/darwin-arm64/mutool 和 bund/darwin-amd64/mutool（darwin 通用二进制需要）
#   - bund/windows-amd64/mutool.exe（Windows 需要）
#   - mingw-w64（编译 Windows 可能需要，当前用 CGO_ENABLED=0 可跳过）
#   - lipo（macOS 自带）

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BUND_DIR="$PROJECT_DIR/bund"
DIST_DIR="$PROJECT_DIR/dist"

export GOTELEMETRY=off

# ———————————————————————————————————————
# 编译 darwin 通用二进制（Apple Silicon + Intel）
# ———————————————————————————————————————
build_darwin() {
    echo ""
    echo "=== [darwin] 编译通用二进制 ==="

    local out_dir="$DIST_DIR/darwin"
    mkdir -p "$out_dir"

    # 1. 编译 arm64 pdf-tool
    local tmp_arm64="$(mktemp)"
    echo "  → 编译 arm64 pdf-tool（临时）"
    GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o "$tmp_arm64" "$PROJECT_DIR" 2>&1 | tail -3

    # 2. 编译 amd64 pdf-tool
    local tmp_amd64="$(mktemp)"
    echo "  → 编译 amd64 pdf-tool（临时）"
    GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o "$tmp_amd64" "$PROJECT_DIR" 2>&1 | tail -3

    # 3. lipo 合并为通用二进制
    echo "  → lipo 合并 pdf-tool"
    lipo -create -output "$out_dir/pdf-tool" "$tmp_arm64" "$tmp_amd64"
    rm "$tmp_arm64" "$tmp_amd64"

    echo "  pdf-tool: $out_dir/pdf-tool"
    file "$out_dir/pdf-tool" | awk '{print "    " $0}'

    # 4. lipo 合并 mutool
    if [ -f "$BUND_DIR/darwin-arm64/mutool" ] && [ -f "$BUND_DIR/darwin-amd64/mutool" ]; then
        echo "  → lipo 合并 mutool"
        lipo -create -output "$out_dir/mutool" \
            "$BUND_DIR/darwin-arm64/mutool" \
            "$BUND_DIR/darwin-amd64/mutool"
        chmod +x "$out_dir/mutool"
        echo "  mutool:   $out_dir/mutool"
        file "$out_dir/mutool" | awk '{print "    " $0}'
    else
        echo "  ⚠ darwin-arm64/amd64 mutool 缺失（跳过，运行 build-mutool.sh 补充）"
    fi

    # 5. 拷贝 gs（Ghostscript）— 从 bund/darwin-universal/ 取通用二进制
    if [ -f "$BUND_DIR/darwin-universal/gs" ]; then
        cp "$BUND_DIR/darwin-universal/gs" "$out_dir/gs"
        chmod +x "$out_dir/gs"
        echo "  gs:       $out_dir/gs"
        file "$out_dir/gs" | awk '{print "    " $0}'
    else
        echo "  ⚠ darwin-universal/gs 缺失（跳过，手动放到 bund/darwin-universal/ 目录补充）"
    fi
}

# ———————————————————————————————————————
# 编译 Windows（仅 amd64）
# ———————————————————————————————————————
build_win() {
    echo ""
    echo "=== [win] 编译 Windows (amd64) ==="

    local out_dir="$DIST_DIR/win"
    mkdir -p "$out_dir"

    # 编译 pdf-tool.exe（CGO_ENABLED=0，无需 mingw）
    echo "  → 编译 pdf-tool.exe"
    GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o "$out_dir/pdf-tool.exe" "$PROJECT_DIR" 2>&1 | tail -3
    echo "  pdf-tool: $out_dir/pdf-tool.exe"
    file "$out_dir/pdf-tool.exe" | awk '{print "    " $0}'

    # 拷贝 mutool.exe
    if [ -f "$BUND_DIR/windows-amd64/mutool.exe" ]; then
        cp "$BUND_DIR/windows-amd64/mutool.exe" "$out_dir/mutool.exe"
        chmod +x "$out_dir/mutool.exe"
        echo "  mutool:   $out_dir/mutool.exe"
        file "$out_dir/mutool.exe" | awk '{print "    " $0}'
    else
        echo "  ⚠ windows-amd64 mutool.exe 缺失（跳过，运行 build-mutool.sh 补充）"
    fi

    # 拷贝 gs.exe（Ghostscript）— 从 bund/ 取二进制
    if [ -f "$BUND_DIR/windows-amd64/gs.exe" ]; then
        cp "$BUND_DIR/windows-amd64/gs.exe" "$out_dir/gs.exe"
        chmod +x "$out_dir/gs.exe"
        echo "  gs.exe:   $out_dir/gs.exe"
        file "$out_dir/gs.exe" | awk '{print "    " $0}'
    else
        echo "  ⚠ windows-amd64 gs.exe 缺失（跳过，手动放到 bund/ 目录补充）"
    fi
}

# ———————————————————————————————————————
# 确定编译哪些平台
# ———————————————————————————————————————
TARGET="${1:-}"

if [ -z "$TARGET" ] || [ "$TARGET" = "auto" ]; then
    # 自动检测
    TARGET="darwin"
    echo "检测到当前平台: macOS → 编译 darwin"
fi

case "$TARGET" in
    darwin)
        build_darwin
        ;;
    win)
        build_win
        ;;
    all)
        echo "=== 编译所有可用平台 ==="
        build_darwin
        build_win
        ;;
    *)
        echo "⚠ 未知目标平台: $TARGET"
        echo "用法: $0 [darwin|win|all]"
        exit 1
        ;;
esac

# ———————————————————————————————————————
# 显示结果
# ———————————————————————————————————————
echo ""
echo "=== dist/ 目录结构 ==="
find "$DIST_DIR" -type f | sort | while read -r f; do
    size=$(ls -lh "$f" | awk '{print $5}')
    echo "  $f ($size)"
done

echo ""
echo "=== 完成 ==="
echo "各平台的 dist/<platform>/ 目录可直接分发（mutool 和 pdf-tool 在同目录）"