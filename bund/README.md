# bund/ — 捆绑二进制（mutool + Ghostscript）

## 目录结构

```
bund/
├── darwin-arm64/mutool          # macOS Apple Silicon (arm64) mutool
├── darwin-amd64/mutool          # macOS Intel (x86_64) mutool
├── darwin-universal/gs          # macOS 通用二进制 (arm64+x86_64) Ghostscript
├── linux-amd64/mutool           # Linux x86_64 mutool
├── windows-amd64/mutool.exe     # Windows x86_64 mutool
├── windows-amd64/gs.exe         # Windows x86_64 Ghostscript (gswin64c.exe)
└── README.md                    # 本文件
```

## mutool 查找方式

pdf-tool 的 `findMutool()` 函数自动按以下顺序查找：

1. PATH 环境变量（系统安装的 mutool）
2. **程序同级 `bund/<os>-<arch>/mutool`**（本目录的跨平台版本）
3. **程序同级 `bund/mutool`**（简单捆绑回退）
4. `/opt/homebrew/bin/mutool`（Homebrew）

## Ghostscript 查找方式

pdf-tool 的 `findGS()` 函数自动按以下顺序查找：

1. PATH 环境变量（系统安装的 gs）
2. **程序同级目录 `gs`**（和 pdf-tool 放一起）
3. **程序同级 `bund/<os>-<arch>/gs`**（跨平台捆绑）
4. **程序同级 `bund/gs`**（简单捆绑回退）
5. `/opt/homebrew/bin/gs`（Homebrew）

## 构建说明

### 状态

| 目标平台 | 状态 | 方式 | 前置条件 |
|---------|------|------|---------|
| darwin-arm64 | ✅ 已编译 | macOS ARM 本机构建 | macOS + Xcode CLT |
| darwin-amd64 | ✅ 已编译 | macOS ARM → x86_64 交叉编译 | macOS + Xcode CLT |
| linux-amd64 | ❌ 待编译 | 需在 Linux 上运行脚本 | Linux + gcc, make |
| windows-amd64 | ✅ 已编译 | macOS → Windows mingw 交叉编译 | mingw-w64 (`brew install mingw-w64`)

### 编译命令速查

**macOS ARM64**（本机）：
```bash
make apps build=release brotli=no HAVE_LIBCRYPTO=no HAVE_GLUT=no
cp build/release/mutool bund/darwin-arm64/
```

**macOS Intel**（在 ARM Mac 上交叉编译）：
```bash
make apps build=release brotli=no HAVE_LIBCRYPTO=no HAVE_GLUT=no \
  CC="clang -arch x86_64" CXX="clang++ -arch x86_64"
cp build/release/mutool bund/darwin-amd64/
```

**Linux amd64**（在 Linux 上）：
```bash
make apps build=release brotli=no
cp build/release/mutool bund/linux-amd64/
```

**Windows amd64**（在 macOS 上交叉编译）：
```bash
make apps build=release brotli=no HAVE_LIBCRYPTO=no HAVE_GLUT=no \
  CC="x86_64-w64-mingw32-gcc -msse4.1" \
  CXX="x86_64-w64-mingw32-g++ -msse4.1" \
  AR=x86_64-w64-mingw32-ar \
  RANLIB=x86_64-w64-mingw32-ranlib \
  LDREMOVEUNREACH="" \
  EXE=.exe
cp build/release/mutool.exe bund/windows-amd64/
```

### 验证

```bash
# 验证二进制
file bund/darwin-arm64/mutool
# 输出: Mach-O 64-bit executable arm64

file bund/darwin-amd64/mutool
# 输出: Mach-O 64-bit executable x86_64

# 测试捆绑版 mutool (临时移除系统 mutool)
PATH=/usr/bin:/bin ./pdf-tool -i test.pdf -o output -l
# 日志应显示: "found mutool at: .../bund/darwin-arm64/mutool"
```

## 版本

当前版本：**1.27.2**（2025年10月发布）

升级方式：
1. 访问 https://mupdf.com/releases 查看最新版本
2. 修改 `scripts/build-mutool.sh` 中的 VERSION 变量
3. 重新运行脚本