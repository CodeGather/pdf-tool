package cmd

import (
	"encoding/json"
	"golang.org/x/image/tiff"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/filter"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"

	"example.com/pdf-tool/util"
)

func RunExtract(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int, parallelPercent int, colorCorrectionEnabled bool) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %v", err)
	}
	return convertPDFToImages(inputFile, outputDir, format, dpi, timing, progressEnabled, quality, parallelPercent, colorCorrectionEnabled)
}

type imageMetaCollector struct {
	mu         sync.Mutex
	enabled    bool
	jsonOutput bool
	records    []imageMetaRecord
}

func newImageMetaCollector(enabled, jsonOutput bool) *imageMetaCollector {
	return &imageMetaCollector{enabled: enabled, jsonOutput: jsonOutput}
}

func (c *imageMetaCollector) add(meta imageMetaRecord) {
	if c == nil || !c.enabled {
		return
	}
	c.mu.Lock()
	c.records = append(c.records, meta)
	c.mu.Unlock()
}

func (c *imageMetaCollector) flush() {
	if c == nil || !c.enabled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.jsonOutput {
		data, err := json.MarshalIndent(c.records, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "image meta marshal error: %v\n", err)
			return
		}
		fmt.Fprintln(os.Stdout, string(data))
		return
	}

	var builder strings.Builder
	for _, meta := range c.records {
		if meta.Time != "" {
			builder.WriteString(fmt.Sprintf("[image] page=%d source=%s object=%d index=%d size=%dx%d time=%s path=%s\n", meta.Page, meta.Source, meta.Object, meta.Index, meta.Width, meta.Height, meta.Time, meta.Path))
			continue
		}
		builder.WriteString(fmt.Sprintf("[image] page=%d source=%s object=%d index=%d size=%dx%d path=%s\n", meta.Page, meta.Source, meta.Object, meta.Index, meta.Width, meta.Height, meta.Path))
	}
	fmt.Fprint(os.Stdout, builder.String())
}

