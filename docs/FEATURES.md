# PDF Tool — 功能说明文档

> 版本：v2.1+ | 多模式 PDF 处理工具
> 语言：Go | 基于 MuPDF (mutool) + pdfcpu + Ghostscript (gs)

---

## 一、四种工作模式

pdf-tool 支持 4 种互斥的工作模式，通过命令行参数切换：

```
模式一：图片提取       （默认，不加任何模式参数）
模式二：PDF 合并        -merge
模式三：PDF 压缩        -compress
模式四：灯位图片替换     -replace
```

各模式互斥，同时只能使用一种。

---

## 二、全部命令行参数

### 2.1 通用参数（所有模式可用）

| 参数 | 简写 | 默认值 | 说明 |
|------|------|--------|------|
| `-i` | — | `input.pdf` | 输入 PDF 文件路径 |
| `-o` | — | `output` | 输出目录（提取模式）或输出文件（合并/压缩/替换模式） |
| `-cpu` | — | `25` | CPU 使用率百分比 0-100 |
| `-l` | `-log` | `false` | 启用调试日志 |
| `-p` | — | `false` | 打印进度 0-100（仅合并/压缩模式有效） |

### 2.2 图片提取参数

| 参数 | 简写 | 默认值 | 说明 |
|------|------|--------|------|
| `-f` | — | `png` | 输出图片格式：`png` 或 `jpg` |
| `-dpi` | — | `300` | 渲染 DPI |
| `-q` | `-quality` | `85` | JPEG 编码质量 1-100 |
| `-t` | `-timing` | `false` | 打印每阶段耗时 |
| `-m` | `-meta` | `false` | 打印图片宽高信息 |
| `-m-json` | `-meta-json` | `false` | JSON 格式图片元数据 |
| `-cc` | — | `false` | 色彩校正（已废弃，保留兼容） |

### 2.3 PDF 合并参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-merge` | `false` | 启用合并模式 |
| `-merge-dir` | `""` | 待合并 PDF 所在目录 |
| `-merge-inputs` | `""` | 逗号分隔文件列表 |
| `-merge-glob` | `*.pdf` | 文件匹配通配符 |
| `-merge-chunk-size` | `50` | 每批合并数（防 OOM） |
| `-merge-divider` | `false` | 文件间插入空白分隔页 |
| `-merge-compress` | `true` | 合并前用 Ghostscript 压缩 |

### 2.4 PDF 压缩参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-compress` | `false` | 启用压缩模式 |
| `-compress-dir` | `""` | 压缩目录下所有 PDF |
| `-compress-preset` | `prepress` | 预设：screen / ebook / printer / prepress / high |
| `-compress-resolution` | `1200` | 覆盖降采样 DPI |
| `-compress-jpegq` | `95` | JPEG 质量 1-100 |

### 2.5 灯位替换参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-replace` | `false` | 启用替换模式 |
| `-replace-json` | `""` | JSON 配置文件路径 |
| `-replace-output` | `""` | 输出 PDF 路径 |
| `-replace-font` | `""` | 中文字体 TTF 路径 |
| `-replace-base-dir` | `""` | 模板 PDF 基础目录 |
| `-replace-dir` | `""` | 批量合成目录 |

---

## 三、详细功能说明

### 3.1 图片提取

**作用**：从 PDF 中提取所有内嵌图片，自动选择最优策略。

**路由机制**（5 种策略逐级判定）：

```
classifyPDFDocument()
  │
  ├─① Form XObject 存在 → render-crop（渲染后裁剪）
  ├─② 透明度+裁剪路径 → render-crop
  ├─③ 无透明度但有裁剪 → render-crop
  ├─④ 所有页尺寸≈图片 → render-whole-page（整页渲染）
  ├─⑤ 有透明度无裁剪 → direct-extract（轻度透明度直取）
  ├─⑥ 多页多图 → direct-extract（编号输出）
  └─⑦ 默认 → direct-extract（单图最简路径）
```

