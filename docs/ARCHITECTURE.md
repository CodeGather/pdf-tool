# PDF Tool 完整架构文档

Version: v2.0+ | 语言: Go | 单文件: main.go (~2900 行)

---

## 1. 系统概述

PDF Tool 是一个 PDF 图片提取与合并工具。核心功能是从 PDF 中提取内嵌图片，支持两种策略的自动路由：

- **直取路径** (Direct Extract)：直接从 PDF 对象流中复制或解码图片，最快
- **渲染裁剪** (Render & Crop)：先用 mutool 渲染整页，再通过连通域分析裁剪出图片主体

渲染引擎优先使用 **mutool draw -ppm**（MuPDF 子进程），不可用时回退到 **go-fitz**（MuPDF CGo）。

---

## 2. 入口与初始化

### 2.1 main() [line 166]

```text
main()
  ├─ 解析所有 flag 参数
  ├─ 初始化全局状态（日志、元数据收集器、并行度）
  │
  ├─ [merge 模式] mergePDFs()
  │   └─ 收集输入 → 分批合并 → 输出
  │
  └─ [默认模式] convertPDFToImages()
      └─ 路由分类 → 选择提取策略
```

**全局变量**：

| 变量 | 用途 |
|------|------|
| `debugLogsEnabled` | `-log` 标志，控制 log.SetOutput |
| `imageMetaEnabled` / `imageMetaJSONEnabled` | 元数据输出开关 |
| `globalImageMetaCollector` | 汇总图片元数据，结束时统一输出 |
| `colorCorrectionEnabled` | `-cc` 弃用选项，默认 false |
| `mutoolPath` | mutool 路径缓存，findMutool() 设置 |
| `parallelPercent` | `-cpu` 值，computeWorkerCount() 使用 |

---

## 3. 路由系统 (classifyPDFDocument)

### 3.1 路由枚举 [line 505]

```go
const (
    routeRenderCropComplexTransparency  // 复杂透明度 → 渲染后裁剪
    routeDirectExtractTransparency      // 轻度透明度 → 直取
    routeDirectExtractMultiImageStack   // 多图堆叠 → 直取
    routeDirectExtractSingleObject      // 单图 → 直取
    routeRenderWholePageImage           // 整页图 → 整页渲染
)
```

### 3.2 决策树 [classifyPDFDocument, line 698]

```text
classifyPDFDocument()
  │
  ├─ 逐页扫描 (for each page)
  │   │
  │   ├── 1. 检测裁剪路径 (hasPageContentClip)
  │   │   └─ content stream 中解析 W/W* 操作符
  │   │
  │   ├── 2. 检查 /Group /Transparency
  │   │   └─ 有 Group → anyGroup = true
  │   │       └─ 同时有裁剪路径 → anyGroupWithClip = true
  │   │
  │   ├── 3. 检查无 Group 但有裁剪路径
  │   │   └─ hasClip 且 !anyGroup → anyRealClip = true
  │   │
  │   ├── 4. 检查 Form XObject
  │   │   └─ Resources/XObject 中有 Fm* 前缀 → anyFormXObject = true
  │   │
  │   └── 5. 检查图片对象
  │       └─ ImageObjNrs → 计数/尺寸评估
  │           └─ 单图且尺寸≈页面 → allPagesWholeImage 保持 true
  │
  └── 判定（优先级从高到低）:
      │
      ├── ① anyFormXObject → routeRenderCropComplexTransparency
      │   (Form XObject 意味着嵌套绘制，直取不完整)
      │
      ├── ② anyGroupWithClip → routeRenderCropComplexTransparency
      │   (透明度 + 裁剪路径，必须渲染才能看到正确效果)
      │
      ├── ③ anyRealClip → routeRenderCropComplexTransparency
      │   (无 Group 但有裁剪，如 Illustrator 30.2 导出的 PDF)
      │
      ├── ④ allPagesWholeImage → routeRenderWholePageImage
      │   (所有页的图片尺寸 ≈ 页面尺寸，直接输出整页)
      │
      ├── ⑤ anyGroup → routeDirectExtractTransparency
      │   (有透明度标记但无裁剪，直取保留透明度)
      │
      ├── ⑥ 多页多图 → routeDirectExtractMultiImageStack
      │   (直取每张图片并编号)
      │
      └── ⑦ 默认 → routeDirectExtractSingleObject
          (最简路径，速度最快)
```

### 3.3 裁剪检测 (hasPageContentClip) [line 560]

