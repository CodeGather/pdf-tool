# PDF Tool

Version: v2.1+ (子包重构，多命令模式)

基于 Go + MuPDF (mutool) + pdfcpu + Ghostscript (gs) 的多功能 PDF 工具。
支持图片提取、PDF 合并（可压缩）、PDF 压缩、灯位图片替换。

## 快速开始

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
./pdf-tool -i input.pdf -o output              # 图片提取
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf  # 合并 PDF
./pdf-tool -compress -i input.pdf -o out.pdf       # 压缩 PDF
./pdf-tool -replace -replace-json config.json -replace-output result.pdf  # 替换
```

## 功能特性

| 命令 | 功能 | 说明 |
|------|------|------|
| **提取**（默认） | PDF → 图片 | 智能路由：直取或渲染裁剪，CMYK 正确还原 |
| **合并** `-merge` | 多 PDF → 单 PDF | 分块合并防 OOM，可选合并前用 gs 压缩 |
| **压缩** `-compress` | PDF 压缩 | Ghostscript pdfwrite，多种预设 |
| **替换** `-replace` | 灯位图片替换 | 根据 JSON 配置替换模板 PDF 中的图片并追加表格 |

### 提取功能详情

- **智能路由**：自动分析 PDF 结构，在「直取」与「渲染裁剪」之间选择最优路径
- **双引擎渲染**：主引擎 mutool draw (MuPDF)，回退 go-fitz (MuPDF CGo)
- **颜色准确**：mutool 的 CMYK→RGB 与 PDF 阅读器/Acrobat 100% 一致
- **CPU 并行可配**：渲染分块和编码池均受 `-cpu` 控制（0-100%，默认 25%）
- **CMYK JPEG 正确还原**：通过 JPEG SOF marker 检测分量数，经工具精准转换
- **SMask 透明合成**：FlateDecode+SMask 场景 21x 加速
- **8-bit 快速路径**：绕过 pdfcpu 锁竞争，直接解码 FlateDecode 图片

## 所有命令行参数

### 核心参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-i` | `input.pdf` | 输入 PDF 文件路径 |
| `-o` | `output` | 输出目录（提取模式）或输出文件（合并/压缩模式） |
| `-f` | `png` | 输出图片格式：`png`、`jpg` |
| `-dpi` | `300` | 渲染 DPI（仅影响渲染路径） |

### CPU 并行度

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-cpu` | `25` | CPU 使用率百分比 0-100。公式：`max(1, round(CPU核心数 × 百分比 / 100))`。例：`-cpu 25` 在 14 核上 = 4 线程 |

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
| `-merge-compress` | `true` | 合并前用 Ghostscript 压缩每个文件。配合 `-p` 查看进度 |
| `-p` | `false` | 打印合并与压缩进度 0-100 |

### 压缩模式

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-compress` | `false` | 启用 PDF 压缩模式（与 `-i` `-o` 配合） |
| `-compress-dir` | `""` | 压缩目录下所有 PDF |
| `-compress-preset` | `prepress` | 预设：`screen`(72dpi) / `ebook`(150dpi) / `printer`(300dpi) / `prepress` / `high`(不降采样) |
| `-compress-resolution` | `1200` | 覆盖预设的降采样 DPI |
| `-compress-jpegq` | `95` | JPEG 质量 1-100 |

### 替换模式

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-replace` | `false` | 启用 PDF 替换模式 |
| `-replace-json` | `""` | JSON 配置文件路径 |
| `-replace-output` | `""` | 输出 PDF 路径 |
| `-replace-font` | `""` | 中文字体文件路径（TTF） |
| `-replace-base-dir` | `""` | 模板 PDF 基础目录 |

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
# 串行模式
./pdf-tool -i sample.pdf -o out -cpu 0

# 使用 50% CPU 资源
./pdf-tool -i sample.pdf -o out -cpu 50

# 用满所有核心
./pdf-tool -i sample.pdf -o out -cpu 100
```

### 调试与诊断

```bash
# 调试日志 + 路由决策
./pdf-tool -i sample.pdf -o out -l

# 调试 + 耗时分析
./pdf-tool -i sample.pdf -o out -l -t
```

### 合并 PDF

```bash
# 合并整个目录
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf

# 指定文件列表
./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf

# 大量文件分批合并
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -merge-chunk-size 25
```

### 压缩 PDF

