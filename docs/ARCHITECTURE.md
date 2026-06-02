     1|# PDF Tool 完整架构文档
     2|
     3|Version: v2.1+ | 语言: Go | 多文件结构: main.go + cmd/ + util/ + pdf/ | 不再使用行号引用
     4|
     5|---
     6|
     7|## 1. 系统概述
     8|
     9|PDF Tool 是一个 PDF 图片提取与合并工具。核心功能是从 PDF 中提取内嵌图片，支持两种策略的自动路由：
    10|
    11|- **直取路径** (Direct Extract)：直接从 PDF 对象流中复制或解码图片，最快
    12|- **渲染裁剪** (Render & Crop)：先用 mutool 渲染整页，再通过连通域分析裁剪出图片主体
    13|
    14|渲染引擎优先使用 **mutool draw -ppm**（MuPDF 子进程），不可用时回退到 **go-fitz**（MuPDF CGo）。
    15|
    16|---
    17|
    18|## 2. 入口与初始化
    19|
    20|### 2.1 main() + cmd/ 入口
    21|
    22|```text
    23|main()
    24|  ├─ 解析所有 flag 参数
    25|  │
    26|  ├─ [merge 模式] cmd.RunMerge()
    27|  │   └─ 收集输入 → 压缩（可选）→ 分批合并 → 输出
    28|  │
    29|  ├─ [replace 模式] cmd.Run()
    30|  │   └─ 加载 JSON 配置 → 逐灯位替换图片 → gopdf 表格 → 输出
    31|  │
    32|  ├─ [compress 模式] cmd.RunCompress()
    33|  │   ├─ 单文件：Ghostscript pdfwrite
    34|  │   └─ 目录：并发压缩
    35|  │
    36|  └─ [默认提取模式] cmd.RunExtract()
    37|      └─ convertPDFToImages()
    38|          └─ 路由分类 → 选择提取策略
    39|```
    40|
    41|**全局变量**（main.go 中）：
    42|
    43|| 变量 | 用途 |
    44||------|------|
    45|| `debugLogsEnabled` | `-log` 标志，控制 log.SetOutput |
    46|| `imageMetaEnabled` / `imageMetaJSONEnabled` | 元数据输出开关 |
    47|| `colorCorrectionEnabled` | `-cc` 弃用选项，默认 false |
    48|| `parallelPercent` | `-cpu` 值，ComputeWorkerCount() 使用 |
    49|
    50|---
    51|
    52|## 3. 路由系统 (classifyPDFDocument)
    53|
    54|### 3.1 路由枚举
    55|
    56|```go
    57|const (
    58|    routeRenderCropComplexTransparency  // 复杂透明度 → 渲染后裁剪
    59|    routeDirectExtractTransparency      // 轻度透明度 → 直取
    60|    routeDirectExtractMultiImageStack   // 多图堆叠 → 直取
    61|    routeDirectExtractSingleObject      // 单图 → 直取
    62|    routeRenderWholePageImage           // 整页图 → 整页渲染
    63|)
    64|```
    65|
    66|### 3.2 决策树 [classifyPDFDocument]
    67|
    68|```text
    69|classifyPDFDocument()
    70|  │
    71|  ├─ 逐页扫描 (for each page)
    72|  │   │
    73|  │   ├── 1. 检测裁剪路径 (hasPageContentClip)
    74|  │   │   └─ content stream 中解析 W/W* 操作符
    75|  │   │
    76|  │   ├── 2. 检查 /Group /Transparency
    77|  │   │   └─ 有 Group → anyGroup = true
    78|  │   │       └─ 同时有裁剪路径 → anyGroupWithClip = true
    79|  │   │
    80|  │   ├── 3. 检查无 Group 但有裁剪路径
    81|  │   │   └─ hasClip 且 !anyGroup → anyRealClip = true
    82|  │   │
    83|  │   ├── 4. 检查 Form XObject
    84|  │   │   └─ Resources/XObject 中有 Fm* 前缀 → anyFormXObject = true
    85|  │   │
    86|  │   └── 5. 检查图片对象
    87|  │       └─ ImageObjNrs → 计数/尺寸评估
    88|  │           └─ 单图且尺寸≈页面 → allPagesWholeImage 保持 true
    89|  │
    90|  └── 判定（优先级从高到低）:
    91|      │
    92|      ├── ① anyFormXObject → routeRenderCropComplexTransparency
    93|      │   (Form XObject 意味着嵌套绘制，直取不完整)
    94|      │
    95|      ├── ② anyGroupWithClip → routeRenderCropComplexTransparency
    96|      │   (透明度 + 裁剪路径，必须渲染才能看到正确效果)
    97|      │
    98|      ├── ③ anyRealClip → routeRenderCropComplexTransparency
    99|      │   (无 Group 但有裁剪，如 Illustrator 30.2 导出的 PDF)
   100|      │
   101|      ├── ④ allPagesWholeImage → routeRenderWholePageImage
   102|      │   (所有页的图片尺寸 ≈ 页面尺寸，直接输出整页)
   103|      │
   104|      ├── ⑤ anyGroup → routeDirectExtractTransparency
   105|      │   (有透明度标记但无裁剪，直取保留透明度)
   106|      │
   107|      ├── ⑥ 多页多图 → routeDirectExtractMultiImageStack
   108|      │   (直取每张图片并编号)
   109|      │
   110|      └── ⑦ 默认 → routeDirectExtractSingleObject
   111|          (最简路径，速度最快)
   112|```
   113|
   114|### 3.3 裁剪检测 (hasPageContentClip) [cmd/extract.go]
   115|
   116|```text
   117|hasPageContentClip(ctx, pageDict)
   118|  │
   119|  ├── 获取 /Contents 流
   120|  ├── FlateDecode 解码
   121|  │
   122|  ├── 解析 cm 矩阵 → extractCM() 获取 (a, d)
   123|  │   (cm a 是 x 方向缩放，d 是 y 方向缩放)
   124|  │
   125|  ├── 解析 re 矩形 → extractRect() 获取 (w, h)
   126|  │   (re 定义了裁剪框的尺寸)
   127|  │
   128|  └── 判断：cm_a / clip_w > 1.05 ⇒ 有实际裁剪路径
   129|      (1.05 的阈值用于过滤掉 Illustrator 自动添加的 /Group 标记)
   130|```
   131|
   132|**关键阈值**：`cm_a / clip_w > 1.05`
   133|
   134|- > 1.05：图片被放大了超过裁剪框，说明有实际裁剪，需要 render-crop
   135|- ≤ 1.05：图片就在裁剪框内，无实际裁剪，直取即可
   136|
   137|---
   138|
   139|## 4. 渲染裁剪路径
   140|
   141|### 4.1 renderCropPDF [cmd/extract.go]
   142|
   143|```text
   144|renderCropPDF()
   145|  │
   146|  ├── 逐页渲染 (for each page):
   147|  │   │
   148|  │   ├── A. mutool 可用 → 渲染 + PPM + flood fill
   149|  │   │   ├── mutool draw -r 300 -o p%d.ppm input.pdf <page>
   150|  │   │   ├── readPPM() → *image.RGBA
   151|  │   │   └── findLargestRegions() → 裁剪
   152|  │   │       ├── flood fill 从四角扫描
   153|  │   │       ├── 按面积降序排列
   154|  │   │       └── 取 top-1 区域
   155|  │   │
   156|  │   └── B. mutool 不可用 → go-fitz 渲染 + flood fill
   157|  │       ├── fitz.New() + doc.Image()
   158|  │       └── findLargestRegions() → 裁剪
   159|  │
   160|  └── 编码输出 JPEG/PNG
   161|```
   162|
   163|**flood fill 裁剪算法** (findLargestRegions [cmd/extract.go])：
   164|
   165|```text
   166|findLargestRegions(img, maxRegions)
   167|  │
   168|  ├── 转 RGBA
   169|  ├── 从四角扫描像素
   170|  │
   171|  ├── 对每个非背景像素 (isBackground=RGB>200):
   172|  │   └── floodFillRegion() 四邻域扩散
   173|  │       ├── 记录外接矩形 (minX, maxX, minY, maxY)
   174|  │       └── 记录像素面积
   175|  │
   176|  ├── 过滤：面积 < 50000 视为噪声
   177|  └── 按面积降序取前 maxRegions 个
   178|```
   179|
   180|### 4.2 renderWholePagePDF [cmd/extract.go]
   181|
   182|```text
   183|renderWholePagePDF()
   184|  │
   185|  ├── Phase 1: 并行 mutool 渲染
   186|  │   │
   187|  │   ├── n = ComputeWorkerCount(parallelPercent)
   188|  │   ├── chunkSize = ceil(pageCount / n)
   189|  │   │
   190|  │   ├── 启动 n 个子进程 (sync.WaitGroup)
   191|  │   │   ├── worker 0: mutool draw -r 300 -o <tmp>/p%d.ppm input.pdf 1-chunkSize
   192|  │   │   ├── worker 1: mutool draw ... input.pdf chunkSize+1-2*chunkSize
   193|  │   │   └── ...
   194|  │   │
   195|  │   └── 任一失败 → 回退串行 mutool → 回退 go-fitz
   196|  │
   197|  ├── Phase 2: 并行 PPM→编码
   198|  │   │
   199|  │   ├── sem = make(chan struct{}, n)  // goroutine 池容量
   200|  │   ├── 遍历每页 → goroutine 获取 token
   201|  │   │   ├── readPPM(path) → *image.RGBA
   202|  │   │   ├── 裁剪（如需）
   203|  │   │   ├── encodeJPEG / encodePNG
   204|  │   │   └── 原子写盘
   205|  │
   206|  └── 清理临时目录
   207|```
   208|
   209|**回退链**：`mutool 并行 → mutool 串行 → go-fitz`
   210|
   211|### 4.3 renderWholePagePDFGoFitz [cmd/extract.go]
   212|
   213|```text
   214|renderWholePagePDFGoFitz()
   215|  │
   216|  ├── mutool 不可用时的回退路径
   217|  ├── 串行：打开一次 doc，逐页 doc.Image()
   218|  └── 每页编码 JPEG/PNG → 原子写盘
   219|```
   220|
   221|**注意**：go-fitz 使用 CGo，在 macOS 上不能 goroutine 并行（信号栈限制），只能串行。
   222|
   223|---
   224|
   225|## 5. 直取路径
   226|
   227|### 5.1 extractDirectImages [cmd/extract.go]
   228|
   229|```text
   230|extractDirectImages()
   231|  │
   232|  ├── 逐页扫描 (for each page):
   233|  │   ├── 获取 /Resources/XObject
   234|  │   │
   235|  │   ├── 遍历所有 /Image 对象
   236|  │   │   │
   237|  │   │   ├── ① 直接引用图片
   238|  │   │   │   └── writeDirectImage()
   239|  │   │   │       ├── writeDirectImageFast() 尝试
   240|  │   │   │       │   ├── true → 成功，继续下一张
   241|  │   │   │       │   └── false → 回退 pdfcpu.ExtractImage
   242|  │   │   │
   243|  │   │   └── ② Form XObject 中嵌套的图片
   244|  │   │       └── 递归遍历 → writeDirectImage()
   245|  │   │
   246|  │   └── 每页编号从 0 开始，按顺序递增
   247|  │
   248|  └── 元数据收集（通过全局 collector 统一输出）
   249|```
   250|
   251|### 5.2 writeDirectImage [cmd/extract.go]
   252|
   253|```text
   254|writeDirectImage(ctx, ctxMu, inputFile, pageNr, objNr, ...)
   255|  │
   256|  ├── 1. 获取图片流字典 sd
   257|  ├── 2. writeDirectImageFast() — 尝试快速路径
   258|  │
   259|  └── 3. 快速路径失败 → 通用解码（锁保护）
   260|      ├── ctxMu.Lock()
   261|      ├── pdfcpu.ExtractImage(ctx, pageNr, objNr)
   262|      ├── 解码并编码输出
   263|      └── ctxMu.Unlock()
   264|```
   265|
   266|### 5.3 writeDirectImageFast [cmd/extract.go] — 快速路径决策树
   267|
   268|**这是整个程序最复杂的函数，需要完全理解每个分支。**
   269|
   270|```text
   271|writeDirectImageFast()
   272|  │
   273|  ├──── 条件检查 ──────────────────────────────────────
   274|  │
   275|  ├── A. sd.Content 为空且 sd.Raw 非空 → sd.Decode()
   276|  │   (关键修复：FlateDecode 过滤器时 pdfcpu 不自动解码)
   277|  │
   278|  ├── B. 直通复制路径 ────────────────────────────────
   279|  │   │
   280|  │   └── 条件：JPEG/JPX 编码 且 过滤器非 FlateDecode
   281|  │       │
   282|  │       ├── B1. JPX → convertJPXToOutput()
   283|  │       ├── B2. JPEG → 直接写 .jpg
   284|  │       └── B3. CMYK JPEG + 用户指定 PNG
   285|  │           └── convertCMYKJPEGToOutput() → sips 转换
   286|  │
   287|  ├── C. 8-bit 快速解码路径 ──────────────────────────
   288|  │   │
   289|  │   └── 条件：8-bit RGB/Gray 或带 SMask
   290|  │       │
   291|  │       ├── C1. sd.Decode() 解码图像数据
   292|  │       │
   293|  │       ├── C2. 检测 SMask (/SMask 键)
   294|  │       │   ├── 有 SMask → extractSoftMask()
   295|  │       │   │   ├── 解码 SMask 灰度流
   296|  │       │   │   ├── 检查尺寸一致
   297|  │       │   │   └── 合成 RGBA (alpha 通道)
   298|  │       │   └── 无 SMask → 直接转为 RGBA
   299|  │       │
   300|  │       ├── C3. 自动格式判断
   301|  │       │   ├── CMYK JPEG 自动输出 jpg
   302|  │       │   └── 否则使用用户指定格式
   303|  │       │
   304|  │       └── C4. encodeJPEG / encodePNG → 原子写盘
   305|  │
   306|  └── D. 回退信号 ────────────────────────────────────
   307|      │
   308|      └── 以上均不满足 → 返回 false, nil
   309|          (调用者走 pdfcpu.ExtractImage 通用解码)
   310|```
   311|
   312|**快速路径 vs 通用解码的性能对比**：
   313|
   314|| 场景 | 快速路径 | 通用解码 | 加速 |
   315||------|---------|---------|------|
   316|| FlateDecode+SMask (2.pdf) | 0.28s | 6.0s | 21x |
   317|| 直接 JPEG 复制 | ~0.01s | ~0.5s | 50x |
   318|| 8-bit RGB FlateDecode | ~0.02s | ~0.3s | 15x |
   319|
   320|---
   321|
   322|## 6. 单页渲染辅助函数
   323|
   324|### 6.1 renderSinglePageCropMutool [cmd/extract.go]
   325|
   326|（函数名含 Pdftoppm 但实际已改为 mutool 渲染）
   327|
   328|```text
   329|renderSinglePageCropPdftoppm(inputFile, pageNr, dpi, cropX, cropY, cropW, cropH)
   330|  │
   331|  ├── 1. mutool draw -r <dpi> -o <tmp>/p%d.ppm input.pdf <pageNr>
   332|  ├── 2. readPPM() → *image.RGBA
   333|  ├── 3. 计算像素裁剪区域 (PDF 点 → 像素：* DPI/72)
   334|  ├── 4. cropImage() 裁剪并编码
   335|  └── 5. 原子写盘
   336|```
   337|
   338|### 6.2 renderSinglePageCrop [cmd/extract.go]
   339|
   340|```text
   341|renderSinglePageCrop(inputFile, pageNr, dpi)
   342|  │
   343|  ├── 1. go-fitz 渲染整页
   344|  ├── 2. findLargestRegions() → flood fill 裁剪
   345|  └── 3. 编码输出
   346|```
   347|
   348|**两者区别**：
   349|- `renderSinglePageCropPdftoppm`：mutool 渲染 + cm 矩阵精确裁剪（更快，颜色正确）
   350|- `renderSinglePageCrop`：go-fitz 渲染 + flood fill 裁剪（回退路径，稍慢）
   351|
   352|---
   353|
   354|## 7. 色彩处理
   355|
   356|### 7.1 CMYK JPEG 检测 [isCMYKJPEG, line 2014]
   357|
   358|```text
   359|isCMYKJPEG(data)
   360|  │
   361|  └── 扫描 JPEG SOF0 marker (0xFF 0xC0)
   362|      └── 第 7 个字节 = 分量数
   363|          ├── 3 → RGB/YUV
   364|          └── 4 → CMYK
   365|```
   366|
   367|### 7.2 CMYK→RGB 转换 [convertCMYKJPEGToOutput, line 2058]
   368|
   369|```text
   370|convertCMYKJPEGToOutput(data, outPath)
   371|  │
   372|  ├── 1. 写入临时文件
   373|  ├── 2. sips -s format png 转换
   374|  │   (macOS 内置 sips 工具，颜色准确)
   375|  └── 3. sips 失败 → Go 标准库解码（部分变体）
   376|```
   377|
   378|### 7.3 SMask 透明合成 [extractSoftMask, line 2111]
   379|
   380|```text
   381|extractSoftMask(ctx, sd, objNr, w, h)
   382|  │
   383|  ├── 从图片字典获取 /SMask
   384|  ├── 解码 SMask 流（灰度图像）
   385|  ├── 检查尺寸一致 (w_smask == w_main && h_smask == h_main)
   386|  ├── 检查位深 == 8
   387|  └── 返回 alpha 通道数据 ([]byte)
   388|```
   389|
   390|在写图片时，SMask 数据作为 alpha 通道合成到 RGBA 像素中，输出 PNG 保留透明度。
   391|
   392|---
   393|
   394|## 8. 并行度计算 [computeWorkerCount, line 2472]
   395|
   396|```text
   397|computeWorkerCount()
   398|  │
   399|  ├── numCPU = runtime.NumCPU()
   400|  │
   401|  ├── parallelPercent ≤ 0 → return 1 (串行)
   402|  ├── parallelPercent ≥ 100 → return numCPU (全核)
   403|  │
   404|  └── n = (numCPU * parallelPercent + 50) / 100  (四舍五入)
   405|      └── max(1, n)
   406|```
   407|
   408|**示例**：
   409|
   410|| `-cpu` 值 | 4 核机器 | 8 核机器 | 14 核机器 |
   411||-----------|---------|---------|----------|
   412|| 0 | 1 | 1 | 1 |
   413|| 25 | 1 | 2 | 4 |
   414|| 50 | 2 | 4 | 7 |
   415|| 100 | 4 | 8 | 14 |
   416|
   417|---
   418|
   419|## 9. 辅助工具函数
   420|
   421|### 9.1 PPM 读取 [readPPM, line 2498]
   422|
   423|```text
   424|readPPM(path)
   425|  │
   426|  ├── 解析 PPM P6 头部：P6\n<w> <h>\n255\n
   427|  ├── 跳过注释行 (# 开头)
   428|  ├── 读取 RGB 像素数据（每像素 3 字节）
   429|  └── 转为 *image.RGBA（alpha=255）
   430|```
   431|
   432|### 9.2 mutool 查找 [findMutool, line 2444]
   433|
   434|```text
   435|findMutool()
   436|  │
   437|  ├── 1. PATH 环境变量
   438|  ├── 2. 同级 bund/ 目录
   439|  ├── 3. /opt/homebrew/bin/mutool
   440|  │
   441|  └── 未找到 → 返回 ""（后续回退 go-fitz）
   442|```
   443|
   444|### 9.3 原子写盘 [writeImageAtomically, line 1912]
   445|
   446|```text
   447|writeImageAtomically(outPath, writeFn)
   448|  │
   449|  ├── 1. 写入 <outPath>.tmp (同目录，确保同一文件系统)
   450|  ├── 2. os.Rename(.tmp → outPath)
   451|  │
   452|  └── 优势：写入中断时不会产生半截损坏文件
   453|```
   454|
   455|---
   456|
   457|## 10. 数据流图
   458|
   459|```text
   460|PDF 文件
   461|  │
   462|  v
   463|classifyPDFDocument()
   464|  │
   465|  ├── routeRenderCropComplexTransparency
   466|  │   │
   467|  │   v
   468|  │   renderCropPDF()
   469|  │     ├── mutool draw -ppm → readPPM → *image.RGBA
   470|  │     │   │
   471|  │     │   v
   472|  │     │   findLargestRegions()
   473|  │     │     └── flood fill → crop rect
   474|  │     │
   475|  │     └── cropImage() → encodeJPEG/PNG → 原子写盘
   476|  │
   477|  ├── routeRenderWholePageImage
   478|  │   │
   479|  │   v
   480|  │   renderWholePagePDF()
   481|  │     ├── Phase 1: N 个 mutool 子进程渲染
   482|  │     │   └── 输出 PPM 文件
   483|  │     ├── Phase 2: N 路 goroutine 编码
   484|  │     │   └── readPPM → encode → 原子写盘
   485|  │     └── [回退] renderWholePagePDFGoFitz()
   486|  │
   487|  └── routeDirectExtract*
   488|      │
   489|      v
   490|      extractDirectImages()
   491|        └── 逐对象 → writeDirectImage()
   492|            ├── writeDirectImageFast()
   493|            │   ├── [JPEG/JPX 直通] → 直接复制
   494|            │   ├── [8-bit FlateDecode] → sd.Decode() → encode
   495|            │   │   └── [有 SMask] → extractSoftMask() → alpha 合成
   496|            │   └── [CMYK JPEG] → convertCMYKJPEGToOutput()
   497|            │
   498|            └── [回退] pdfcpu.ExtractImage() → 通用解码
   499|```
   500|
   501|