```text
hasPageContentClip(ctx, pageDict)
  │
  ├── 获取 /Contents 流
  ├── FlateDecode 解码
  │
  ├── 解析 cm 矩阵 → extractCM() 获取 (a, d)
  │   (cm a 是 x 方向缩放，d 是 y 方向缩放)
  │
  ├── 解析 re 矩形 → extractRect() 获取 (w, h)
  │   (re 定义了裁剪框的尺寸)
  │
  └── 判断：cm_a / clip_w > 1.05 ⇒ 有实际裁剪路径
      (1.05 的阈值用于过滤掉 Illustrator 自动添加的 /Group 标记)
```

**关键阈值**：`cm_a / clip_w > 1.05`

- > 1.05：图片被放大了超过裁剪框，说明有实际裁剪，需要 render-crop
- ≤ 1.05：图片就在裁剪框内，无实际裁剪，直取即可

---

## 4. 渲染裁剪路径

### 4.1 renderCropPDF [line 799]

```text
renderCropPDF()
  │
  ├── 逐页渲染 (for each page):
  │   │
  │   ├── A. mutool 可用 → 渲染 + PPM + flood fill
  │   │   ├── mutool draw -r 300 -o p%d.ppm input.pdf <page>
  │   │   ├── readPPM() → *image.RGBA
  │   │   └── findLargestRegions() → 裁剪
  │   │       ├── flood fill 从四角扫描
  │   │       ├── 按面积降序排列
  │   │       └── 取 top-1 区域
  │   │
  │   └── B. mutool 不可用 → go-fitz 渲染 + flood fill
  │       ├── fitz.New() + doc.Image()
  │       └── findLargestRegions() → 裁剪
  │
  └── 编码输出 JPEG/PNG
```

**flood fill 裁剪算法** (findLargestRegions [line 2345])：

```text
findLargestRegions(img, maxRegions)
  │
  ├── 转 RGBA
  ├── 从四角扫描像素
  │
  ├── 对每个非背景像素 (isBackground=RGB>200):
  │   └── floodFillRegion() 四邻域扩散
  │       ├── 记录外接矩形 (minX, maxX, minY, maxY)
  │       └── 记录像素面积
  │
  ├── 过滤：面积 < 50000 视为噪声
  └── 按面积降序取前 maxRegions 个
```

### 4.2 renderWholePagePDF [line 907]

```text
renderWholePagePDF()
  │
  ├── Phase 1: 并行 mutool 渲染
  │   │
  │   ├── n = computeWorkerCount()
  │   ├── chunkSize = ceil(pageCount / n)
  │   │
  │   ├── 启动 n 个子进程 (sync.WaitGroup)
  │   │   ├── worker 0: mutool draw -r 300 -o <tmp>/p%d.ppm input.pdf 1-chunkSize
  │   │   ├── worker 1: mutool draw ... input.pdf chunkSize+1-2*chunkSize
  │   │   └── ...
  │   │
  │   └── 任一失败 → 回退串行 mutool → 回退 go-fitz
  │
  ├── Phase 2: 并行 PPM→编码
  │   │
  │   ├── sem = make(chan struct{}, n)  // goroutine 池容量
  │   ├── 遍历每页 → goroutine 获取 token
  │   │   ├── readPPM(path) → *image.RGBA
  │   │   ├── 裁剪（如需）
  │   │   ├── encodeJPEG / encodePNG
  │   │   └── writeImageAtomically (原子写盘)
  │
  └── 清理临时目录
```

**回退链**：`mutool 并行 → mutool 串行 → go-fitz`

### 4.3 renderWholePagePDFGoFitz [line 1097]

```text
renderWholePagePDFGoFitz()
  │
  ├── mutool 不可用时的回退路径
  ├── 串行：打开一次 doc，逐页 doc.Image()
  └── 每页编码 JPEG/PNG → 原子写盘
```

**注意**：go-fitz 使用 CGo，在 macOS 上不能 goroutine 并行（信号栈限制），只能串行。

---

## 5. 直取路径

### 5.1 extractDirectImages [line 1154]

```text
extractDirectImages()
  │
  ├── 逐页扫描 (for each page):
  │   ├── 获取 /Resources/XObject
  │   │
  │   ├── 遍历所有 /Image 对象
  │   │   │
  │   │   ├── ① 直接引用图片
  │   │   │   └── writeDirectImage()
  │   │   │       ├── writeDirectImageFast() 尝试
  │   │   │       │   ├── true → 成功，继续下一张
  │   │   │       │   └── false → 回退 pdfcpu.ExtractImage
  │   │   │
  │   │   └── ② Form XObject 中嵌套的图片
  │   │       └── 递归遍历 → writeDirectImage()
  │   │
  │   └── 每页编号从 0 开始，按顺序递增
  │
  └── 元数据收集（通过全局 collector 统一输出）
```

### 5.2 writeDirectImage [line 1480]