**渲染引擎回退链**：

```
mutool 并行渲染 → mutool 串行渲染 → go-fitz（CGo 回退）
```

**并行策略**（两阶段）：

| 阶段 | 方式 | 控制 |
|------|------|------|
| 渲染 | N 个子进程按页范围拆分 | `-cpu` |
| PPM→编码 | N 个 goroutine 池 | `-cpu` |

**快速路径**（8-bit FlateDecode 图片绕过 pdfcpu 通用解码，加速 21x）：
- 直接 `sd.Decode()` 解码
- SMask 透明合成在快速路径内完成（goroutine-safe）
- CMYK JPEG 正确还原（SOF marker 检测分量数）

### 3.2 PDF 合并

**作用**：将多个 PDF 合并为单一 PDF 文件。

**工作流程**：

```
1. collectMergeInputs()  →  收集文件（目录/列表/通配符）
2. [可选] Ghostscript 并发压缩每个文件
3. 单批合并（≤chunkSize）或分批合并（防 OOM）
4. [可选] 文件间插入空白分隔页
```

**合并前压缩**（默认开启）：
- 使用 Ghostscript pdfwrite
- 支持所有压缩预设
- 并发压缩多文件
- 自动统计压缩比

### 3.3 PDF 压缩

**作用**：使用 Ghostscript 压缩 PDF 文件体积。

**预设说明**：

| 预设 | 降采样 DPI | 典型场景 | 体积 |
|------|-----------|---------|------|
| screen | 72 | 预览、邮件附件 | 最小 |
| ebook | 150 | 电子书阅读 | 中等 |
| printer | 300 | 打印输出 | 较大 |
| prepress（默认） | 1200 | 印刷级 | 质量最高 |
| high | 不降采样 | 档案保存 | 原图质量 |

**输出命名**：单文件模式 → `原文件名_compressed.pdf`

### 3.4 灯位图片替换

**作用**：根据 JSON 配置文件，替换模板 PDF 中的灯位图片，追加 isNew 表格。

**工作流程**：

```
Run()
  ├─1. LoadConfig() → 解析 JSON 配置（店铺、灯位、素材、品牌配置）
  ├─2. OpenTemplate() → 打开模板 PDF
  ├─3. 遍历灯位：
  │   ├─ a. 解析灯位编号
  │   ├─ b. 从 excel-data 取灯位属性
  │   ├─ c. 收集 isNew 表格行（独立于素材是否存在）
  │   └─ d. 匹配图片位置 + 素材图片
  ├─4. 并行处理图片（解码→缩放→文字叠加→JPEG 编码）
  ├─5. 串行替换 PDF 图片（Flate 压缩写入）
  ├─6. 叠加矢量边框
  ├─7. 构建 isNew 表格（gopdf）
  └─8. WriteToFile()
```

**核心要素**：

| 要素 | 说明 |
|------|------|
| 图片匹配 | 通过灯位坐标 (x,y,w,h) 匹配 PDF 中图片对象 |
| 素材选择 | 灯位备注 → 素材 PDF → 横/竖方向匹配 → 最接近宽高比 |
| 图片缩放 | contain 适配（保持比例，留白填充） |
| 上市文字 | 图片上方居中叠加，自适应字号 |
| 矢量边框 | 独立 PDF 矢量描边，不受分辨率影响 |
| isNew 表格 | 底部追加，自动扩展页面高度 |

**批量合成**（`-replace-dir`）：
```
./pdf-tool -replace -replace-dir ./configs/ -replace-output ./results/
```
扫描目录下所有 `*.json`，逐个执行替换，输出同名 `.pdf` 文件到结果目录。

---

## 四、项目结构

