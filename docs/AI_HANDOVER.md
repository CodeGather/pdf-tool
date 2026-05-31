# AI 模型接手文档 — PDF Tool

> 目标：让下一个大模型（LLM/AI Agent）能在 5 分钟内完全理解本项目的代码结构、关键决策、所有分支逻辑和常见陷阱，无需重读 2900 行代码。

---

## 0. 项目速览

| 属性 | 值 |
|------|-----|
| 语言 | Go 1.24 |
| 文件 | 单文件 `main.go` (~2900 行) + 1 个 CGo stub |
| 核心依赖 | pdfcpu (PDF 解析)、go-fitz (MuPDF CGo)、mutool (子进程渲染) |
| 编译 | `go build -o pdf-tool .` |
| 运行 | `./pdf-tool -i input.pdf -o output` |
| 风格 | 所有代码在 `package main`，无子包，无接口，面向过程 |

---

## 1. 文件结构

```
pdf-tool/
├── main.go                  # 核心文件 (~2900 行，所有功能都在这里)
├── fitz_warning_stub.go     # 98 字节 CGo 废弃警告抑制
├── go.mod / go.sum          # Go 依赖
├── README.md                # 用户文档
├── docs/
│   ├── ARCHITECTURE.md      # 完整架构文档
│   └── AI_HANDOVER.md       # 本文件
└── scripts/                 # 构建脚本（可选）
```

**没有子包，没有接口，没有测试文件。** 如果用户让你添加测试或重构，从创建 `internal/` 子包开始。

注意：`fitz_warning_stub.go` 引用了已删除的 `ProcessWarning` 函数符号，编译时 `go vet` 会报 `undefined: muteFitzWarnings`，但 Go 编译器只报 vet 警告不报编译错误 — 不影响 `go build`。如果用户要求消除 vet 警告，需要删除该文件或将引用修改为 `go-fitz` 的官方禁止警告 API。

---

## 2. 函数调用图（完整）

```
main()
  ├── mergePDFs()
  │     ├── collectMergeInputs()        # 收集文件（目录/列表/通配符）
  │     └── api.MergeCreateFile()       # pdfcpu 合并（分批）
  │
  └── convertPDFToImages()
        ├── classifyPDFDocument()       # ★ 路由决策核心
        │     ├── hasPageContentClip()  #   裁剪路径检测
        │     │     ├── extractCM()     #     cm 矩阵的 a,d
        │     │     └── extractRect()   #     re 矩形的 w,h
        │     ├── getPageContentString()
        │     ├── shouldRenderWholePageImage()
        │     └── dictAt()
        │
        ├── [routeRenderCropComplexTransparency]
        │   └── renderCropPDF()
        │         ├── findMutool()
        │         ├── [mutool] renderSinglePageCropPdftoppm()
        │         │     ├── readPPM()
        │         │     └── cropImage()
        │         └── [go-fitz] renderSinglePageCrop()
        │               └── findLargestRegions()
        │                     └── floodFillRegion()
        │
        ├── [routeRenderWholePageImage]
        │   ├── renderWholePagePDF()
        │   │     ├── computeWorkerCount()
        │   │     ├── [Phase 1] N × mutool draw -ppm (子进程)
        │   │     ├── [Phase 2] N goroutines readPPM → encode
        │   │     └── [回退] renderWholePagePDFGoFitz()
        │   └── renderWholePagePDFGoFitz()
        │
        └── [routeDirectExtract*]
            └── extractDirectImages()
                  └── writeDirectImage()
                        ├── writeDirectImageFast()     # ★ 快速路径
                        │     ├── sd.Decode()          #   FlateDecode 修复
                        │     ├── [直通] 直接写流
                        │     ├── [CMYK JPEG] convertCMYKJPEGToOutput()
                        │     ├── [SMask] extractSoftMask()
                        │     └── [8-bit] encodeJPEG/PNG
                        └── [回退] pdfcpu.ExtractImage()
```

---

## 3. 关键决策（必须理解，否则改错）

### 3.1 为什么用 mutool 而不是 pdftoppm？

**历史**：原来用 pdftoppm（poppler-utils），但它的 CMYK→RGB 转换（通过 lcms2）偏红 —— G 通道偏低 ~10，B 通道偏低 ~6。mutool（MuPDF）的颜色与 macOS PDFKit/Acrobat 100% 一致。

**影响**：
- `pdftoppm` 相关的所有代码、捆绑二进制、库文件已全部删除
- 色彩校正矩阵（`-cc`）保留但默认关闭，仅用于回归测试
- 函数名 `renderSinglePageCropPdftoppm` 未改（历史遗留），内容已改为 mutool