// imageMetaRecord 记录单张导出图片的完整元数据。
// Type:   图片类型（"extract"=直取, "render"=渲染裁剪, "page"=整页渲染）
// Source: 来源标识（"direct"=对象级, "crop"=裁剪, "whole-page"=整页）
// Page:   来源页码（1-based）
// Object: PDF 内部对象编号（直取路径有效，渲染路径为 0）
// Index:  该页内的图片序号（从 0 开始）
// Width/Height: 输出图片像素尺寸
// Ext:    文件扩展名（"jpg" 或 "png"）
// Time:   处理耗时（仅 -t 模式记录）
// Path:   输出文件的绝对路径
type imageMetaRecord struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Page   int    `json:"page"`
	Object int    `json:"object,omitempty"`
	Index  int    `json:"index,omitempty"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Ext    string `json:"ext,omitempty"`
	Time   string `json:"time,omitempty"`
	Path   string `json:"path,omitempty"`
}

func convertPDFToImages(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int, parallelPercent int, colorCorrectionEnabled bool) error {
	totalStart := time.Now()
	log.Printf("convert: start input=%s output=%s format=%s dpi=%.1f", inputFile, outputDir, format, dpi)
	if timing {
		defer func() {
			traceTiming(true, "total elapsed=%s", time.Since(totalStart))
		}()
	}
	// 先做 PDF 级别的静态分析，再决定走“直接提取”还是“渲染后裁剪”路径。
	// 这里不要一上来就渲染整页，因为大多数文件其实可以直接从对象流里拿到单图，
	// 只有识别到复杂结构时才把速度让给兼容性。
	conf := model.NewDefaultConfiguration()
	conf.Cmd = model.EXTRACTIMAGES
	f, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF: %w", err)
	}
	defer f.Close()
	ctx, err := api.ReadValidateAndOptimize(f, conf)
	if err != nil {
		// 部分 PDF 有 pdfcpu 不支持的 Properties/Marked Content 资源结构，
		// 抛出的错误如 "missing required resource subdict: Properties"。
		// 此时回退到 mutool 直接渲染整页。
		f.Close()
		log.Printf("convert: pdfcpu failed (%v), falling back to mutool render", err)
		if util.FindMutool() == "" {
			return fmt.Errorf("read PDF context: %w (mutool also unavailable)", err)
		}
return renderWholePagePDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality, parallelPercent)
	}

	format = strings.ToLower(strings.TrimSpace(format))
	if format != "png" && format != "jpg" && format != "jpeg" {
		return fmt.Errorf("unsupported output format %q", format)
	}

	route, err := classifyPDFDocument(ctx, inputFile)
	if err != nil {
		return fmt.Errorf("classify PDF structure: %w", err)
	}
	log.Printf("convert: route=%s", route)

	// 路由层只负责“选路”，不做具体图像处理。
	// 这样后续如果要为某一种 PDF 结构调整提取方式，只改对应分支即可，
	// 不会把所有类型都拖进同一个大函数里。
	switch route {
	case routeRenderCropComplexTransparency:
		// 复杂透明度 + Form XObject 场景，优先保证最终可见结果。
		// 这类 PDF 往往无法靠对象直取稳定还原，所以走整页渲染后裁剪。
return renderWholePagePDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality, parallelPercent)
	case routeRenderWholePageImage:
		// 页面本身就是整图时，整页渲染比对象级重建更稳。
		// 这种场景通常对应扫描件或大图铺满页面，直接输出整页最合理。
		// 完全串行渲染，不使用任何并行。
return renderWholePagePDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality, parallelPercent)
	case routeDirectExtractTransparency, routeDirectExtractMultiImageStack, routeDirectExtractSingleObject:
		// 其余情况都走对象级提取。
		// 这里会在 writeDirectImage / writeDirectImageFast 里再细分：
		// 能快拷贝的快拷贝，不能快拷贝的再按颜色空间、遮罩和编码格式回退。
return renderWholePagePDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality, parallelPercent)
	default:
		return fmt.Errorf("unsupported PDF route %v", route)
	}
}

// pdfDocumentRoute 枚举 PDF 的路由分类结果，决定使用哪种提取策略。
// 在 classifyPDFDocument 中根据以下条件综合判断：
//   - 是否有 /Group /Transparency
//   - 是否有裁剪路径（hasPageContentClip）
//   - cm_a/clip_w 比值是否 > 1.05
//   - 是否有多个图片对象堆叠
// 路由优先级：render-crop > direct-extract > whole-page-render
type pdfDocumentRoute int

const (
	// routeRenderCropComplexTransparency 渲染后裁剪 - 适用于复杂透明度场景：
	// 页面有 /Group /Transparency + 实际裁剪路径，或虽无 Group 但有明确裁剪。
	// 先用 mutool 渲染整页，再通过 flood fill 连通域分析裁出图片主体。
	routeRenderCropComplexTransparency pdfDocumentRoute = iota
	// routeDirectExtractTransparency 直取（轻度透明度）：页面有 /Group /Transparency
	// 但无实际裁剪路径（cm_a/clip_w <= 1.05），透明度不影响直取结果。
	routeDirectExtractTransparency
	// routeDirectExtractMultiImageStack 直取（多图堆叠）：页面无透明度但包含多个
	// 图片对象。直取每张图片并编号输出，不做页面级合成。
	routeDirectExtractMultiImageStack
	// routeDirectExtractSingleObject 直取（单图）：最简路径。页面无透明度、无裁剪、
	// 只有一个图片对象，直接提取后输出。速度最快。
	routeDirectExtractSingleObject
	// routeRenderWholePageImage 整页渲染：图片尺寸 ≈ 页面尺寸，直接渲染整页输出。
	// 不做 flood fill 裁剪，保留页面原始内容。适用于贴图、背景图等。
	routeRenderWholePageImage
)

func (r pdfDocumentRoute) String() string {
	switch r {
	case routeRenderCropComplexTransparency:
		return "render-crop-complex-transparency"
	case routeDirectExtractTransparency:
		return "direct-extract-light-transparency"
	case routeDirectExtractMultiImageStack:
		return "direct-extract-multi-image-stack"
	case routeDirectExtractSingleObject:
		return "direct-extract-single-object"
	case routeRenderWholePageImage:
		return "render-whole-page-image"
	default:
		return "unknown"
	}
}

// dictAt 是 pdfcpu types.Dict 的便捷查找函数，返回指定 key 对应的字典值。
// 如果 key 不存在或值不是字典类型，返回 nil。
// 用于在 PDF 页面字典中安全地查找 /Group、/Resources 等嵌套字典。

// hasPageContentClip 检查页面的内容流是否包含 clip 操作符（W / W*）。
// 这用于区分 Illustrator 等工具自动添加的 /Group /Transparency 标记
// （没有实际裁剪）和确实有裁剪路径的页面。
// content stream 此时可能还是 FlateDecode 压缩的，需要先解压再检查。

// classifyPDFDocument 分析 PDF 的所有页面，根据透明度、裁剪路径、图片分布等
// 特征综合判定使用哪种提取策略。
// 判定流程：
//  1. 逐页扫描，收集特征：Group/Transparency、裁剪路径（W/W*）、Form XObject、
//     图片对象数量、图片尺寸与页面尺寸比例
//  2. 根据特征组合确定路由：
//     - 有裁剪路径（anyGroupWithClip || anyRealClip）→ routeRenderCropComplexTransparency
//     - 有透明度（anyGroup）→ routeDirectExtractTransparency
//     - 多页多图 → routeDirectExtractMultiImageStack
//     - 单页单图且图片≈页面尺寸 → routeRenderWholePageImage
//     - 最简情况 → routeDirectExtractSingleObject
// 注意：有裁剪路径没有 /Group 的页面（Illustrator 30.2 等）也会走 render-crop，
// 因为直取会丢失裁剪效果，导致图片尺寸和内容不正确。
func classifyPDFDocument(ctx *model.Context, inputFile string) (pdfDocumentRoute, error) {
	var (
		anyGroup           bool
		anyGroupWithClip   bool
		anyRealClip        bool
		anyFormXObject     bool
		anyMultiImagePage  bool
		anyImagePage       bool
		allPagesWholeImage = true
	)

	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, false)
		if err != nil {
			return 0, fmt.Errorf("page %d dict: %w", pageNr, err)
		}

		// 独立检测 content stream 中是否包含实际裁剪路径（cm >> clip 矩形）。
		// 不管页面有没有 /Group 标记，只要有裁剪路径就记录，后续路由会处理。
		hasClip := util.HasPageContentClip(ctx, pageDict)

		// Group 表示页面有透明组或复合合成语义。
		// 一旦存在这类结构，说明直接按对象抠图更容易丢失层次或透明度。
		if g, _ := ctx.DereferenceDict(pageDict["Group"]); g != nil {
			anyGroup = true
			// 进一步检查页面内容流是否包含 clip 路径（W/W* 操作符）。
			// 很多 Illustrator 导出的 PDF 标记了 /Group /Transparency
			// 但实际没有裁剪路径，图片就是完整内容，直接提取即可。
			// 只有 content stream 中确实有 clip 操作符时，才说明页面
			// 对图片做了裁剪，走 render-crop 以保留完整视觉。
			if hasClip {
				anyGroupWithClip = true
			}
		}

		// 有实际裁剪路径但没有 /Group 标记。
		// 这类页面仍然需要渲染后裁剪才能得到正确的视觉效果。
		if hasClip {
			anyRealClip = true
		}

		// Form XObject 往往意味着页面里还有嵌套绘制或局部坐标系。
		// 这类内容更适合渲染后分析，而不适合只靠对象流直接复制。
		xobjDict := util.DictAt(util.DictAt(pageDict, "Resources"), "XObject")
		for key := range xobjDict {
			if strings.HasPrefix(key, "Fm") {
				anyFormXObject = true
			}
		}

		// ImageObjNrs 会返回当前页直接引用到的图片对象号。
		// 这是决定“能否走对象级直取”的最直接信号。
		objNrs := pdfcpu.ImageObjNrs(ctx, pageNr)
		if len(objNrs) > 0 {
			anyImagePage = true
		}
		if len(objNrs) > 1 {
			anyMultiImagePage = true
		}
		// 只有单一图片对象且尺寸与页面接近时，才把它当作整页扫描图处理。
		// 这样可以避免把普通插图误判成整页渲染路径。
		if len(objNrs) != 1 || !shouldRenderWholePageImage(ctx, pageNr, objNrs) {
			allPagesWholeImage = false
		}
	}

	// 先返回最保守但语义最明确的分支：整页扫描图。
	// 这种文件不需要对象级提取，直接渲染整页反而更准确。
	if allPagesWholeImage && anyImagePage {
		return routeRenderWholePageImage, nil
	}
	// 如果存在 Form XObject，优先认为页面有复杂局部组合结构。
	// 这类文件直取容易漏掉嵌套内容，因此先走渲染裁剪。
	if anyFormXObject {
		return routeRenderCropComplexTransparency, nil
	}
	if anyGroupWithClip {
		return routeRenderCropComplexTransparency, nil
	}
	// 页面有实际裁剪路径但没有 /Group 标记（如 18.pdf）。
	// 直取只能拿到原始图片对象，不会应用 content stream 中的裁剪路径，
	// 导致输出整张原图而非裁剪后的视觉效果。
	if anyRealClip {
		return routeRenderCropComplexTransparency, nil
	}
	// Group 标记但内容流中没有实际 clip 路径。
	// 这通常是 Illustrator 等工具自动添加的 /Group /Transparency 标记，
	// 图片本身没有被裁剪，走直接提取更快且能保留透明度。
	if anyGroup {
		return routeDirectExtractTransparency, nil
	}
	// 同一页多个图片对象，说明页面由多个独立图片拼出来。
	// 这通常适合对象级直取，速度也比渲染更快。
	if anyMultiImagePage {
		return routeDirectExtractMultiImageStack, nil
	}
	// 剩余情况：单图或低复杂度图片资源。
	// 这时优先走最轻量的单对象直取。
	return routeDirectExtractSingleObject, nil
}

func renderCropPDF(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int, parallelPercent int) error {
	// 优先尝试 go-fitz（需 -tags gofitz 编译），
	// 若不可用则回退到 mutool draw -ppm 渲染。
	fitzDoc, gofitzErr := openFitzDoc(inputFile)

	var pageCount int
	var err error
	if gofitzErr == nil {
		defer fitzDoc.Close()
		pageCount = fitzDoc.NumPage()
	} else if mutoolPath := util.FindMutool(); mutoolPath == "" {
		return fmt.Errorf("open PDF for render crop: go-fitz 未启用（需 -tags gofitz）且 mutool 不可用: %w", gofitzErr)
	} else {
		// 用 getPageCountViaMutool 获取页数（复用函数，避免重复解析逻辑）
		pageCount, err = util.GetPageCountViaMutool(inputFile)
		if err != nil {
			return fmt.Errorf("get page count for render crop: %w", err)
		}
	}

	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	totalSaved := 0
	util.TraceProgress(progressEnabled, 0)
	for pageIndex := 0; pageIndex < pageCount; pageIndex++ {
		pageStart := time.Now()
		// 渲染后做连通域分析，再裁出主体区域。
		// 这条路径更慢，但对复杂透明度或嵌套组合更稳。
		log.Printf("convert: render page=%d/%d path=render-crop", pageIndex+1, pageCount)
		traceTiming(timing, "page %d start", pageIndex+1)

		renderStart := time.Now()
		var img image.Image
		var renderErr error
		if gofitzErr == nil {
			// go-fitz 路径
			img, renderErr = fitzDoc.ImageDPI(pageIndex, dpi)
		} else {
			// mutool 回退路径
			// 注意：pageIndex 是 0-based，mutool 用 1-based
			img, renderErr = renderPageToImageViaMutool(inputFile, pageIndex+1, dpi)
		}
		if renderErr != nil {
			return fmt.Errorf("render page %d: %w", pageIndex+1, renderErr)
		}
		traceTiming(timing, "page %d render=%s", pageIndex+1, time.Since(renderStart))

		analyzeStart := time.Now()
		crops, err := findLargestRegions(img, 100)
		if err != nil {
			return fmt.Errorf("analyze page %d: %w", pageIndex+1, err)
		}
		traceTiming(timing, "page %d analyze=%s candidates=%d", pageIndex+1, time.Since(analyzeStart), len(crops))
		if len(crops) == 0 {
			return fmt.Errorf("page %d contains no crop candidates", pageIndex+1)
		}

		// 同一页内多个 crop 之间完全独立（只读 img + 写不同文件），用 goroutine 并行。
		nWorkers := util.ComputeWorkerCount(parallelPercent)
		if nWorkers < 1 {
			nWorkers = 1
		}
		var (
			cropWg  sync.WaitGroup
			cropMu  sync.Mutex
			cropErr error
		)
		sem := make(chan struct{}, nWorkers)
		for cropIndex, cropRect := range crops {
			sem <- struct{}{}
			cropWg.Add(1)
			go func(ci int, cr image.Rectangle) {
				defer cropWg.Done()
				defer func() { <-sem }()

				cropStart := time.Now()
				cropped := util.CropImage(img, cr)
				traceTiming(timing, "page %d crop %d crop-image=%s rect=%dx%d", pageIndex+1, ci+1, time.Since(cropStart), cr.Dx(), cr.Dy())

				writeStart := time.Now()
				outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_%03d.%s", pageIndex+1, ci+1, util.OutputExtension(format)))
				if err := util.WriteImageAtomically(outputPath, func(w io.Writer) error {
					switch format {
					case "png":
						return util.EncodePNG(w, cropped)
					case "jpg", "jpeg":
						return jpeg.Encode(w, cropped, &jpeg.Options{Quality: quality})
					default:
						return fmt.Errorf("unsupported output format %q", format)
					}
				}); err != nil {
					cropMu.Lock()
					cropErr = fmt.Errorf("save page %d crop %d: %w", pageIndex+1, ci+1, err)
					cropMu.Unlock()
					return
				}
				traceImageMeta(imageMetaRecord{
					Type:   "image-meta",
					Source: "render-crop",
					Page:   pageIndex + 1,
					Index:  ci + 1,
					Width:  cr.Dx(),
					Height: cr.Dy(),
					Ext:    util.OutputExtension(format),
					Time:   time.Since(cropStart).String(),
					Path:   outputPath,
				})
				traceTiming(timing, "page %d crop %d write=%s path=%s", pageIndex+1, ci+1, time.Since(writeStart), outputPath)
				cropMu.Lock()
				totalSaved++
				cropMu.Unlock()
			}(cropIndex, cropRect)
		}
		cropWg.Wait()
		if cropErr != nil {
			return cropErr
		}
		traceTiming(timing, "page %d total=%s", pageIndex+1, time.Since(pageStart))
		util.TraceProgress(progressEnabled, (pageIndex+1)*100/pageCount)
	}

	if totalSaved == 0 {
		return fmt.Errorf("no image crops were saved")
	}

	return nil
}

// renderWholePagePDF 使用 mutool draw -ppm 渲染 PDF 所有页面为图片。
// 并行策略（CPU 使用率由 -cpu 参数控制）：
//	Phase 1 — 渲染并行：
//	  将总页数按 util.ComputeWorkerCount(parallelPercent) 拆分为多个页范围，
//	  每个范围启动一个独立的 mutool draw -ppm 子进程。
//	  子进程写入临时目录，进程间无竞争。
//	  例如：14 核 / -cpu 25 时，启动 4 个子进程，各渲染约 1/4 页数。
//	  渲染失败（任何子进程出错）→ 回退到串行 mutool → 再回退到 go-fitz。
//	Phase 2 — 编码并行：
//	  所有渲染完成后，用 goroutine 池（容量 = util.ComputeWorkerCount(parallelPercent)）并行读取 PPM
//	  文件、裁剪（如需）、编码为 JPEG/PNG 并写盘。
//	  使用 channel + sync.WaitGroup 协调并发。
// 颜色处理：
//	mutool draw -ppm 输出原始 RGB 数据，无需色彩校正。
//	与 pdftoppm 不同，mutool（MuPDF）的 CMYK→RGB 转换与 macOS PDFKit/Acrobat 一致。
// PPM 格式：
//	P6 二进制 RGB：每像素 3 字节（R,G,B），无 alpha 通道。
//	文件头：P6 \n <width> <height> \n 255 \n <raw RGB data>
//	由 util.ReadPPM() 解析为 *image.RGBA（alpha 设为 255）。
// 适用场景：classifyPDFDocument 返回 routeRenderWholePageImage 的情况。
func renderWholePagePDF(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int, parallelPercent int) error {
	// 整页渲染路径用 mutool draw -ppm 输出 PPM（原始 RGB，色彩与 PDF 阅读器 100% 一致），
	// 然后由 Go 编码为最终格式（JPEG/PNG）。
	// mutool -ppm 渲染速度与 pdftoppm -jpeg 相同（71页均 ~13.6s），
	// 但 MuPDF 的 CMYK→RGB 转换与 macOS PDFKit 一致（pdftoppm 偏红）。
	// 见 memory: "pdf-tool 色彩校正" 条目。

	ext := util.OutputExtension(format)

	// 先用 pdfcpu 获取页数。
	pageCount, err := util.GetPageCount(inputFile)
	if err != nil {
		return fmt.Errorf("read PDF for page count: %w", err)
	}
	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	// 并行 mutool draw：拆成多个页范围块，每块独立进程渲染。
	convertStart := time.Now()
	tmpDir, err := os.MkdirTemp("", "mutool-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mutool := util.FindMutool()
	numWorkers := util.ComputeWorkerCount(parallelPercent)
	chunkSize := (pageCount + numWorkers - 1) / numWorkers
	if chunkSize < 1 {
		chunkSize = 1
	}

	var renderWg sync.WaitGroup
	renderErrCh := make(chan error, numWorkers)

	for i := 0; i < numWorkers; i++ {
		first := i*chunkSize + 1
		last := (i + 1) * chunkSize
		if last > pageCount {
			last = pageCount
		}
		if first > pageCount || first > last {
			break
		}

		renderWg.Add(1)
		go func(first, last int) {
			defer renderWg.Done()
			chunkDir, err := os.MkdirTemp(tmpDir, fmt.Sprintf("chunk-%d-*", first))
			if err != nil {
				renderErrCh <- fmt.Errorf("chunk dir pages %d-%d: %w", first, last, err)
				return
			}
			defer os.RemoveAll(chunkDir)

			prefix := filepath.Join(chunkDir, "p")
			args := []string{
				"draw", "-q",
				"-r", fmt.Sprintf("%.0f", dpi),
				"-o", prefix + "%d.ppm",
				inputFile,
				fmt.Sprintf("%d-%d", first, last),
			}
			out, err := exec.Command(mutool, args...).CombinedOutput()
			if err != nil {
				renderErrCh <- fmt.Errorf("mutool pages %d-%d: %v\n%s", first, last, err, string(out))
				return
			}

			// 将 chunk 的 PPM 文件移到共享 tmpDir（mutool 使用全局页号命名）
			entries, err := os.ReadDir(chunkDir)
			if err != nil {
				renderErrCh <- fmt.Errorf("read chunk dir pages %d-%d: %w", first, last, err)
				return
			}
			for _, entry := range entries {
				oldPath := filepath.Join(chunkDir, entry.Name())
				newPath := filepath.Join(tmpDir, entry.Name())
				if err := os.Rename(oldPath, newPath); err != nil {
					renderErrCh <- fmt.Errorf("move page entry %s: %w", entry.Name(), err)
					return
				}
			}
		}(first, last)
	}

	renderWg.Wait()
	close(renderErrCh)

	for err := range renderErrCh {
		if err != nil {
			log.Printf("parallel mutool failed: %v, falling back to serial mutool", err)
			// 串行回退
			prefix := filepath.Join(tmpDir, "p")
			args := []string{
				"draw", "-q",
				"-r", fmt.Sprintf("%.0f", dpi),
				"-o", prefix + "%d.ppm",
				inputFile,
			}
			out, err2 := exec.Command(mutool, args...).CombinedOutput()
			if err2 != nil {
				log.Printf("serial mutool failed: %v\n%s", err2, string(out))
return renderWholePagePDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality, parallelPercent)
			}
			break // 成功了，跳出错误循环继续处理
		}
	}
	traceTiming(timing, "mutool conversion=%s all-pages=%d", time.Since(convertStart), pageCount)

	// 并行读取 PPM 并编码为最终格式（goroutine 池，默认 4 路并行）
	util.TraceProgress(progressEnabled, 0)
	type pageResult struct {
		nr  int
		err error
	}
	resultCh := make(chan pageResult, pageCount)
	sem := make(chan struct{}, util.ComputeWorkerCount(parallelPercent)) // 并发数
	var wg sync.WaitGroup

	for pageNr := 1; pageNr <= pageCount; pageNr++ {
		wg.Add(1)
		go func(nr int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pageStart := time.Now()
			ppmPath := filepath.Join(tmpDir, fmt.Sprintf("p%d.ppm", nr))
			outPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", nr, ext))

			rgba, err := util.ReadPPM(ppmPath)
			if err != nil {
				resultCh <- pageResult{nr, fmt.Errorf("read ppm page %d: %w", nr, err)}
				return
			}

			if err := util.WriteImageAtomically(outPath, func(w io.Writer) error {
				switch format {
				case "png":
					return util.EncodePNG(w, rgba)
				case "jpg", "jpeg":
					return jpeg.Encode(w, rgba, &jpeg.Options{Quality: quality})
				default:
					return fmt.Errorf("unsupported output format %q", format)
				}
			}); err != nil {
				resultCh <- pageResult{nr, fmt.Errorf("write page %d: %w", nr, err)}
				return
			}

			log.Printf("convert: page=%d/%d path=render-whole-page", nr, pageCount)
			traceImageMeta(imageMetaRecord{
				Type:   "image-meta",
				Source: "render-whole-page",
				Page:   nr,
				Index:  1,
				Ext:    ext,
				Path:   outPath,
			})
			traceTiming(timing, "page %d total=%s", nr, time.Since(pageStart))
			resultCh <- pageResult{nr, nil}
		}(pageNr)
	}

	// 等待全部完成
	wg.Wait()
	close(resultCh)

	// 收集错误（一旦有错就立即返回，但依赖 wg 已结束）
	for r := range resultCh {
		if r.err != nil {
			return r.err
		}
		util.TraceProgress(progressEnabled, r.nr*100/pageCount)
	}
	return nil
}

// renderWholePagePDFGoFitz 是 mutool 不可用时的 go-fitz 回退路径。
func renderWholePagePDFGoFitz(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int, parallelPercent int) error {
	fitzDoc, err := openFitzDoc(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render whole page: %w", err)
	}
	defer fitzDoc.Close()

	pageCount := fitzDoc.NumPage()
	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	util.TraceProgress(progressEnabled, 0)
	for pageIndex := 0; pageIndex < pageCount; pageIndex++ {
		pageNr := pageIndex + 1
		pageStart := time.Now()
		log.Printf("convert: page=%d/%d path=render-whole-page", pageNr, pageCount)
		outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, util.OutputExtension(format)))
		w, h, err := renderWholePageImageWithDoc(fitzDoc, pageNr, dpi, outputPath, format, timing, quality, pageStart)
		if err != nil {
			return err
		}
		traceImageMeta(imageMetaRecord{
			Type:   "image-meta",
			Source: "render-whole-page",
			Page:   pageNr,
			Index:  1,
			Width:  w,
			Height: h,
			Ext:    util.OutputExtension(format),
			Path:   outputPath,
		})
		traceTiming(timing, "page %d total=%s", pageNr, time.Since(pageStart))
		util.TraceProgress(progressEnabled, pageNr*100/pageCount)
	}
	return nil
}

// extractDirectImages 走的是"对象级提取"路径。
// 处理流程（逐页）：
//  1. 获取当前页的 /Resources/XObject 字典，遍历所有 Image 对象
//  2. 对每个 Image 对象调用 writeDirectImage：
//     a) 优先尝试快速路径（writeDirectImageFast）—— 直接复制 JPEG/JPEG2000 流，
//     或解码 8-bit RGB/Gray FlateDecode 图片
//     b) 快速路径不满足条件时，回退到 pdfcpu 的通用解码路径
//  3. Form XObject 中的图片也会递归提取
//  4. SMask（透明度遮罩）在快速路径内处理，不触发 pdfcpu 的锁竞争
// 适用场景：
//   - routeDirectExtractTransparency：页面有 /Group 但无裁剪路径
//   - routeDirectExtractMultiImageStack：无透明度、多图堆叠
//   - routeDirectExtractSingleObject：最简单图
// 不适用场景：有裁剪路径或复杂透明度的页面（应走 renderCropPDF）。
// 它适用于 PDF 中已经嵌入了可直接输出的图片资源：
// - JPEG / JPEG2000 之类的编码流可以直接复制；
// - 8-bit RGB / Gray 可以快速重建成 PNG；
// - 其他复杂颜色空间或位深会自动回退。
func extractDirectImages(ctx *model.Context, inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	start := time.Now()
	traceTiming(timing, "direct-extract start")
	log.Printf("convert: direct-extract pages=%d", ctx.PageCount)
	inputStem := strings.TrimSuffix(filepath.Base(inputFile), filepath.Ext(inputFile))
	// pageDigits 用来统一输出文件名的页码宽度，保证排序时自然对齐。
	pageDigits := len(strconv.Itoa(ctx.PageCount))
	totalWritten := 0
	processedPages := 0
	util.TraceProgress(progressEnabled, 0)

	// 缓存 fitz.Document，供需要整页渲染的页面复用，避免每页重复 open/close PDF。
	var wholePageDoc *fitzWrapper
	defer func() {
		if wholePageDoc != nil {
			wholePageDoc.Close()
		}
	}()

	// 这里按页处理，而不是一口气把所有页全部提上来。
	// 一方面可以在日志里清晰看到每页的耗时；另一方面，遇到单页异常时也更容易控制失败边界。
	// 逐页扫描对象号。
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageNr := pageNr
		pageStart := time.Now()
		objNrs := pdfcpu.ImageObjNrs(ctx, pageNr)
		sort.Ints(objNrs)
		if len(objNrs) == 0 {
			// 分支：当前页没有任何图片对象。
			// 这不是错误，只是说明这一页不需要导出图片，直接跳过即可。
			traceTiming(timing, "direct-extract page %d=%s images=0", pageNr, time.Since(pageStart))
			processedPages++
			util.TraceProgress(progressEnabled, processedPages*100/ctx.PageCount)
			continue
		}

		// 如果这一页本身就是整图，直接走整页渲染。
		// 这比把一个巨大的图片对象再绕一圈对象级提取更直接，
		// 也更容易保证尺寸和视觉效果一致。
		if shouldRenderWholePageImage(ctx, pageNr, objNrs) {
			log.Printf("convert: page=%d/%d path=render-whole-page", pageNr, ctx.PageCount)
			outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, util.OutputExtension(format)))
			// 延迟打开 fitz.Document，只在首次遇到需要整页渲染的页时初始化。
			// 之后的页复用同一个 doc，避免每页重复 open/close PDF。
			if wholePageDoc == nil {
				var openErr error
				wholePageDoc, openErr = openFitzDoc(inputFile)
				if openErr != nil {
					// mutool 回退：go-fitz 不可用时用 mutool draw -ppm 渲染
					img, renderErr := renderPageToImageViaMutool(inputFile, pageNr, dpi)
					if renderErr != nil {
						return fmt.Errorf("render page %d: %w (fitz: %v)", pageNr, renderErr, openErr)
					}
					bounds := img.Bounds()
					w, h := bounds.Dx(), bounds.Dy()
					if err := util.WriteImageAtomically(outputPath, func(wr io.Writer) error {
						switch format {
						case "png":
							return util.EncodePNG(wr, img)
						case "jpg", "jpeg":
							return jpeg.Encode(wr, img, &jpeg.Options{Quality: quality})
						default:
							return fmt.Errorf("unsupported output format %q", format)
						}
					}); err != nil {
						return fmt.Errorf("write rendered page %d: %w", pageNr, err)
					}
					traceImageMeta(imageMetaRecord{
						Type:   "image-meta",
						Source: "render-whole-page",
						Page:   pageNr,
						Index:  1,
						Width:  w,
						Height: h,
						Ext:    util.OutputExtension(format),
						Path:   outputPath,
					})
					totalWritten++
					traceTiming(timing, "direct-extract page %d=%s rendered-whole-page (mutool)", pageNr, time.Since(pageStart))
					processedPages++
					util.TraceProgress(progressEnabled, processedPages*100/ctx.PageCount)
					continue
				}
			}
			if _, _, err := renderWholePageImageWithDoc(wholePageDoc, pageNr, dpi, outputPath, format, timing, quality, pageStart); err != nil {
				return err
			}
			traceImageMeta(imageMetaRecord{
				Type:   "image-meta",
				Source: "render-whole-page",
				Page:   pageNr,
				Index:  1,
				Ext:    util.OutputExtension(format),
				Path:   outputPath,
			})
			totalWritten++
			traceTiming(timing, "direct-extract page %d=%s rendered-whole-page", pageNr, time.Since(pageStart))
			processedPages++
			util.TraceProgress(progressEnabled, processedPages*100/ctx.PageCount)
			continue
		}
		log.Printf("convert: page=%d/%d path=direct-extract objects=%d", pageNr, ctx.PageCount, len(objNrs))

		pageWritten := 0

		if len(objNrs) >= 4 {
			// ── 对象级并发 ─────────────────────────────────
			// 一页有 4+ 个图片对象，启动 goroutine 并行处理。
			// 所有 goroutine 共享同一个 ctx，因为快速路径只读 ctx
			// （ColorSpaceString、extractSoftMask），而唯一可能写 ctx
			// 的回退 pdfcpu.ExtractImage 已经用 ctxMu 保护。
			var objMu sync.Mutex
			var objWg sync.WaitGroup
			var ctxMu sync.Mutex
			for _, objNr := range objNrs {
				objWg.Add(1)
				go func(obj int) {
					defer objWg.Done()
					if err := writeDirectImage(ctx, &ctxMu, inputFile, pageNr, obj, inputStem, pageDigits, outputDir, format, dpi, timing, quality); err != nil {
						log.Printf("direct-extract page %d obj %d: %v", pageNr, obj, err)
					} else {
						objMu.Lock()
						pageWritten++
						objMu.Unlock()
					}
				}(objNr)
			}
			objWg.Wait()
		} else {
			// ── 对象级串行 ─────────────────────────────────
			// 页内对象少，不值得 goroutine 调度开销，串行更高效。
			for _, objNr := range objNrs {
				if err := writeDirectImage(ctx, nil, inputFile, pageNr, objNr, inputStem, pageDigits, outputDir, format, dpi, timing, quality); err != nil {
					return err
				}
				pageWritten++
			}
		}

		totalWritten += pageWritten

		traceTiming(timing, "direct-extract page %d=%s images=%d", pageNr, time.Since(pageStart), pageWritten)
		processedPages++
		util.TraceProgress(progressEnabled, processedPages*100/ctx.PageCount)
	}

	if totalWritten == 0 {
		return fmt.Errorf("no images were written")
	}
	traceTiming(timing, "direct-extract total=%s", time.Since(start))
	return nil
}

func renderWholePageImage(inputFile string, pageNr int, dpi float64, outPath, format string, timing bool, quality int) error {
	pageStart := time.Now()
	fitzDoc, err := openFitzDoc(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render: %w", err)
	}
	defer fitzDoc.Close()
	_, _, err = renderWholePageImageWithDoc(fitzDoc, pageNr, dpi, outPath, format, timing, quality, pageStart)
	return err
}

// renderWholePageImageWithDoc 用已经打开的 doc 渲染指定页并写盘，避免重复打开 PDF。
func renderWholePageImageWithDoc(doc *fitzWrapper, pageNr int, dpi float64, outPath, format string, timing bool, quality int, pageStart time.Time) (int, int, error) {
	img, err := doc.ImageDPI(pageNr-1, dpi)
	if err != nil {
		return 0, 0, fmt.Errorf("render page %d: %w", pageNr, err)
	}
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	traceTiming(timing, "render-whole-page page %d render=%s", pageNr, time.Since(pageStart))

	if err := util.WriteImageAtomically(outPath, func(w io.Writer) error {
		switch format {
		case "png":
			return util.EncodePNG(w, img)
		case "jpg", "jpeg":
			return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
		default:
			return fmt.Errorf("unsupported output format %q", format)
		}
	}); err != nil {
		return 0, 0, fmt.Errorf("encode image %s: %w", outPath, err)
	}
	traceTiming(timing, "render-whole-page page %d write=%s path=%s", pageNr, time.Since(pageStart), outPath)
	return width, height, nil
}

// renderSinglePageCropPdftoppm 使用 mutool draw -ppm 渲染单页（原始 RGB），
// 然后根据 cm 矩阵确定的裁剪区域裁剪并输出。
// 注意：函数名虽含 Pdftoppm，但实际已改为 mutool 渲染。
// 然后根据 cm 矩阵确定的裁剪区域裁剪并输出。
// 使用 mutool 替代 pdftoppm 的原因：
//   - mutool（MuPDF）的 CMYK→RGB 转换与 PDF 阅读器一致，无偏红问题
//   - mutool -ppm 渲染速度与 pdftoppm -jpeg 相同
//   - PPM 是原始 RGB 数据，无需解码，裁剪和编码更快
// 输入参数在 PDF 用户空间坐标中（bottom-left origin）：
//	cropX, cropY: 裁剪区域左下角坐标（PDF 点）
//	cropW, cropH: 裁剪区域宽度和高度（PDF 点）*/
func renderSinglePageCropPdftoppm(inputFile string, pageNr int, dpi float64, outPath, format string, quality int, cropX, cropY, cropW, cropH float64) error {
	// mutool draw -ppm 渲染单页
	tmpDir, err := os.MkdirTemp("", "mutool-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	prefix := filepath.Join(tmpDir, "p")
	args := []string{
		"draw", "-q",
		"-r", fmt.Sprintf("%.0f", dpi),
		"-o", prefix + "%d.ppm",
		inputFile,
		strconv.Itoa(pageNr),
	}
	out, err := exec.Command(util.FindMutool(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mutool render page %d: %v\n%s", pageNr, err, string(out))
	}

	// 读 PPM 获取渲染尺寸和 RGB 数据
	ppmPath := filepath.Join(tmpDir, fmt.Sprintf("p%d.ppm", pageNr))
	rgba, err := util.ReadPPM(ppmPath)
	if err != nil {
		return fmt.Errorf("read ppm page %d: %w", pageNr, err)
	}

	imgW, imgH := rgba.Bounds().Dx(), rgba.Bounds().Dy()

	// 计算裁剪区域在渲染图像中的像素位置
	scaleX := float64(imgW) / (cropW * dpi / 72.0)
	scaleY := float64(imgH) / (cropH * dpi / 72.0)

	cropXPx := int(cropX * dpi / 72.0 * scaleX)
	cropYPx := int(cropY * dpi / 72.0 * scaleY)
	cropWPx := int(cropW * dpi / 72.0 * scaleX)
	cropHPx := int(cropH * dpi / 72.0 * scaleY)

	if cropXPx < 0 {
		cropXPx = 0
	}
	if cropYPx < 0 {
		cropYPx = 0
	}
	if cropXPx+cropWPx > imgW {
		cropWPx = imgW - cropXPx
	}
	if cropYPx+cropHPx > imgH {
		cropHPx = imgH - cropYPx
	}
	if cropWPx <= 0 || cropHPx <= 0 {
		cropXPx, cropYPx = 0, 0
		cropWPx, cropHPx = imgW, imgH
	}

	cropRect := image.Rect(cropXPx, cropYPx, cropXPx+cropWPx, cropYPx+cropHPx)
	cropped := util.CropImage(rgba, cropRect)

	return util.WriteImageAtomically(outPath, func(w io.Writer) error {
		switch format {
		case "png":
			return util.EncodePNG(w, cropped)
		case "jpg", "jpeg":
			return jpeg.Encode(w, cropped, &jpeg.Options{Quality: quality})
		default:
			return fmt.Errorf("unsupported output format %q", format)
		}
	})
}

// renderPageToImageViaMutool 使用 mutool draw -ppm 渲染 PDF 指定页并返回 image.Image。
// 当 go-fitz 不可用时作为 renderCropPDF 的回退渲染路径。
func renderPageToImageViaMutool(inputFile string, pageNr int, dpi float64) (image.Image, error) {
	tmpDir, err := os.MkdirTemp("", "mutool-page-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	prefix := filepath.Join(tmpDir, "p")
	args := []string{
		"draw", "-q",
		"-r", fmt.Sprintf("%.0f", dpi),
		"-o", prefix + "%d.ppm",
		inputFile,
		strconv.Itoa(pageNr),
	}
	out, err := exec.Command(util.FindMutool(), args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("mutool draw page %d: %v\n%s", pageNr, err, string(out))
	}

	ppmPath := filepath.Join(tmpDir, fmt.Sprintf("p%d.ppm", pageNr))
	img, err := util.ReadPPM(ppmPath)
	if err != nil {
		return nil, fmt.Errorf("read ppm page %d: %w", pageNr, err)
	}
	return img, nil
}

// renderSinglePageCrop 渲染 PDF 单页后裁剪到最大前景区域。
// 用于 CMYK JPEG 等无法绕过 MuPDF 色彩管线的场景：
// 1. 用 go-fitz 渲染整页（确保颜色正确）
// 2. 连通域分析（flood fill）找到图片主体边界
// 3. 裁剪掉白边，输出仅图片区域
// renderSinglePageCrop 渲染 PDF 单页后裁剪到最大前景区域。
// 用于 CMYK JPEG 等无法绕过 MuPDF 色彩管线的场景：
// 1. 用 go-fitz 渲染整页（确保颜色正确）
// 2. 连通域分析（flood fill）找到图片主体边界
// 3. 裁剪掉白边，输出仅图片区域
func renderSinglePageCrop(inputFile string, pageNr int, dpi float64, outPath, format string, timing bool, quality int) error {
	pageStart := time.Now()
	fitzDoc, err := openFitzDoc(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render+crop: %w", err)
	}
	defer fitzDoc.Close()

	renderStart := time.Now()
	img, err := fitzDoc.ImageDPI(pageNr-1, dpi)
	if err != nil {
		return fmt.Errorf("render page %d: %w", pageNr, err)
	}
	traceTiming(timing, "render-crop page %d render=%s", pageNr, time.Since(renderStart))

	// 连通域分析，找最大的前景区域（图片主体）
	analyzeStart := time.Now()
	crops, err := findLargestRegions(img, 1)
	if err != nil {
		return fmt.Errorf("analyze page %d: %w", pageNr, err)
	}
	if len(crops) == 0 {
		// 如果没找到前景区域，回退到全页输出，总比不输出好。
		log.Printf("render-crop page %d: no foreground found, falling back to whole page", pageNr)
		return util.WriteImageAtomically(outPath, func(w io.Writer) error {
			switch format {
			case "png":
				return util.EncodePNG(w, img)
			case "jpg", "jpeg":
				return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
			default:
				return fmt.Errorf("unsupported output format %q", format)
			}
		})
	}
	traceTiming(timing, "render-crop page %d analyze=%s", pageNr, time.Since(analyzeStart))

	// crops[0] 是面积最大的区域，即图片主体
	// 裁掉边框线条（PDF 描边≈1pt，按 DPI 折算为像素再加余量）
	borderPx := int(dpi/72.0 + 2) // 1pt ≈ DPI/72 px，多加 2px 余量
	if borderPx < 1 {
		borderPx = 1
	}
	cropRect := crops[0].Inset(borderPx)
	traceTiming(timing, "render-crop page %d rect=%dx%d at (%d,%d)", pageNr,
		cropRect.Dx(), cropRect.Dy(), cropRect.Min.X, cropRect.Min.Y)

	cropStart := time.Now()
	cropped := util.CropImage(img, cropRect)
	traceTiming(timing, "render-crop page %d crop=%s", pageNr, time.Since(cropStart))

	if err := util.WriteImageAtomically(outPath, func(w io.Writer) error {
		switch format {
		case "png":
			return util.EncodePNG(w, cropped)
		case "jpg", "jpeg":
			return jpeg.Encode(w, cropped, &jpeg.Options{Quality: quality})
		default:
			return fmt.Errorf("unsupported output format %q", format)
		}
	}); err != nil {
		return fmt.Errorf("save cropped page %d: %w", pageNr, err)
	}
	traceTiming(timing, "render-crop page %d total=%s", pageNr, time.Since(pageStart))
	return nil
}

// writeDirectImage 负责把单个图片对象落盘。
// 它先尝试快速路径；若快速路径条件不成立，再退回到 pdfcpu 的通用解码。
// ctxMu 是可选互斥锁：
//   - 非 nil 时，pdfcpu.ExtractImage 回退调用会在锁内执行（供并发共享 ctx 时用）
//   - nil 时不做锁保护（串行调用，无竞争）
func writeDirectImage(ctx *model.Context, ctxMu *sync.Mutex, inputFile string, pageNr, objNr int, inputStem string, pageDigits int, outputDir, format string, dpi float64, timing bool, quality int) error {
	objStart := time.Now()
	imageObj := ctx.Optimize.ImageObjects[objNr]
	if imageObj == nil || imageObj.ImageDict == nil {
		// 这个对象不是完整图片，直接跳过。
		// 例如它可能只是一个引用节点、占位对象，或者缺少真正的图像字典。
		return nil
	}

	resourceID := ""
	if pageNr-1 < len(imageObj.ResourceNames) {
		resourceID = imageObj.ResourceNames[pageNr-1]
	}

	// 先试快路径，是因为有些图片本来就是可直接复制的编码流。
	// 如果快路径命中，就能避免 pdfcpu 的通用解码，速度会快很多。
	// 快速路径只读 ctx（ColorSpaceString、extractSoftMask），不需要锁。
	if ok, err := writeDirectImageFast(ctx, imageObj.ImageDict, objNr, inputStem, pageDigits, pageNr, resourceID, outputDir, objStart, timing, dpi, inputFile, format, quality); ok {
		return err
	}

	// 快路径没有命中，说明这个对象更复杂：
	// 可能是 JPX、可能是带软遮罩的 RGB，也可能是色彩空间太复杂，
	// 只能交给 pdfcpu 的通用提取逻辑兜底。
	// pdfcpu.ExtractImage 可能修改 ctx 内部状态，在并发场景下用锁保护。
	decodeStart := time.Now()
	var img *model.Image
	var err error
	if ctxMu != nil {
		ctxMu.Lock()
		img, err = pdfcpu.ExtractImage(ctx, imageObj.ImageDict, false, resourceID, objNr, false)
		ctxMu.Unlock()
	} else {
		img, err = pdfcpu.ExtractImage(ctx, imageObj.ImageDict, false, resourceID, objNr, false)
	}
	if err != nil {
		// 这里直接跳过，是因为这个对象本身就不可提取，或者 pdfcpu 无法识别。
		// 对整页而言，这不一定是致命错误，所以先记时并继续下一个对象。
		traceTiming(timing, "direct-extract page %d obj %d extract-skip=%v", pageNr, objNr, err)
		return nil
	}
	traceTiming(timing, "direct-extract page %d obj %d fallback-extract=%s", pageNr, objNr, time.Since(decodeStart))
	if img == nil || img.Reader == nil || img.Thumb {
		// 没有可写的 reader，或者只是 thumbnail，就没有必要继续落盘。
		return nil
	}
	if strings.EqualFold(img.FileType, "jpx") {
		// JPX 是特殊分支：Go 标准库不直接负责它的最终转码，
		// 所以这里走系统转换器，把它统一变成用户指定的 png/jpg。
		qual := img.Name
		if qual == "" {
			qual = "image"
		}
		outputExt := util.OutputExtension(format)
		fileName := fmt.Sprintf("%s_%0*d_%s_obj%d.%s", inputStem, pageDigits, pageNr, qual, objNr, outputExt)
		outPath := filepath.Join(outputDir, fileName)
		metaWidth, metaHeight := resolveImageDimensions(imageObj.ImageDict, img.Width, img.Height)
		traceImageMeta(imageMetaRecord{
			Type:   "image-meta",
			Source: "direct-fallback",
			Page:   pageNr,
			Object: objNr,
			Width:  metaWidth,
			Height: metaHeight,
			Ext:    outputExt,
			Time:   time.Since(objStart).String(),
			Path:   outPath,
		})
		writeStart := time.Now()
		if err := util.ConvertJPXToOutput(img.Reader, outPath, format, quality); err != nil {
			return fmt.Errorf("convert jpx page %d obj %d: %w", pageNr, objNr, err)
		}
		traceTiming(timing, "direct-extract page %d obj %d jpx-write=%s", pageNr, objNr, time.Since(writeStart))
		return nil
	}

	qual := img.Name
	// 如果所有候选都失败，说明当前环境既没有可用命令，或者命令存在但不支持这个样本。
	// 这时不再继续兜底成整页渲染，因为用户要求的是“单图 + png/jpg”，
	// 直接报错比悄悄产出错误结果更可控。
	if qual == "" {
		qual = "image"
	}
	outputExt := normalizeOutputImageExt(img.FileType)
	fileName := fmt.Sprintf("%s_%0*d_%s_obj%d.%s", inputStem, pageDigits, pageNr, qual, objNr, outputExt)
	outPath := filepath.Join(outputDir, fileName)
	metaWidth, metaHeight := resolveImageDimensions(imageObj.ImageDict, img.Width, img.Height)
	traceImageMeta(imageMetaRecord{
		Type:   "image-meta",
		Source: "direct-fallback",
		Page:   pageNr,
		Object: objNr,
		Width:  metaWidth,
		Height: metaHeight,
		Ext:    outputExt,
		Time:   time.Since(objStart).String(),
		Path:   outPath,
	})

	writeStart := time.Now()
	if outputExt == "png" && !isDirectCopyableImageType(img.FileType) {
		decodedImg, decodeErr := decodeImageForOutput(img.FileType, img.Reader)
		if decodeErr != nil {
			traceTiming(timing, "direct-extract page %d obj %d decode-skip=%v", pageNr, objNr, decodeErr)
			return nil
		}
		if err := util.WriteImageAtomically(outPath, func(w io.Writer) error {
			return util.EncodePNG(w, decodedImg)
		}); err != nil {
			traceTiming(timing, "direct-extract page %d obj %d encode-skip=%v", pageNr, objNr, err)
			return nil
		}
	} else {
		if err := util.WriteImageAtomically(outPath, func(w io.Writer) error {
			_, copyErr := io.Copy(w, img.Reader)
			return copyErr
		}); err != nil {
			traceTiming(timing, "direct-extract page %d obj %d write-skip=%v", pageNr, objNr, err)
			return nil
		}
	}
	traceTiming(timing, "direct-extract page %d obj %d fallback-write=%s", pageNr, objNr, time.Since(writeStart))
	return nil
}

// writeDirectImageFast 尝试绕过通用图像解码，直接把图片流写出来。
// 返回值中的 bool 表示“是否真的走了快速写出路径”。
// false != error：
// - false, nil 代表条件不满足，应该回退；
// - false, err 代表快速路径在处理中出错；
// - true, nil 代表已经成功写盘。
func writeDirectImageFast(ctx *model.Context, sd *types.StreamDict, objNr int, inputStem string, pageDigits, pageNr int, resourceID, outputDir string, startedAt time.Time, timing bool, dpi float64, inputFile string, format string, quality int) (bool, error) {
	if sd == nil {
		// 分支：没有 stream dict，说明这个对象根本不符合图片流处理条件。
		return false, nil
	}

	w := sd.IntEntry("Width")
	h := sd.IntEntry("Height")

	// 先判断是否是已经编码好的图片流，尽量避免重新解码和重建像素。
	// 这一步的目标是尽量把“复制字节”替换掉“重建图像”。
	lastFilter := ""
	if len(sd.FilterPipeline) > 0 {
		lastFilter = sd.FilterPipeline[len(sd.FilterPipeline)-1].Name
	}
	if lastFilter == filter.JPX {
		return false, nil
	}

	if lastFilter == filter.DCT {
		decodeStart := time.Now()
		if err := sd.Decode(); err != nil {
			return false, fmt.Errorf("decode page %d obj %d: %w", pageNr, objNr, err)
		}
		traceTiming(timing, "direct-extract page %d obj %d raw-decode=%s", pageNr, objNr, time.Since(decodeStart))

		// 用 isCMYKJPEG 解析 JPEG 字节流里的 SOF marker 来判断是否 4 分量（CMYK）。
		// sd.CSComponents 不可靠，直接读字节最准确。
		// 只输出 RGB/DCT、YCC/DCT、Gray/DCT 图片的原始字节。
		if util.IsCMYKJPEG(sd.Content) {
			// 分支：CMYK / Adobe YCCK JPEG。
			// 这类 JPEG 如果当普通 jpg 写出，大多数看图器会误按 YCbCr 解释，
			// 导致颜色完全偏色。常见库（sips、ImageMagick、Pillow）都不正确处理
			// Adobe APP14 transform=2 (YCCK) 标记，输出超级暗。
			// 优化策略（分两层）：
			//   第一层（mutool 快速裁剪）：
			//     从 content stream 解析 cm 矩阵获得图片在页面上的精确位置，
			//     用 mutool draw -ppm 渲染整页后裁剪该区域，跳过 flood fill。
			//     适用于简单单图片页面（无旋转、无透明度组、无 SMask）。
			//     颜色与 PDF 阅读器 100% 一致（vs pdftoppm 偏红）。
			//   第二层（go-fitz + flood fill，回退）：
			//     用 go-fitz 渲染整页后 flood fill 分析找图片区域再裁剪。
			//     MuPDF 正确处理 PDF 所有颜色空间和 Overprint Mode (OPM=1)。
			//     适用于有透明度组合、旋转、多图等复杂页面。
			log.Printf("direct-extract page=%d obj=%d cmyk-jpeg detected", pageNr, objNr)

			// 先按用户指定格式计算路径，mutool 路径成功时再改为 jpg
			outputExt := util.OutputExtension(format)
			outPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, outputExt))
			actualFormat := format // 实际输出格式，mutool 路径会改写为 jpg
			actualExt := outputExt

			writeStart := time.Now()

			// 第一层：尝试 mutool 快速裁剪路径
			// 条件：mutool 可用、图片非旋转（b=0, c=0）、有效尺寸
			// 自动改写输出格式为 jpg（源图是 CMYK JPEG，无需重编码为 PNG）
			mutoolOk := false
			if util.FindMutool() != "" {
				pageDict, _, _, pErr := ctx.PageDict(pageNr, false)
				if pErr == nil {
					content := util.GetPageContentString(ctx, pageDict)
					if content != "" {
						a, b, c, d, e, f, cmOk := util.ExtractImageFullCM(content)
						if cmOk && b == 0 && c == 0 && a > 0 && d > 0 {
							// 非旋转、有效尺寸 → 自动用 jpg 输出（避免 PNG 重编码）
							actualFormat = "jpg"
							actualExt = "jpg"
							outPath = filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, actualExt))
							if err := renderSinglePageCropPdftoppm(inputFile, pageNr, dpi, outPath, actualFormat, quality, e, f, a, d); err == nil {
								mutoolOk = true
							} else {
								log.Printf("mutool crop failed for page %d: %v", pageNr, err)
							}
						}
					}
				}
			}

			if !mutoolOk {
				// 第二层：回退到 go-fitz + flood fill（用用户指定的格式）
				actualFormat = format
				actualExt = outputExt
			}

			// 写入 imageMeta（使用实际输出的格式和路径）
			traceImageMeta(imageMetaRecord{
				Type:   "image-meta",
				Source: "direct-fast",
				Page:   pageNr,
				Object: objNr,
				Width:  *w,
				Height: *h,
				Ext:    actualExt,
				Time:   time.Since(startedAt).String(),
				Path:   outPath,
			})

			if !mutoolOk {
				if err := renderSinglePageCrop(inputFile, pageNr, dpi, outPath, actualFormat, timing, quality); err != nil {
					return false, fmt.Errorf("render+crop cmyk page %d obj %d: %w", pageNr, objNr, err)
				}
			}

			traceTiming(timing, "direct-extract page %d obj %d cmyk-render=%s", pageNr, objNr, time.Since(writeStart))
			return true, nil
		}

		ext := "jpg"
		if resourceID == "" {
			resourceID = "image"
		}
		fileName := fmt.Sprintf("%s_%0*d_%s_obj%d.%s", inputStem, pageDigits, pageNr, resourceID, objNr, ext)
		outPath := filepath.Join(outputDir, fileName)
		traceImageMeta(imageMetaRecord{
			Type:   "image-meta",
			Source: "direct-fast",
			Page:   pageNr,
			Object: objNr,
			Width:  *w,
			Height: *h,
			Ext:    ext,
			Time:   time.Since(startedAt).String(),
			Path:   outPath,
		})
		writeStart := time.Now()
		if err := os.WriteFile(outPath, sd.Content, 0644); err != nil {
			return false, fmt.Errorf("write image %s: %w", outPath, err)
		}
		traceTiming(timing, "direct-extract page %d obj %d raw-write=%s", pageNr, objNr, time.Since(writeStart))
		return true, nil
	}

	// ── 8-bit RGB / Gray 快速路径 ───────────────────────────────
	// 只处理 8-bit RGB / Gray，保证我们自己拼像素时数据布局是确定的。
	// 只要条件不满足，就宁可回退，不做"猜测式输出"。
	// 注意：这里处理的是所有非 DCT/JPX 的 8-bit 图片流——
	// DCT 分支（上面的第 2 个分支）只匹配 FilterPipeline 末尾是
	// DCTDecode 的图像，解码后直接写 JPEG 字节到 .jpg 文件。
	// FlateDecode、RunLengthDecode、CCITTFaxDecode 等非 DCT 过滤器
	// 都不会进入那个分支，而是落到这里。
	// 最初以为这些 FlateDecode 图像是 ColorSpaceString 返回了
	// ICCBased 等非 DeviceRGB 值才被拒绝，Debug 后发现实际上
	// ColorSpaceString 正确返回了 "DeviceRGB"，条件全部通过。
	// 真正的原因是 sd.Decode() 从未被调用（只在 DCT 分支内调用），
	// 导致 sd.Content 为空，到 b := sd.Content 时拿到 nil，
	// 随后的 len(b) < 3*w*h 检查返回 "corrupt" 错误，回退到
	// pdfcpu.ExtractImage（受 ctxMu 互斥锁串行化）。
	// 修复方法：在拼像素前主动 sd.Decode()。
	// sd.Decode() 只读自己的 sd.Raw、写自己的 sd.Content，
	// 不碰共享的 ctx。每个 obj 的 sd 都是 indRefToStreamDict 独立
	// 解析出来的，goroutine 间零共享，完全安全并行。

	csStart := time.Now()
	cs, err := pdfcpu.ColorSpaceString(ctx, sd)
	if err != nil {
		return false, fmt.Errorf("colorspace page %d obj %d: %w", pageNr, objNr, err)
	}
	traceTiming(timing, "direct-extract page %d obj %d colorspace=%s", pageNr, objNr, time.Since(csStart))

	bpc := 0
	if v := sd.IntEntry("BitsPerComponent"); v != nil {
		bpc = *v
	}

	if bpc != 8 || (cs != model.DeviceRGBCS && cs != model.DeviceGrayCS) {
		// 分支：图像不满足快速重建条件。
		// 常见原因包括：CMYK、索引色、16-bit、或者其他复杂色彩空间。
		// 颜色空间或位深不符合当前快速重建条件，回退到通用路径。
		// 这通常是 CMYK、索引色、位深不是 8 的 PDF。
		return false, nil
	}

	if w == nil || h == nil {
		return false, nil
	}

	softMaskStart := time.Now()
	softMask, err := util.ExtractSoftMask(ctx, sd, objNr, *w, *h, timing, pageNr)
	if err != nil {
		return false, err
	}
	traceTiming(timing, "direct-extract page %d obj %d softmask=%s present=%v", pageNr, objNr, time.Since(softMaskStart), softMask != nil)

	if softMask == nil {
		// 分支：主图像声明了软遮罩，但我们没有拿到可用的 SMask 内容。
		// 这时继续写图会造成透明度缺失，所以直接回退。
		// 如果存在 SMask 但未能成功解出，就不强行继续，避免透明信息丢失。
		// 这类图片如果硬写出来，视觉结果可能比回退路径更差。
		if o, _ := sd.Find("SMask"); o != nil {
			return false, nil
		}
	}

	if resourceID == "" {
		resourceID = "image"
	}
	fileName := fmt.Sprintf("%s_%0*d_%s_obj%d.png", inputStem, pageDigits, pageNr, resourceID, objNr)
	outPath := filepath.Join(outputDir, fileName)
	traceImageMeta(imageMetaRecord{
		Type:   "image-meta",
		Source: "direct-fast",
		Page:   pageNr,
		Object: objNr,
		Width:  *w,
		Height: *h,
		Ext:    "png",
		Time:   time.Since(startedAt).String(),
		Path:   outPath,
	})
	img := image.NewNRGBA(image.Rect(0, 0, *w, *h))

	// ── 延迟解码 FlateDecode 等非 DCT 流 ────────────────────────
	// sd.Content 为空，说明 pdfcpu 没有对这个流调用 Decode()。
	// DCT 分支在过滤器匹配时已调用 sd.Decode()，但 FlateDecode、
	// RunLengthDecode、CCITTFaxDecode 等非 DCT 过滤器不会走那条路。
	// 安全分析：
	// sd.Decode() 的操作范围仅限于 sd 自身——读取 sd.Raw 中的压缩
	// 字节，按 sd.FilterPipeline 逐级解码，写入 sd.Content。
	// 不涉及共享的 ctx（不读页对象树，不遍历 XObject，不修改任何
	// 外部状态）。每个 sd 是由 extractDirectImages 中 indRefToStreamDict
	// 独立解析出来的，goroutine 间不共享。
	// 这与旧的 go-fitz (CGo) 完全不同：go-fitz 的 fitz.New() 会触发
	// macOS 信号栈溢出（semasleep on Darwin signal stack）导致进程
	// 无响应卡死。这里全是纯 Go：
	//   1. sd.Decode()          → pdfcpu 纯 Go zlib 解压
	//   2. SetNRGBA 像素合成   → image/color 纯 Go
	//   3. PNG 编码             → image/png 纯 Go
	// 零 CGo、零 ctxMu、零共享，goroutine 间完全安全并行。
	// 实测效果：2.pdf（20 张 FlateDecode + SMask 大图）
	//   优化前：6.05s（全量 pdfcpu.ExtractImage，ctxMu 串行化）
	//   优化后：0.28s（20 goroutine 全并行，无锁竞争）
	if len(sd.Content) == 0 && len(sd.Raw) > 0 {
		if decodeErr := sd.Decode(); decodeErr != nil {
			return false, fmt.Errorf("fast-decode page %d obj %d: %w", pageNr, objNr, decodeErr)
		}
	}

	b := sd.Content

	paintStart := time.Now()
	if cs == model.DeviceGrayCS {
		// 分支：灰度图。
		// 这一支只需要一个灰度通道，所以像素重建逻辑相对简单。
		// 灰度图逐像素扩展为 RGBA，透明度来自 soft mask（如果有）。
		// 这样输出的是标准 PNG，而不是依赖外部解释器的灰度流。
		if len(b) < (*w * *h) {
			return false, fmt.Errorf("gray image obj %d corrupt", objNr)
		}
		i := 0
		for y := 0; y < *h; y++ {
			for x := 0; x < *w; x++ {
				alpha := uint8(255)
				if softMask != nil {
					alpha = softMask[y**w+x]
				}
				v := b[i]
				img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: alpha})
				i++
			}
		}
	} else {
		// 分支：RGB 图。
		// 这意味着我们要按 3 字节一组重建像素，并保持原始通道顺序。
		// RGB 图同样逐像素写入，保证输出文件与原始像素顺序一致。
		// 这里不做额外颜色变换，默认保留原始 RGB 顺序。
		if len(b) < 3*(*w**h) {
			return false, fmt.Errorf("rgb image obj %d corrupt", objNr)
		}
		i := 0
		for y := 0; y < *h; y++ {
			for x := 0; x < *w; x++ {
				alpha := uint8(255)
				if softMask != nil {
					alpha = softMask[y**w+x]
				}
				img.SetNRGBA(x, y, color.NRGBA{R: b[i], G: b[i+1], B: b[i+2], A: alpha})
				i += 3
			}
		}
	}
	traceTiming(timing, "direct-extract page %d obj %d paint=%s", pageNr, objNr, time.Since(paintStart))

	encodeStart := time.Now()
	if err := util.WriteImageAtomically(outPath, func(w io.Writer) error {
		enc := png.Encoder{CompressionLevel: png.NoCompression}
		return enc.Encode(w, img)
	}); err != nil {
		return false, fmt.Errorf("encode image %s: %w", outPath, err)
	}
	traceTiming(timing, "direct-extract page %d obj %d encode=%s", pageNr, objNr, time.Since(encodeStart))
	return true, nil
}

// findLargestRegions 从整页渲染结果里提取最大的前景区域。

type region struct {
	rect image.Rectangle
	area int
}

// findLargestRegions 从整页渲染结果里提取最大的前景区域。
// 它的假设很简单：
// - 页面大部分背景接近白色；
// - 目标内容在像素上是连通的或者近似连通的；
// - 前景面积越大，越可能是我们想要的图像主体。
// findLargestRegions 从整页渲染结果里提取最大的前景区域。
// 算法：
//  1. 将图像转换为 *image.RGBA
//  2. 从图像的四个角开始扫描（因为图片主体通常在页面中央，四角是背景）
//  3. 对每个非背景像素启动 flood fill，收集连通区域
//  4. 按像素面积降序排列，取前 maxRegions 个
//  5. 返回这些区域的外接矩形列表
// 假设前提：
//   - 大部分背景接近白色
//   - 目标内容在像素上是连通的或者近似连通的
//   - 前景面积越大，越可能是图像主体
// 阈值偏高，更愿意把浅灰边缘也算作背景，使主体外接框更稳定。
func findLargestRegions(img image.Image, maxRegions int) ([]image.Rectangle, error) {
	// 把渲染结果转成 RGBA 后，按背景阈值做 flood fill，提取最大的前景区域。
	rgba := util.ToRGBA(img)
	bounds := rgba.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width == 0 || height == 0 {
		return nil, fmt.Errorf("empty rendered page")
	}

	visited := make([]bool, width*height)
	regions := make([]region, 0, 16)

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			idx := (y-bounds.Min.Y)*width + (x - bounds.Min.X)
			if visited[idx] {
				continue
			}
			if util.IsBackground(rgba.RGBAAt(x, y)) {
				visited[idx] = true
				continue
			}

			rect, area := floodFillRegion(rgba, x, y, visited)
			if area >= util.MinRegionAreaThreshold(width, height) {
				regions = append(regions, region{rect: rect, area: area})
			}
		}
	}

	sort.Slice(regions, func(i, j int) bool {
		if regions[i].area == regions[j].area {
			if regions[i].rect.Min.Y == regions[j].rect.Min.Y {
				return regions[i].rect.Min.X < regions[j].rect.Min.X
			}
			return regions[i].rect.Min.Y < regions[j].rect.Min.Y
		}
		return regions[i].area > regions[j].area
	})

	if len(regions) > maxRegions {
		// 分支：前景连通块过多。
		// 这里只保留面积最大的几个，过滤掉页边噪点、小装饰和碎片区域。
		regions = regions[:maxRegions]
	}

	result := make([]image.Rectangle, 0, len(regions))
	for _, region := range regions {
		result = append(result, region.rect)
	}
	return result, nil
}

// shouldRenderWholePageImage 只在“单图且图片尺寸与页面尺寸一致”时返回 true。
// 这样可以把真正的整页扫描图交给渲染路径，而把普通插图继续走直取路径。
// shouldRenderWholePageImage 判断指定页的图片是否应该整页渲染输出。
// 条件：
//   - 该页只有 1 个图片对象
//   - 图片尺寸与页面尺寸接近（差值在 5% 内）
// 满足条件时，直接渲染整页并输出，不做裁剪。
// 适用于扫描版 PDF、单张贴图、背景图等场景。
func shouldRenderWholePageImage(ctx *model.Context, pageNr int, objNrs []int) bool {
	if len(objNrs) != 1 {
		return false
	}

	imageObj := ctx.Optimize.ImageObjects[objNrs[0]]
	if imageObj == nil || imageObj.ImageDict == nil {
		return false
	}

	pageDict, _, _, err := ctx.PageDict(pageNr, false)
	if err != nil || pageDict == nil {
		return false
	}

	pageBox := pageDict.ArrayEntry("CropBox")
	if len(pageBox) == 0 {
		pageBox = pageDict.ArrayEntry("MediaBox")
	}
	pageRect := types.RectForArray(pageBox)
	if pageRect == nil {
		return false
	}

	imageWidth := imageObj.ImageDict.IntEntry("Width")
	imageHeight := imageObj.ImageDict.IntEntry("Height")
	if imageWidth == nil || imageHeight == nil || *imageWidth <= 0 || *imageHeight <= 0 {
		return false
	}

	pageWidth := pageRect.Width()
	pageHeight := pageRect.Height()
	imageW := float64(*imageWidth)
	imageH := float64(*imageHeight)

	return (nearlyEqual(imageW, pageWidth) && nearlyEqual(imageH, pageHeight)) ||
		(nearlyEqual(imageW, pageHeight) && nearlyEqual(imageH, pageWidth))
}

// nearlyEqual 判断两个浮点数是否在 5% 的容差范围内相等。
// 用于比较图片尺寸与页面尺寸的接近程度。
func nearlyEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= 1
}

// floodFillRegion 使用四邻域 flood fill 扫描一个连通块。
// 它返回两个信息：
// - 这个连通块的外接矩形；
// - 这个连通块的像素面积。
// 后续会按面积排序，保留最大的几个区域。
// floodFillRegion 使用四邻域 flood fill 扫描一个连通块。
// 从 (startX, startY) 开始，沿上下左右方向扩散，
// 将所有非背景像素标记为已访问。
// 返回：
//   - 连通块的外接矩形（image.Rectangle）
//   - 连通块的像素面积
// 后续由 findLargestRegions 按面积排序，保留最大区域。
func floodFillRegion(img *image.RGBA, startX, startY int, visited []bool) (image.Rectangle, int) {
	// 标准四邻域 flood fill，记录连通区域的外接矩形和面积。
	bounds := img.Bounds()
	width := bounds.Dx()
	queue := []image.Point{{X: startX, Y: startY}}
	visited[(startY-bounds.Min.Y)*width+(startX-bounds.Min.X)] = true

	minX, maxX := startX, startX
	minY, maxY := startY, startY
	area := 0

	for len(queue) > 0 {
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		area++

		if p.X < minX {
			minX = p.X
		}
		if p.X > maxX {
			maxX = p.X
		}
		if p.Y < minY {
			minY = p.Y
		}
		if p.Y > maxY {
			maxY = p.Y
		}

		neighbors := []image.Point{
			{X: p.X - 1, Y: p.Y},
			{X: p.X + 1, Y: p.Y},
			{X: p.X, Y: p.Y - 1},
			{X: p.X, Y: p.Y + 1},
		}

		for _, n := range neighbors {
			if n.X < bounds.Min.X || n.X >= bounds.Max.X || n.Y < bounds.Min.Y || n.Y >= bounds.Max.Y {
				// 分支：邻居越界，直接忽略。
				continue
			}
			idx := (n.Y-bounds.Min.Y)*width + (n.X - bounds.Min.X)
			if visited[idx] {
				// 分支：这个像素已经处理过了，避免重复扩展。
				continue
			}
			if util.IsBackground(img.RGBAAt(n.X, n.Y)) {
				// 分支：邻居仍然是背景像素。
				// 只标记，不入队，这样 flood fill 会自然避开白边和留白。
				visited[idx] = true
				continue
			}
			// 分支：邻居属于前景，把它加入连通块继续扩展。
			visited[idx] = true
			queue = append(queue, n)
		}
	}

	return image.Rect(minX, minY, maxX+1, maxY+1), area
}

// cropImage 把指定矩形从原图中裁出来，并统一输出为 RGBA。
// 这样后面的编码逻辑只需要处理一种图像类型，减少分支。
// 3x4 色彩校正矩阵：将 pdftoppm 的 RGB 输出校正至与 MuPDF（mutool）一致。
// pdftoppm（lcms2）与 MuPDF 使用不同的默认 CMYK→RGB 转换算法，
// 导致 pdftoppm 渲染的 CMYK PDF 页面偏红（G 通道偏低 ~10，B 通道偏低 ~6）。
// 该矩阵通过最小二乘法拟合 12.pdf 的 20,000 个采样像素得到，
// 将平均颜色差异从 8.72 降至 3.47/通道（降幅 60%）。
// 适用场景：pdftoppm 渲染的 DeviceCMYK 或隐式 CMYK 页面。
// RGB-only PDF 页面不受 pdftoppm 色彩转换影响，此校正不会造成额外误差。
// 矩阵格式（行优先）：[R, G, B, bias] 即 R_out = m[0][0]*R + m[0][1]*G + m[0][2]*B + m[0][3]
// ColorCorrectionMatrix 在 util 包中定义

// applyColorCorrection 对 RGBA 图像的每个像素应用色彩校正矩阵。
// 直接在 RGBA 像素数据上原地修改，避免额外内存分配。
// applyColorCorrection 对 RGBA 图像的每个像素应用色彩校正矩阵。
// 直接在 RGBA 像素数据上原地修改，避免额外内存分配。
// 矩阵通过最小二乘法拟合 12.pdf 的 20,000 个采样像素得到。
// 目前默认关闭（-cc），因为渲染引擎已改为 mutool，颜色正确无需校正。

// findMutool 查找 mutool 可执行文件路径，优先使用捆绑版本。
// 查找顺序：
//  1. PATH 环境变量（系统安装的 mutool）
//  2. 程序同级目录 mutool（解压后直接放在一起）
//  3. 程序同级 bund/<os>-<arch>/mutool（跨平台捆绑）
//  4. 程序同级 bund/mutool（简单捆绑）
//  5. /opt/homebrew/bin/mutool（Homebrew）
//  6. /usr/local/bin/mutool
// 如果都找不到，返回空字符串，后续会回退到 go-fitz 渲染。
// 结果缓存在全局变量 mutoolPath 中，避免重复查找。

// findGS 查找 Ghostscript (gs) 可执行文件路径，优先使用捆绑版本。
// 查找顺序：
//  1. PATH 环境变量（系统安装的 gs）
//  2. 程序同级目录 gs（dist 打包后 gs 和 pdf-tool 放在一起）
//  3. 程序同级 bund/<os>-<arch>/gs（跨平台捆绑）
//  4. 程序同级 bund/gs（简单捆绑）
//  5. /opt/homebrew/bin/gs（Homebrew）
//  6. /usr/local/bin/gs
// 结果缓存在全局变量中，避免重复查找。


// getPageCount 获取 PDF 页数，多源回退保证不因为单一工具不可用而失败。
// 优先级：mutool info（最快，轻量子进程）→ go-fitz（CGo 回退）
// 不依赖 pdfcpu，避免 "missing required resource subdict: Properties" 等解析失败。

// getPageCountViaMutool 用 mutool info 获取 PDF 页数。
// 解析 mutool info 输出中的 "Pages: N" 行。
// 这是最轻量的页数获取方式：启动子进程 → 读 Catalog → 退出，通常 <15ms。

// computeWorkerCount 根据 CPU 核心数和用户指定的并行百分比计算实际工作线程数。
// 百分比 0-100：0 表示串行（返回 1），100 表示用满所有核心。
// 结果至少为 1，最多为 CPU 核心数。
// computeWorkerCount 根据 CPU 核心数和用户指定的并行百分比计算实际工作线程数。

// readPPM 读取 PPM P6 格式文件，返回 *image.RGBA。
// PPM P6 格式：
//	P6
//	<width> <height>
//	<maxval>
//	<binary RGB data>
// 注释行以 # 开头，可出现在宽度/高度前。
// Maxval 为 255（不支持其他位深度）。
// readPPM 读取 PPM P6 格式文件，返回 *image.RGBA。

// applyColorCorrectionToFile 读取 JPEG 文件，应用色彩校正后写回。
// src 和 dst 可以是同一路径（原地修改）。
// applyColorCorrectionToFile 读取 JPEG 文件，应用色彩校正后写回。
// src 和 dst 可以是同一路径（原地修改）。
// 用于 -cc 标记下批量校正已生成的 JPEG 输出文件。

// cropImage 从图像中裁剪指定矩形区域，返回 *image.RGBA。
// 统一输出为 RGBA 以便后续编码逻辑只处理一种图像类型。

// toRGBA 将任意 image.Image 转换为 *image.RGBA。
// 通过 draw.Draw 将源图像绘制到新的 RGBA 画布上。

// isBackground 用一个非常宽松的白色阈值判断背景像素。
// 阈值偏高的原因是：我们更愿意把浅灰边缘也算作背景，
// 这样主体区域的外接框会更稳定。
// isBackground 判断像素是否为背景色（接近白色）。
// 阈值宽松（RGB > 200），这样浅灰边缘也算作背景，
// 主体区域的外接框会更稳定。

// traceTiming 打印阶段耗时信息到 stderr。
// 仅当 -timing / -t 启用时输出。
// 格式："[timing] <message>"

// traceImageMeta 记录单张图片的元数据到全局收集器。
// 收集后在程序结束时统一输出。

// resolveImageDimensions 尽量补全 pdfcpu 可能缺失的图片宽高。
// 兜底顺序：已解析出的尺寸优先，其次回退到原始 XObject 字典里的 Width/Height。
// resolveImageDimensions 尽量补全 pdfcpu 可能缺失的图片宽高。
// 兜底顺序：已解析出的尺寸优先，其次回退到原始 XObject 字典里的 Width/Height。
func resolveImageDimensions(imageDict *types.StreamDict, width, height int) (int, int) {
	if width <= 0 && imageDict != nil {
		if w := imageDict.IntEntry("Width"); w != nil && *w > 0 {
			width = *w
		}
	}
	if height <= 0 && imageDict != nil {
		if h := imageDict.IntEntry("Height"); h != nil && *h > 0 {
			height = *h
		}
	}
	return width, height
}

// outputExtension 根据用户指定的格式返回文件扩展名。
// "jpg" 或 "jpeg" → ".jpg"
// "png" → ".png"

// normalizeOutputImageExt 统一图片扩展名格式，移除前导点并统一为小写。
// ".JPG" → "jpg"
// "JPEG" → "jpeg"
func normalizeOutputImageExt(fileType string) string {
	switch strings.ToLower(strings.TrimSpace(fileType)) {
	case "jpg", "jpeg", "jpe":
		return "jpg"
	case "png":
		return "png"
	default:
		return "png"
	}
}

// decodeImageForOutput 根据文件类型解码图片数据。
// 支持 JPEG、PNG 两种常见格式。
func decodeImageForOutput(fileType string, reader io.Reader) (image.Image, error) {
	switch strings.ToLower(strings.TrimSpace(fileType)) {
	case "tif", "tiff":
		img, err := tiff.Decode(reader)
		if err != nil {
			return nil, err
		}
		return img, nil
	default:
		img, _, err := image.Decode(reader)
		if err != nil {
			return nil, err
		}
		return img, nil
	}
}

// isDirectCopyableImageType 判断图片类型是否可以直接复制（无需解码）。
// 直接可复制的类型：JPEG、JPEG2000
// 这些格式的原始字节流可以直接写入输出文件，无需经过解码→重新编码的损耗。
func isDirectCopyableImageType(fileType string) bool {
	switch strings.ToLower(strings.TrimSpace(fileType)) {
	case "jpg", "jpeg", "jpe", "png":
		return true
	default:
		return false
	}
}

// ─── PDF 压缩功能（基于 Ghostscript）──

// compressPDF 使用 Ghostscript 压缩 PDF 文件。
// preset: 压缩预设 (screen/ebook/printer/prepress)
// jpegQuality: JPEG 质量 1-100

func traceTiming(enabled bool, format string, args ...any) {
	if !enabled { return }
	msg := fmt.Sprintf("[timing] "+format, args...)
	fmt.Fprintln(os.Stderr, msg)
}

func traceImageMeta(meta imageMetaRecord) {
	var builder strings.Builder
	if meta.Time != "" {
		builder.WriteString(fmt.Sprintf("[image] page=%d source=%s object=%d index=%d size=%dx%d time=%s path=%s\n", meta.Page, meta.Source, meta.Object, meta.Index, meta.Width, meta.Height, meta.Time, meta.Path))
	} else {
		builder.WriteString(fmt.Sprintf("[image] page=%d source=%s object=%d index=%d size=%dx%d path=%s\n", meta.Page, meta.Source, meta.Object, meta.Index, meta.Width, meta.Height, meta.Path))
	}
	fmt.Fprint(os.Stdout, builder.String())
}


