# PDF Tool

Version: v2.0+ (基于 mutool 渲染引擎，已移除 pdftoppm)

基于 Go + MuPDF (mutool) + pdfcpu 的 PDF 图片提取与合并工具。支持多种提取策略自动路由、CMYK JPEG 正确色彩还原、以及 CPU 使用率可配的并行渲染加速。

## 快速开始

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
./pdf-tool -i input.pdf -o output
./pdf-tool -i input.pdf -o output -f jpg -cpu 50
```

## 功能特性

- **智能路由**：自动分析 PDF 结构，在「直取」与「渲染裁剪」之间选择最优路径
- **双引擎渲染**：主引擎 mutool draw (MuPDF)，回退引擎 go-fitz (MuPDF CGo)
- **颜色准确**：mutool 的 CMYK→RGB 与 PDF 阅读器/Acrobat 100% 一致，无偏红问题
- **CPU 并行可配**：渲染分块和编码池均受 `-cpu` 控制（0-100%，默认 25% 核心）
- **CMYK JPEG 正确还原**：通过 JPEG SOF marker 检测分量数，经 sips 精准转换
- **SMask 透明合成**：FlateDecode+SMask 场景 21x 加速（goroutine-safe 快速路径）
- **8-bit 快速路径**：绕过 pdfcpu 锁竞争，直接解码 FlateDecode 图片
- **多 PDF 合并**：支持目录/列表/通配符，分批合并避免 OOM

## 所有命令行参数

### 核心参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-i` | `input.pdf` | 输入 PDF 文件路径 |
| `-o` | `output` | 输出目录 |
| `-f` | `png` | 输出图片格式：`png`、`jpg` |
| `-dpi` | `300` | 渲染 DPI（仅影响渲染路径） |

### CPU 并行度

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-cpu` | `25` | CPU 使用率百分比 0-100。0=串行，100=用满所有核心。公式：`max(1, round(CPU核心数 × 百分比 / 100))`。例：`-cpu 25` 在 14 核上 = 4 线程 |

### 编码与质量

| 参数 | 简写 | 默认值 | 说明 |
|------|------|--------|------|
| `-quality` | `-q` | `85` | JPEG 编码质量 1-100 |

### 调试与诊断

| 参数 | 简写 | 说明 |
|------|------|------|
| `-log` | `-l` | 启用调试日志输出（含路由决策） |
| `-meta` | `-m` | 打印每张图片的宽高信息 |
| `-meta-json` | `-m-json` | 以 JSON 格式输出图片元数据 |
| `-timing` | `-t` | 打印每个阶段的耗时信息 |

### 合并模式

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-merge` | `false` | 启用 PDF 合并模式（此时 `-o` 为输出文件，非目录） |
| `-merge-dir` | `""` | 待合并 PDF 所在目录 |
| `-merge-inputs` | `""` | 逗号分隔的待合并 PDF 文件列表 |
| `-merge-glob` | `*.pdf` | 合并模式下的文件匹配模式 |
| `-merge-chunk-size` | `50` | 每批合并的 PDF 数量 |
| `-merge-divider` | `false` | 在合并文件之间插入空白分隔页 |
| `-p` | `false` | 打印合并进度 0-100 |

### 已废弃（保留兼容）

| 参数 | 说明 |
|------|------|
| `-cc` | 色彩校正矩阵。pdftoppm 时代遗留，当前引擎 mutool 颜色正确无需校正 |

## 用法示例

### 基础用法

```bash
# 默认 PNG 输出
./pdf-tool -i sample.pdf -o out

# JPG 输出
./pdf-tool -i sample.pdf -o out -f jpg
```

### CPU 并行度控制

```bash
# 串行模式（适用于小文件或内存受限场景）
./pdf-tool -i sample.pdf -o out -cpu 0

# 使用 50% CPU 资源
./pdf-tool -i sample.pdf -o out -cpu 50

# 用满所有核心（最大吞吐）
./pdf-tool -i sample.pdf -o out -cpu 100

# 默认 25%（14 核机器 ≈ 4 线程）
./pdf-tool -i sample.pdf -o out
```

### 调试与诊断

```bash
# 调试日志 + 路由决策
./pdf-tool -i sample.pdf -o out -l

# 调试 + 耗时分析
./pdf-tool -i sample.pdf -o out -l -t

# 查看图片元数据
./pdf-tool -i sample.pdf -o out -l -m

# JSON 格式元数据
./pdf-tool -i sample.pdf -o out -l -m-json
```

### 合并 PDF

```bash
# 合并整个目录
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf

# 指定文件列表
./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf

# 大量文件分批合并
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -merge-chunk-size 25

# 合并 + 进度
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -p
```

## 架构概览

详见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) 完整架构文档。

### 路由机制

```text
convertPDFToImages()
  ├─ classifyPDFDocument() ── 逐页分析 → 路由分类
  │
  ├─ routeRenderCropComplexTransparency
  │   └─ renderCropPDF(): mutool 渲染整页 → flood fill 裁剪
  │
  ├─ routeRenderWholePageImage
  │   └─ renderWholePagePDF(): mutool 并行渲染 → PPM→JPEG 并行编码
  │
  ├─ routeDirectExtractTransparency
  │   └─ extractDirectImages(): 对象流提取（含 SMask 透明合成）
  │
  ├─ routeDirectExtractMultiImageStack
  │   └─ extractDirectImages(): 多图逐个提取并编号
  │
  └─ routeDirectExtractSingleObject
      └─ extractDirectImages(): 单图最简路径
```

### 并行策略

| 阶段 | 方式 | 控制参数 |
|------|------|----------|
| mutool 渲染 | 子进程拆分页范围 | `-cpu` → `computeWorkerCount()` |
| PPM→编码 | goroutine 池 | `-cpu` → `computeWorkerCount()` |
| go-fitz 回退 | 串行（CGo 不安全） | 无（仅回退） |

## 构建

### 本地构建

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
```

**依赖**：

- **mutool**（MuPDF）：需在 PATH 中，或安装在 `/opt/homebrew/bin/mutool`，或放在同级 `bund/` 目录
- **Go 模块**：自动下载（pdfcpu, go-fitz, tiff）

### 交叉编译

```bash
# macOS Intel/ARM
GOTELEMETRY=off go build -o dist/darwin/pdf-tool .

# Linux
GOOS=linux GOARCH=amd64 go build -o dist/linux/pdf-tool .

# Windows (需 mingw-w64)
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 \
  CC=x86_64-w64-mingw32-gcc \
  go build -o dist/win/pdf-tool.exe .
```

## 项目文档

| 文件 | 说明 |
|------|------|
| [README.md](README.md) | 本文件：快速入门、参数说明、示例 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 完整架构文档：每个分支和流程的精确描述 |
| [docs/AI_HANDOVER.md](docs/AI_HANDOVER.md) | AI 模型接手文档：函数调用图、关键陷阱、修改指南 |
| [main.go](main.go) | 核心代码（～2900 行，单文件，有完整中文注解） |