```text
writeDirectImage(ctx, ctxMu, inputFile, pageNr, objNr, ...)
  │
  ├── 1. 获取图片流字典 sd
  ├── 2. writeDirectImageFast() — 尝试快速路径
  │
  └── 3. 快速路径失败 → 通用解码（锁保护）
      ├── ctxMu.Lock()
      ├── pdfcpu.ExtractImage(ctx, pageNr, objNr)
      ├── 解码并编码输出
      └── ctxMu.Unlock()
```

### 5.3 writeDirectImageFast [line 1611] — 快速路径决策树

**这是整个程序最复杂的函数，需要完全理解每个分支。**

```text
writeDirectImageFast()
  │
  ├──── 条件检查 ──────────────────────────────────────
  │
  ├── A. sd.Content 为空且 sd.Raw 非空 → sd.Decode()
  │   (关键修复：FlateDecode 过滤器时 pdfcpu 不自动解码)
  │
  ├── B. 直通复制路径 ────────────────────────────────
  │   │
  │   └── 条件：JPEG/JPX 编码 且 过滤器非 FlateDecode
  │       │
  │       ├── B1. JPX → convertJPXToOutput()
  │       ├── B2. JPEG → 直接写 .jpg
  │       └── B3. CMYK JPEG + 用户指定 PNG
  │           └── convertCMYKJPEGToOutput() → sips 转换
  │
  ├── C. 8-bit 快速解码路径 ──────────────────────────
  │   │
  │   └── 条件：8-bit RGB/Gray 或带 SMask
  │       │
  │       ├── C1. sd.Decode() 解码图像数据
  │       │
  │       ├── C2. 检测 SMask (/SMask 键)
  │       │   ├── 有 SMask → extractSoftMask()
  │       │   │   ├── 解码 SMask 灰度流
  │       │   │   ├── 检查尺寸一致
  │       │   │   └── 合成 RGBA (alpha 通道)
  │       │   └── 无 SMask → 直接转为 RGBA
  │       │
  │       ├── C3. 自动格式判断
  │       │   ├── CMYK JPEG 自动输出 jpg
  │       │   └── 否则使用用户指定格式
  │       │
  │       └── C4. encodeJPEG / encodePNG → 原子写盘
  │
  └── D. 回退信号 ────────────────────────────────────
      │
      └── 以上均不满足 → 返回 false, nil
          (调用者走 pdfcpu.ExtractImage 通用解码)
```

**快速路径 vs 通用解码的性能对比**：

| 场景 | 快速路径 | 通用解码 | 加速 |
|------|---------|---------|------|
| FlateDecode+SMask (2.pdf) | 0.28s | 6.0s | 21x |
| 直接 JPEG 复制 | ~0.01s | ~0.5s | 50x |
| 8-bit RGB FlateDecode | ~0.02s | ~0.3s | 15x |

---

## 6. 单页渲染辅助函数

### 6.1 renderSinglePageCropPdftoppm [line 1325]

（函数名含 Pdftoppm 但实际已改为 mutool 渲染）

```text
renderSinglePageCropPdftoppm(inputFile, pageNr, dpi, cropX, cropY, cropW, cropH)
  │
  ├── 1. mutool draw -r <dpi> -o <tmp>/p%d.ppm input.pdf <pageNr>
  ├── 2. readPPM() → *image.RGBA
  ├── 3. 计算像素裁剪区域 (PDF 点 → 像素：* DPI/72)
  ├── 4. cropImage() 裁剪并编码
  └── 5. 原子写盘
```

### 6.2 renderSinglePageCrop [line 1406]

```text
renderSinglePageCrop(inputFile, pageNr, dpi)
  │
  ├── 1. go-fitz 渲染整页
  ├── 2. findLargestRegions() → flood fill 裁剪
  └── 3. 编码输出
```

**两者区别**：
- `renderSinglePageCropPdftoppm`：mutool 渲染 + cm 矩阵精确裁剪（更快，颜色正确）
- `renderSinglePageCrop`：go-fitz 渲染 + flood fill 裁剪（回退路径，稍慢）

---

## 7. 色彩处理

### 7.1 CMYK JPEG 检测 [isCMYKJPEG, line 2014]

```text
isCMYKJPEG(data)
  │
  └── 扫描 JPEG SOF0 marker (0xFF 0xC0)
      └── 第 7 个字节 = 分量数
          ├── 3 → RGB/YUV
          └── 4 → CMYK
```

### 7.2 CMYK→RGB 转换 [convertCMYKJPEGToOutput, line 2058]

```text
convertCMYKJPEGToOutput(data, outPath)
  │
  ├── 1. 写入临时文件
  ├── 2. sips -s format png 转换
  │   (macOS 内置 sips 工具，颜色准确)
  └── 3. sips 失败 → Go 标准库解码（部分变体）
```