```
pdf-tool/
├── main.go               # 入口（~195 行）：flag 解析 + 路由分发
├── cmd/
│   ├── extract.go        # 图片提取（路由、渲染、直取、flood fill，~2000 行）
│   ├── merge.go          # PDF 合并（分块、压缩、分隔页）
│   ├── compress.go       # PDF 压缩（Ghostscript 封装）
│   ├── replace.go        # 灯位替换（单文件 + 批量合成）
│   └── fitz_*.go         # MuPDF go-fitz 封装
├── util/
│   ├── util.go           # FindMutool / FindGS / ComputeWorkerCount / FormatSize
│   ├── image.go          # 图片编码、CMYK 转换、SMask、原子写盘
│   └── str.go            # 自然排序、裁剪操作符检测
├── pdf/
│   ├── template.go       # 模板操作（OpenTemplate / ReplaceStreamDict / FindImageByRect）
│   ├── draw.go           # 图片缩放 + 文字叠加
│   ├── table.go          # gopdf 表格生成 + 注入
│   └── extend.go         # 页面高度扩展
├── model/types.go        # 数据模型
├── config/json.go        # JSON 配置加载
├── matcher/matcher.go    # 图片匹配算法
├── assets/               # 字体资源
├── dist/ + scripts/      # 构建产物 + 构建脚本
└── docs/
    ├── ARCHITECTURE.md   # 架构文档（路由、渲染、并行策略）
    └── AI_HANDOVER.md    # AI 接手文档（调用图、陷阱、修改指南）
```

---

## 五、构建与依赖

### 本地构建

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
```

### 通用二进制（macOS ARM + Intel）

```bash
./scripts/build-release.sh darwin
# → dist/darwin/{pdf-tool, mutool, gs}
```

### 依赖查找路径

| 工具 | 用途 | 查找顺序 |
|------|------|---------|
| **mutool** (MuPDF) | 图片提取渲染引擎 | PATH → 同目录 → `binaries/<platform>/mutool` |
| **gs** (Ghostscript) | PDF 压缩 | PATH → 同目录 → `../Resources/gs` → `binaries/<platform>/gs` |
| go-fitz | 渲染回退引擎 | Go CGo 编译时自带 |
| pdfcpu | PDF 解析/合并 | Go 模块自动下载 |
| gopdf | 表格生成 | Go 模块自动下载 |

---

## 六、快速参考

### 常见用法速查表

| 需求 | 命令 |
|------|------|
| 提取图片（PNG） | `./pdf-tool -i input.pdf -o output` |
| 提取图片（JPEG） | `./pdf-tool -i input.pdf -o output -f jpg` |
| 提取+调试 | `./pdf-tool -i input.pdf -o output -l -t` |
| 合并目录 | `./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf` |
| 合并列表 | `./pdf-tool -merge -merge-inputs a.pdf,b.pdf -o merged.pdf` |
| 不压缩的合并 | `./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -merge-compress=false` |
| 压缩单个 PDF | `./pdf-tool -compress -i input.pdf -o out.pdf` |
| 压缩目录 | `./pdf-tool -compress -compress-dir ./pdfs -o /out` |
| 替换灯位 | `./pdf-tool -replace -replace-json config.json -replace-output out.pdf` |
| 批量替换 | `./pdf-tool -replace -replace-dir ./configs -replace-output ./out` |
| 加速（全核） | `./pdf-tool -i input.pdf -o output -cpu 100` |
| 串行（低内存） | `./pdf-tool -i input.pdf -o output -cpu 0` |

### 参数分组速记

```
图片提取相关：  -i -o -f -dpi -q -cpu -l -t -m -m-json -cc
合并相关：      -merge -merge-dir -merge-inputs -merge-glob
                -merge-chunk-size -merge-divider -merge-compress
压缩相关：      -compress -compress-preset -compress-resolution -compress-jpegq
替换相关：      -replace -replace-json -replace-output -replace-font
                -replace-base-dir -replace-dir
通用：           -i -o -cpu -l -p
```