### 3.2 Clip 检测的两个独立条件

**为什么需要两个条件？**

1. **`anyGroupWithClip`** — 页面有 `/Group /Transparency` 标记 + 实际裁剪路径
2. **`anyRealClip`** — 页面无 Group 但有裁剪路径（Illustrator 30.2 导出）

**为什么没有 /Group 也需要渲染？**

`18.pdf` 案例：Illustrator 30.2 导出的 PDF 没有 /Group 但有裁剪路径，直取得到 5921×1749（全页）而非裁剪后的 2954×3839（正确内容）。

### 3.3 sd.Decode() 的 21x 加速

**问题**：对于 FlateDecode 编码的图片，`pdfcpu` 在非 DCT 过滤器时不会自动调用 `sd.Decode()`，导致 `sd.Content` 为空，快速路径无法处理。

**修复**：在快速路径入口添加：

```go
if len(sd.Content) == 0 && len(sd.Raw) > 0 {
    sd.Decode()  // 手动解码
}
```

这使 2.pdf（FlateDecode+SMask）从 6s 降到 0.28s。

### 3.4 CGo 信号栈限制

**Go 的 CGo 在 macOS 上有已知限制**：goroutine 的栈不是真正的 pthread 栈，而 Darwin 的信号处理要求信号栈是 pthread 栈。当 CGo 调用 MuPDF 时触发信号（如 SIGABRT/SIGSEGV），goroutine 的信号栈不足，导致 `semasleep on Darwin signal stack` 崩溃。

**应对**：
- mutool 渲染使用 `os/exec` 子进程（无 CGo，完全安全）
- go-fitz 仅作为回退路径（串行调用，减少触发概率）

### 3.5 并行策略的两个阶段

```go
n := computeWorkerCount()
// Phase 1: 渲染并行 — n 个子进程
// Phase 2: 编码并行 — n 个 goroutine
```

**渲染为什么用子进程**：mutool draw 是独立的可执行文件，通过 `os/exec` 启动。按页范围分块。

**编码为什么用 goroutine**：PPM→JPEG 编码是纯 Go 标准库操作，无 CGo，goroutine 安全。

---

## 4. 路由决策边界值（这些数字别改，除非你验证了）

| 边界值 | 位置 | 含义 |
|--------|------|------|
| `1.05` | hasPageContentClip | cm_a / clip_w 超过此值说明有实际裁剪 |
| `5%` | nearlyEqual | 图片尺寸与页面尺寸的容差（用于整页渲染判断） |
| `50000` | findLargestRegions | flood fill 最小面积过滤噪声 |
| `200` | isBackground | RGB 通道阈值（RGB>200 视为背景） |
| `8` | maxClipOperatorCountForCrop | 同一内容流中裁剪操作符超过此数视为"重复裁剪框"而非实际裁剪 |

---

## 5. 修改指南（给 AI 的自我检查清单）

如果你要修改代码，每次改前问自己：

1. **修改分类逻辑** → 会影响所有 PDF 的路由选择。改完后测试 `16.pdf`（应该 render-crop）、`17.pdf`（应该 direct-extract）、`18.pdf`（应该 render-crop）

2. **修改并行度** → computeWorkerCount 的返回值影响渲染分块和编码池大小。确保 `-cpu 0` 返回 1（串行），不超过 NumCPU

3. **修改快速路径** → 一定测试 2.pdf（FlateDecode+SMask）。确保 sd.Decode() 在正确的位置被调用

4. **修改颜色处理** → 测试 12.pdf 和 15.pdf 的 RGB 值是否与 mutool 参考一致。12.pdf 的参考值 RGB(163,109,99)

5. **修改渲染引擎** → 如果换回 pdftoppm，必须重新启用色彩校正矩阵。如果添加新引擎，需要扩展 findMutool()

6. **添加新参数** → 注意 `-p` 已被合并进度占用，不可复用。`-w` 是已废弃子进程参数

---

## 6. 测试 PDF 文件索引

放在 `/Users/Yau/work/1.Resources/2.AI/test/` 目录下：