```bash
# 压缩单个文件
./pdf-tool -compress -i input.pdf -o compressed.pdf

# 自定义预设和分辨率
./pdf-tool -compress -compress-preset prepress -compress-resolution 600 -i input.pdf -o compressed.pdf

# 不降采样（最大质量）
./pdf-tool -compress -compress-preset high -i input.pdf -o compressed.pdf
```

### 合并前压缩

```bash
# 合并目录中所有 PDF，并先压缩每个文件（默认开启）
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf

# 关闭合并前压缩
./pdf-tool -merge -merge-compress=false -merge-dir ./pdfs -o merged.pdf

# 指定压缩预设
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -compress-preset ebook -compress-resolution 150
```

### 替换模式

```bash
# 根据 JSON 配置替换模板 PDF 中的灯位图片
./pdf-tool -replace -replace-json config.json -replace-output result.pdf

# 指定字体和基础目录
./pdf-tool -replace -replace-json ./config/1.json -replace-output output.pdf \
  -replace-font ./assets/hyzdx.ttf -replace-base-dir /data
```

## 项目结构

```
pdf-tool/
├── main.go              # 入口：flag 解析 + 路由分发（~240 行）
├── cmd/                 # 业务逻辑子包
│   ├── extract.go       # 图片提取（路由、渲染、直取、flood fill）
│   ├── merge.go         # PDF 合并（分块、压缩、分隔页）
│   ├── compress.go      # PDF 压缩（Ghostscript pdfwrite）
│   ├── replace.go       # 灯位图片替换
│   └── fitz_*.go        # MuPDF go-fitz 封装层
├── util/                # 工具函数包
│   ├── util.go          # FindMutool / FindGS / ComputeWorkerCount / FormatSize
│   ├── image.go         # 图片处理（编码、CMYK 转换、SMask）
│   └── str.go           # 字符串排序、裁剪操作符检测
├── pdf/                 # PDF 模板操作（替换模式）
│   ├── template.go      # pdfcpu 模板替换
│   ├── draw.go          # 图片缩放与文字叠加
│   ├── table.go         # gopdf 表格生成
│   └── extend.go        # 页面扩展
├── model/               # 数据模型（替换模式）
├── config/              # JSON 配置加载
├── matcher/             # 图片匹配算法
├── assets/              # 字体等资源文件
├── dist/                # 编译产物（各平台通用二进制）
├── scripts/             # 构建脚本
└── docs/                # 文档
    ├── ARCHITECTURE.md
    └── AI_HANDOVER.md
```

## 架构概览

详见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) 完整架构文档。

### 提取路由机制

```text
cmd.RunExtract()
  └─ convertPDFToImages()
      ├─ classifyPDFDocument() ── 逐页分析 → 路由分类
      │
      ├─ routeRenderCropComplexTransparency
      │   └─ renderCropPDF(): mutool 渲染整页 → flood fill 裁剪
      │
      ├─ routeRenderWholePageImage
      │   └─ renderWholePagePDF(): mutool 并行渲染 → PPM→JPEG 并行编码
      │
      └─ routeDirectExtract*
          └─ extractDirectImages(): 对象流提取（含 SMask 透明合成）
```

### 并行策略

| 阶段 | 方式 | 控制参数 |
|------|------|----------|
| mutool 渲染 | 子进程拆分页范围 | `-cpu` → `ComputeWorkerCount()` |
| PPM→编码 | goroutine 池 | `-cpu` → `ComputeWorkerCount()` |
| go-fitz 回退 | 串行（CGo 不安全） | 无（仅回退） |

## 构建

### 本地构建

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
```

### 通用二进制（macOS Intel + ARM）

```bash
./scripts/build-release.sh darwin
# 产物在 dist/darwin/pdf-tool
```

### 依赖

- **mutool**（MuPDF）：需在 PATH 中，可放在 `dist/<platform>/` 目录。构建脚本自动附带
- **Ghostscript (gs)**：压缩/合并前压缩需要。查找顺序：PATH → 同目录 → `binaries/<platform>/` → 平台子目录
- **Go 模块**：自动下载（pdfcpu, go-fitz, gopdf, golang.org/x/image）

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
| [docs/FEATURES.md](docs/FEATURES.md) | 功能说明文档：四种模式的详细流程和参数参考 |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 完整架构文档：每个分支和流程的精确描述 |
| [docs/AI_HANDOVER.md](docs/AI_HANDOVER.md) | AI 模型接手文档：函数调用图、关键陷阱、修改指南 |