### 7.3 SMask 透明合成 [extractSoftMask, line 2111]

```text
extractSoftMask(ctx, sd, objNr, w, h)
  │
  ├── 从图片字典获取 /SMask
  ├── 解码 SMask 流（灰度图像）
  ├── 检查尺寸一致 (w_smask == w_main && h_smask == h_main)
  ├── 检查位深 == 8
  └── 返回 alpha 通道数据 ([]byte)
```

在写图片时，SMask 数据作为 alpha 通道合成到 RGBA 像素中，输出 PNG 保留透明度。

---

## 8. 并行度计算 [computeWorkerCount, line 2472]

```text
computeWorkerCount()
  │
  ├── numCPU = runtime.NumCPU()
  │
  ├── parallelPercent ≤ 0 → return 1 (串行)
  ├── parallelPercent ≥ 100 → return numCPU (全核)
  │
  └── n = (numCPU * parallelPercent + 50) / 100  (四舍五入)
      └── max(1, n)
```

**示例**：

| `-cpu` 值 | 4 核机器 | 8 核机器 | 14 核机器 |
|-----------|---------|---------|----------|
| 0 | 1 | 1 | 1 |
| 25 | 1 | 2 | 4 |
| 50 | 2 | 4 | 7 |
| 100 | 4 | 8 | 14 |

---

## 9. 辅助工具函数

### 9.1 PPM 读取 [readPPM, line 2498]

```text
readPPM(path)
  │
  ├── 解析 PPM P6 头部：P6\n<w> <h>\n255\n
  ├── 跳过注释行 (# 开头)
  ├── 读取 RGB 像素数据（每像素 3 字节）
  └── 转为 *image.RGBA（alpha=255）
```

### 9.2 mutool 查找 [findMutool, line 2444]

```text
findMutool()
  │
  ├── 1. PATH 环境变量
  ├── 2. 同级 bund/ 目录
  ├── 3. /opt/homebrew/bin/mutool
  │
  └── 未找到 → 返回 ""（后续回退 go-fitz）
```

### 9.3 原子写盘 [writeImageAtomically, line 1912]

```text
writeImageAtomically(outPath, writeFn)
  │
  ├── 1. 写入 <outPath>.tmp (同目录，确保同一文件系统)
  ├── 2. os.Rename(.tmp → outPath)
  │
  └── 优势：写入中断时不会产生半截损坏文件
```

---

## 10. 数据流图

```text
PDF 文件
  │
  v
classifyPDFDocument()
  │
  ├── routeRenderCropComplexTransparency
  │   │
  │   v
  │   renderCropPDF()
  │     ├── mutool draw -ppm → readPPM → *image.RGBA
  │     │   │
  │     │   v
  │     │   findLargestRegions()
  │     │     └── flood fill → crop rect
  │     │
  │     └── cropImage() → encodeJPEG/PNG → 原子写盘
  │
  ├── routeRenderWholePageImage
  │   │
  │   v
  │   renderWholePagePDF()
  │     ├── Phase 1: N 个 mutool 子进程渲染
  │     │   └── 输出 PPM 文件
  │     ├── Phase 2: N 路 goroutine 编码
  │     │   └── readPPM → encode → 原子写盘
  │     └── [回退] renderWholePagePDFGoFitz()
  │
  └── routeDirectExtract*
      │
      v
      extractDirectImages()
        └── 逐对象 → writeDirectImage()
            ├── writeDirectImageFast()
            │   ├── [JPEG/JPX 直通] → 直接复制
            │   ├── [8-bit FlateDecode] → sd.Decode() → encode
            │   │   └── [有 SMask] → extractSoftMask() → alpha 合成
            │   └── [CMYK JPEG] → convertCMYKJPEGToOutput()
            │
            └── [回退] pdfcpu.ExtractImage() → 通用解码
```

---

## 11. 已知限制与注意事项

1. **mutool 不可用时性能下降**：go-fitz 串行渲染，且 CGo 在 macOS 上有信号栈限制
2. **SMask 尺寸必须一致**：extractSoftMask 检查尺寸严格一致，不一致时返回 nil
3. **PPM Maxval 255 限制**：readPPM 不支持其他位深度（常见 PPM 均为 255）
4. **CMYK JPEG 依赖 sips**：仅 macOS 可用，其他平台需回退或安装 ImageMagick
5. **flood fill 阈值影响**：`isBackground` RGB>200 的阈值是针对广告设计 PDF 优化的
6. **TMPDIR 必须有效**：mutool 渲染使用临时目录，TMPDIR 无效会导致渲染失败被静默吞掉
7. **`-p` 已占用**：用于合并进度，因此 CPU 并行度用 `-cpu` 而非 `-p`