| 文件 | 特征 | 预期路由 | 特殊说明 |
|------|------|---------|---------|
| 1-5.pdf | 简单嵌入图片 | direct-extract | 基准测试 |
| 12.pdf | CMYK 印刷文件 | render-whole-page | 71 页，颜色参考 RGB(163,109,99) |
| 15.pdf | CMYK 文件 | render-whole-page | 颜色参考（与 12.pdf 同一类） |
| 16.pdf | 有 /Group + 裁剪 | render-crop | clip ratio=7.68 |
| 17.pdf | 有 /Group 无裁剪 | direct-extract | clip ratio=1.00，透明度直取 |
| 18.pdf | 无 Group 但有裁剪 | render-crop | Illustrator 30.2，clip ratio=7.92 |
| 19.pdf | 大文件慢速 | render-whole-page | 曾 54s，现 mutool 压缩 |
| 20.pdf | 大图 | render-whole-page | 测试自动格式判断 |

大规模测试目录：
- `/Users/Yau/work/1.Resources/2.AI/test/Visual画面素材/` — 72 个广告设计 PDF
- `/Users/Yau/work/1.Resources/2.AI/test/YSL比例档/` — 110 个比例档 PDF（486 张图片，11.66s）

**已知问题**：`SKIN CARE-左右双拼-2601.pdf` 在 YSL 比例档中输出 0 张（pdfcpu 解析 bug？），尚未排查。

---

## 7. 性能基线

测试文件：12.pdf（71 页，300 DPI，JPEG 85%）

| 模式 | 耗时 | 说明 |
|------|------|------|
| `-cpu 0` | 33.6s | 串行 |
| `-cpu 25`（默认） | ~7.6s | 4 线程（14 核上） |
| `-cpu 50` | ~5.5s | 7 线程 |
| `-cpu 100` | ~6.6s | 14 线程（更多线程开销） |
| 并行渲染 Phase 1 | 5.9s | mutool 4 进程 |
| 并行编码 Phase 2 | 5.4s | 4 goroutines |

**全量测试**（Visual 素材，72 PDF，199 张）：~11.66s

---

## 8. 常见陷阱

### 8.1 路由误判

如果用户在分析后说"输出不正确"，先检查路由分类：运行 `./pdf-tool -i input.pdf -o /tmp/out -l`，查看日志中的路由决策。

- 该走 render-crop 走了 direct-extract → 图片尺寸和内容不对
- 该走 direct-extract 走了 render-crop → 性能下降但内容可能正确

### 8.2 mutool 不可用

如果用户说"没有输出"或"性能异常慢"，检查 mutool：
```bash
which mutool  # 或 /opt/homebrew/bin/mutool
```

### 8.3 TMPDIR

mutool 渲染使用 `os.MkdirTemp`，如果 `TMPDIR` 环境变量无效或目录不存在，渲染失败会被静默吞掉（`renderWholePagePDF` 的并行阶段出错时回退到串行）。

### 8.4 `-p` 同时用于压缩与合并进度

`-p` 参数同时输出压缩阶段（`-merge-compress=true`）和合并阶段的进度数字。压缩阶段每完成一个文件输出一次 `completed*100/total`，合并阶段每完成一个 chunk 输出一次。两者都是 0→100 的数字序列，Tauri 前端通过 stderr 接收。

---

## 9. 添加新功能的模式

由于所有代码在单个 `main.go` 中，添加功能时：

1. **添加函数** → 在文件末尾添加（自定义排序：public 函数在前，private 辅助在后）
2. **添加全局变量** → 在文件顶部 `var` 区块添加（必须加中文注释）
3. **添加参数** → 在 `main()` 的 flag 定义区块添加，同时在 `flag.Usage` 中添加示例
4. **添加进度输出** → 如果新增慢速阶段，使用 `traceProgress(enabled, 0→100)` 输出到 stderr，配合已有 `-p` 参数。注意启动循环和结果收集需分 goroutine 并行，避免 sem/信号量阻塞延迟进度；`wg.Add(total)` 必须在主 goroutine 提前设置，防止 WaitGroup 竞态
5. **添加路由** → 扩展 `pdfDocumentRoute` 枚举 + `classifyPDFDocument` 的判定 + `convertPDFToImages` 的分支 + 实现对应函数

---

## 10. 留给下一个 AI 的任务清单

- [ ] **2601.pdf 输出 0 张** — 分析原因并修复（可能是 pdfcpu 解析 bug）
- [ ] **17.pdf 透明度保留** — render-crop 使用 mutool RGBA 渲染以保留透明度
- [ ] **函数名清理** — `renderSinglePageCropPdftoppm` 应重命名为 `renderSinglePageCropMutool`
- [ ] **并行负载均衡** — 当前 chunk 按页数平分，大页多的 chunk 可能拖慢整体
- [ ] **Windows/Linux 测试** — mutool 跨平台需测试
- [ ] **单元测试** — 添加 `_test.go` 文件测试路由、颜色、快速路径