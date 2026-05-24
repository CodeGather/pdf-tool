# PDF Tool

Version: v1.0.4

基于 Go + go-fitz (MuPDF) + pdfcpu 的 PDF 图片提取工具。支持多种提取策略自动路由、CMYK/YCCK JPEG 正确色彩还原、以及 macOS 上的进程级并行渲染加速。

## 快速开始

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
./pdf-tool -i input.pdf -o output
```

## 所有命令行参数

### 核心参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-i` | `input.pdf` | 输入 PDF 文件路径 |
| `-o` | `output` | 输出目录 |
| `-f` | `png` | 输出图片格式：`png`、`jpg` / `jpeg` |
| `-dpi` | `300` | 渲染 DPI（仅影响整页渲染/裁剪路径） |

### 编码与质量

| 参数 | 简写 | 默认值 | 说明 |
|------|------|--------|------|
| `-quality` | `-q` | `95` | JPEG 编码质量 1-100。仅对整页渲染路径有效（直接拷贝的 JPEG 不受影响）。降低质量可显著缩小文件并加快编码：`-q 50` 比 `-q 95` 文件缩小约 4x，编码提速约 15% |

### 并行渲染

| 参数 | 简写 | 默认值 | 说明 |
|------|------|--------|------|
| `-workers` | `-w` | `1` | 并行渲染的进程数。仅当路由为 `render-whole-page`（页面本身就是整图）且页数 ≥ 4 时生效。例：`-w 4` 同时启动 4 个子进程各渲染一部分页面 |
| `-worker-start` | — | `0` | 内部参数。子进程通过此参数收到分配的起始页号（从 1 开始），0 表示渲染全部页面 |
| `-worker-end` | — | `0` | 内部参数。子进程通过此参数收到分配的结束页号（含） |

> 为什么是进程级并行？macOS 上 CGo 库（如 go-fitz/MuPDF）无法安全地在 goroutine 中并发调用，会触发 `semasleep on Darwin signal stack` 崩溃。进程级并行通过 `os/exec` 启动独立子进程绕过此限制，每个进程拥有自己的 MuPDF CGo 上下文和信号栈。实测 71 页 PDF：串行 13.6s → 4 进程 6.1s（2.23x 加速）。

### 调试与诊断

| 参数 | 简写 | 说明 |
|------|------|------|
| `-log` | `-l` | 启用调试日志输出 |
| `-meta` | `-m` | 打印每张图片的宽高信息 |
| `-meta-json` | `-m-json` | 以 JSON 格式输出图片元数据 |
| `-timing` | `-t` | 打印每个阶段的耗时信息；与 `-meta-json` 同时使用时，耗时写入记录的 `time` 字段 |
| `-p` | — | 打印处理进度百分比（0-100），仅合并模式有效 |

### 合并模式

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-merge` | `false` | 启用 PDF 合并模式（此时 `-o` 为输出文件，非目录） |
| `-merge-dir` | `""` | 待合并 PDF 所在目录 |
| `-merge-inputs` | `""` | 逗号分隔的待合并 PDF 文件列表 |
| `-merge-glob` | `*.pdf` | 合并模式下的文件匹配模式 |
| `-merge-chunk-size` | `50` | 每批合并的 PDF 数量，大量文件时建议降低 |
| `-merge-divider` | `false` | 在合并文件之间插入空白分隔页 |

## 用法示例

### 基础用法

```bash
# 默认 PNG 输出，300 DPI
./pdf-tool -i sample.pdf -o out

# JPG 输出 + 高质量
./pdf-tool -i sample.pdf -o out -f jpg -dpi 300

# 高 DPI（适合印刷级图片）
./pdf-tool -i sample.pdf -o out -f jpg -dpi 450

# 低质量小文件（适合缩略图/预览）
./pdf-tool -i sample.pdf -o out -f jpg -q 50
```

### 并行渲染加速

```bash
# 4 进程并行渲染多页 PDF（仅整页渲染路径有效）
./pdf-tool -i sample.pdf -o out -f jpg -w 4

# 8 进程 + 低质量，最大化吞吐
./pdf-tool -i sample.pdf -o out -f jpg -w 8 -q 50

# 并行 + 耗时观察
./pdf-tool -i sample.pdf -o out -f jpg -w 4 -t
```

### 调试与诊断

```bash
# 调试日志 + 耗时
./pdf-tool -i sample.pdf -o out -l -t

# 查看图片元数据
./pdf-tool -i sample.pdf -o out -l -m

# JSON 格式元数据
./pdf-tool -i sample.pdf -o out -l -m-json

# 完整调试
./pdf-tool -i sample.pdf -o out -l -t -m-json
```

### 合并 PDF

```bash
# 合并整个目录
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf

# 指定文件列表
./pdf-tool -merge -merge-inputs 1.pdf,2.pdf,3.pdf -o merged.pdf

# 大量文件分批合并
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -merge-chunk-size 25

# 合并 + 进度 + 日志
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -p -l -t
```

## 架构说明

### 路由机制

PDF 工具会根据文档结构自动选择最佳提取策略：

```
convertPDFToImages()
  ├─ classifyPDFDocument() → 静态分析得到路由
  │
  ├─ routeRenderCropComplexTransparency
  │   └─ renderCropPDF(): 渲染整页 → 连通域分析 → 裁剪主体区域
  │      适用于：复杂透明度 + Form XObject，无法直接提取
  │
  ├─ routeRenderWholePageImage
  │   ├─ 串行 (w=1 或 页数<4)
  │   │   └─ renderWholePagePDF(): 复用 doc 逐页渲染
  │   └─ 并行 (w>1 且 页数≥4)
  │       └─ renderWholePageParallel()
  │           ├─ worker 0: spawn -worker-start=1 -worker-end=N1
  │           ├─ worker 1: spawn -worker-start=N1+1 -worker-end=N2
  │           └─ ...
  │       适用于：扫描件、满版大图
  │
  └─ routeDirectExtractTransparency / MultiImageStack / SingleObject
      └─ extractDirectImages(): 直接从对象流提取嵌入图片
          ├─ writeDirectImageFast(): JPEG/JPX 快速拷贝
          ├─ writeDirectImage(): 逐对象解码 → 编码写盘
          └─ renderSinglePageCrop(): CMYK JPEG 回退（MuPDF 渲染→裁剪）
```

### 色彩空间处理

- **RGB JPEG/PNG**：直接拷贝或解码重建，最快路径
- **CMYK JPEG**：通过 isCMYKJPEG() 从 JPEG SOF marker 检测 4 分量 → 走 MuPDF 渲染整页后裁剪（避免 sips/ImageMagick/Pillow 对 Adobe YCCK 的错误转换）
- **JPXDecode (JPEG2000)**：pdfcpu 内部解码 + 颜色空间转换，消除外部命令依赖
- **Gray/8-bit**：快速重建为 PNG

### 性能优化

1. **Doc 复用**：`renderWholePagePDF` 打开一次 PDF 文件，所有页面共用 `fitz.Document`，避免每页重复 `fitz.New()`
2. **JPEG 质量可配**：`-quality N` 控制 JPEG 编码质量，q=50 比 q=95 编码速度快约 15%，文件缩小约 4x
3. **进程级并行**（整页渲染）：`-w N` 启动 N 个子进程分担页面渲染，实测 71 页 4 进程加速 2.23x。仅对 `render-whole-page` 路由生效
4. **对象级 goroutine 并行**（直取路径）：`extractDirectImages` 内部使用 `runtime.NumCPU()` 个 goroutine 并行提取同一页内的多个图片对象。pdfcpu 是纯 Go 库，goroutine 并行安全，macOS 上不会触发 CGo 信号栈问题。实测 20 对象单页 PDF 从 6.8s 降到 1.07s（6.4x）
5. **路由分流**：先做 PDF 结构静态分析，对能直接提取的文件不走渲染路径。避免对已有嵌入图片的 PDF 做不必要的全页渲染

### 并行渲染说明

为什么不用 goroutine：
- macOS 上 CGo 库（MuPDF）的信号栈与 goroutine 调度冲突
- goroutine 并行调用 `fitz.New()` 或 `doc.ImageDPI()` 会导致 `semasleep on Darwin signal stack` 崩溃

进程级并行 vs goroutine 并行：

| 特性 | goroutine 并行 | 进程级并行 |
|------|---------------|-----------|
| macOS 兼容性 | ❌ 崩溃 | ✅ 稳定 |
| 每 worker 独立上下文 | ✅ 共享进程空间 | ✅ 完全隔离 |
| 启动开销 | ~1µs | ~50ms |
| 页面分配 | 需保证 MuPDF 线程安全 | 各开各的 doc |
| 适用场景 | Linux 可用 | 全平台 |

## 构建

### 本地构建

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
```

### 交叉编译

```bash
# macOS Intel/ARM
GOTELEMETRY=off GOCACHE="$TMPDIR/go-cache" go build -o dist/darwin/pdf-tool .

# Windows 64-bit (需 mingw-w64)
GOTELEMETRY=off GOCACHE="$TMPDIR/go-cache" CGO_ENABLED=1 \
  GOOS=windows GOARCH=amd64 \
  CC=/opt/homebrew/bin/x86_64-w64-mingw32-gcc \
  go build -o dist/win/pdf-tool.exe .
```

### 一键构建脚本

```bash
./scripts/build-release.sh
```

输出：
- `dist/darwin/pdf-tool`
- `dist/win/pdf-tool.exe`