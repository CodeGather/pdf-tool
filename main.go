package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/go-fitz"
	tiff "github.com/hhrutter/tiff"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/filter"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var debugLogsEnabled bool
var imageMetaEnabled bool
var imageMetaJSONEnabled bool
var globalImageMetaCollector *imageMetaCollector

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

func main() {
	inputFile := flag.String("i", "input.pdf", "输入 PDF 文件")
	outputDir := flag.String("o", "output", "输出目录")
	format := flag.String("f", "png", "输出图片格式：png 或 jpg")
	dpi := flag.Float64("dpi", 300, "渲染 DPI")
	mergeEnabled := flag.Bool("merge", false, "合并 PDF 文件")
	mergeInputDir := flag.String("merge-dir", "", "待合并 PDF 所在目录")
	mergeInputList := flag.String("merge-inputs", "", "逗号分隔的待合并 PDF 文件列表")
	mergeGlob := flag.String("merge-glob", "*.pdf", "合并模式下的文件匹配模式")
	mergeChunkSize := flag.Int("merge-chunk-size", 50, "合并模式下每批处理的 PDF 数量")
	mergeDivider := flag.Bool("merge-divider", false, "在合并文件之间插入分隔页")
	progressEnabled := flag.Bool("p", false, "打印合并进度 0-100")
	logEnabled := flag.Bool("log", false, "打印调试日志")
	flag.BoolVar(logEnabled, "l", false, "打印调试日志")
	metaEnabled := flag.Bool("meta", false, "打印图片宽高信息")
	flag.BoolVar(metaEnabled, "m", false, "打印图片宽高信息")
	metaJSONEnabled := flag.Bool("meta-json", false, "以 JSON 形式打印图片宽高信息")
	flag.BoolVar(metaJSONEnabled, "m-json", false, "以 JSON 形式打印图片宽高信息")
	timing := flag.Bool("timing", false, "打印每个阶段的耗时信息")
flag.BoolVar(timing, "t", false, "打印每个阶段的耗时信息")
	quality := flag.Int("quality", 85, "JPEG 编码质量 1-100（默认 85，兼顾质量和速度）")
	flag.IntVar(quality, "q", 85, "JPEG 编码质量 1-100（默认 85）")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "用法：%s [参数]\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output())
		fmt.Fprintln(flag.CommandLine.Output(), "使用示例：")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 基础转换")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -dpi 300")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 编码质量控制")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -q 50   # 小文件/低质量")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -q 85   # 高质量（默认）")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 调试与诊断")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l -t")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l -m")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l -m-json")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 合并 PDF")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -merge-chunk-size 20")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf -l")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -p")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -p -l")
	}
	flag.Parse()

	debugLogsEnabled = *logEnabled
	imageMetaEnabled = *metaEnabled
	imageMetaJSONEnabled = *metaJSONEnabled
	if debugLogsEnabled {
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(io.Discard)
	}
	log.SetFlags(0)
	globalImageMetaCollector = newImageMetaCollector(debugLogsEnabled && (imageMetaEnabled || imageMetaJSONEnabled), imageMetaJSONEnabled)

	if *mergeEnabled {
		if err := mergePDFs(*mergeInputDir, *mergeInputList, *mergeGlob, *outputDir, *mergeChunkSize, *mergeDivider, *progressEnabled); err != nil {
			fmt.Fprintf(os.Stderr, "PDF 合并失败：%v\n", err)
			os.Exit(1)
		}
		globalImageMetaCollector.flush()
		return
	}

	// 入口只负责参数解析、输出目录准备和调用核心转换逻辑。
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败：%v\n", err)
		os.Exit(1)
	}

	if err := convertPDFToImages(*inputFile, *outputDir, *format, *dpi, *timing, *progressEnabled, *quality); err != nil {
		fmt.Fprintf(os.Stderr, "PDF 转图片失败：%v\n", err)
		os.Exit(1)
	}
	globalImageMetaCollector.flush()
}

// mergePDFs 负责按参数收集输入文件，并在需要时分批合并，避免一次性处理过多文件。
func mergePDFs(inputDir, inputList, globPattern, outputFile string, chunkSize int, dividerPage, progressEnabled bool) error {
	files, err := collectMergeInputs(inputDir, inputList, globPattern)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no pdf files found for merge")
	}

	log.Printf("merge: start files=%d output=%s", len(files), outputFile)

	if chunkSize < 2 {
		chunkSize = len(files)
	}

	outDir := filepath.Dir(outputFile)
	if outDir != "." && outDir != "" {
		if err := os.MkdirAll(outDir, 0755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}

	traceProgress(progressEnabled, 0)
	if len(files) <= chunkSize {
		log.Printf("merge: single pass files=%d", len(files))
		if err := api.MergeCreateFile(files, outputFile, dividerPage, nil); err != nil {
			return fmt.Errorf("merge pdfs: %w", err)
		}
		log.Printf("merge: done output=%s", outputFile)
		traceProgress(progressEnabled, 100)
		return nil
	}

	tempDir, err := os.MkdirTemp(outDir, "pdf-tool-merge-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	chunkFiles := make([]string, 0, (len(files)+chunkSize-1)/chunkSize)
	for index := 0; index < len(files); index += chunkSize {
		end := index + chunkSize
		if end > len(files) {
			end = len(files)
		}
		log.Printf("merge: chunk %d start files=%d", len(chunkFiles)+1, end-index)
		chunkOut := filepath.Join(tempDir, fmt.Sprintf("chunk_%04d.pdf", len(chunkFiles)+1))
		if err := api.MergeCreateFile(files[index:end], chunkOut, dividerPage, nil); err != nil {
			return fmt.Errorf("merge chunk %d: %w", len(chunkFiles)+1, err)
		}
		chunkFiles = append(chunkFiles, chunkOut)
		log.Printf("merge: chunk %d done output=%s", len(chunkFiles), chunkOut)
		progress := len(chunkFiles) * 100 / ((len(files) + chunkSize - 1) / chunkSize)
		if progress > 99 {
			progress = 99
		}
		traceProgress(progressEnabled, progress)
	}

	log.Printf("merge: final pass chunks=%d", len(chunkFiles))
	if err := api.MergeCreateFile(chunkFiles, outputFile, dividerPage, nil); err != nil {
		return fmt.Errorf("merge final output: %w", err)
	}
	log.Printf("merge: done output=%s", outputFile)
	traceProgress(progressEnabled, 100)
	return nil
}

// collectMergeInputs 根据目录或显式列表收集合并输入，并保持稳定排序。
func collectMergeInputs(inputDir, inputList, globPattern string) ([]string, error) {
	if strings.TrimSpace(inputList) != "" {
		parts := strings.FieldsFunc(inputList, func(r rune) bool {
			return r == ',' || r == '\n' || r == '\r' || r == ';'
		})
		files := make([]string, 0, len(parts))
		for _, part := range parts {
			path := strings.TrimSpace(part)
			if path == "" {
				continue
			}
			files = append(files, path)
		}
		return files, nil
	}

	if strings.TrimSpace(inputDir) == "" {
		return nil, fmt.Errorf("merge mode requires -merge-dir or -merge-inputs")
	}
	pattern := globPattern
	if strings.TrimSpace(pattern) == "" {
		pattern = "*.pdf"
	}
	matches, err := filepath.Glob(filepath.Join(inputDir, pattern))
	if err != nil {
		return nil, fmt.Errorf("glob merge inputs: %w", err)
	}
	sort.Slice(matches, func(i, j int) bool {
		return naturalLess(filepath.Base(matches[i]), filepath.Base(matches[j]))
	})
	return matches, nil
}

var naturalTokenPattern = regexp.MustCompile(`\d+|\D+`)

func traceProgress(enabled bool, progress int) {
	if !enabled {
		return
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	fmt.Fprintln(os.Stderr, progress)
}

func naturalLess(left, right string) bool {
	leftParts := naturalTokenPattern.FindAllString(left, -1)
	rightParts := naturalTokenPattern.FindAllString(right, -1)
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		leftPart := leftParts[index]
		rightPart := rightParts[index]
		leftIsNumber := leftPart[0] >= '0' && leftPart[0] <= '9'
		rightIsNumber := rightPart[0] >= '0' && rightPart[0] <= '9'
		if leftIsNumber && rightIsNumber {
			leftTrimmed := strings.TrimLeft(leftPart, "0")
			rightTrimmed := strings.TrimLeft(rightPart, "0")
			if leftTrimmed == "" {
				leftTrimmed = "0"
			}
			if rightTrimmed == "" {
				rightTrimmed = "0"
			}
			if len(leftTrimmed) != len(rightTrimmed) {
				return len(leftTrimmed) < len(rightTrimmed)
			}
			if leftTrimmed != rightTrimmed {
				return leftTrimmed < rightTrimmed
			}
			if len(leftPart) != len(rightPart) {
				return len(leftPart) < len(rightPart)
			}
			continue
		}
		if leftPart != rightPart {
			return leftPart < rightPart
		}
	}
	return len(leftParts) < len(rightParts)
}

// convertPDFToImages 是整个程序的核心调度器。
// 它会先判断 PDF 是否存在裁剪路径，再决定：
// 1. 直接从对象流里提取嵌入图片；
// 2. 先渲染整页，再通过连通域分析把主体区域裁出来。
// 这样做的目的，是在“速度”和“兼容性”之间做分流。
func convertPDFToImages(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
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
	file, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF: %w", err)
	}
	defer file.Close()

	conf := model.NewDefaultConfiguration()
	conf.Cmd = model.EXTRACTIMAGES
	ctx, err := api.ReadValidateAndOptimize(file, conf)
	if err != nil {
		return fmt.Errorf("read PDF context: %w", err)
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
		return renderCropPDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
	case routeRenderWholePageImage:
		// 页面本身就是整图时，整页渲染比对象级重建更稳。
		// 这种场景通常对应扫描件或大图铺满页面，直接输出整页最合理。
		// 完全串行渲染，不使用任何并行。
		return renderWholePagePDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
	case routeDirectExtractTransparency, routeDirectExtractMultiImageStack, routeDirectExtractSingleObject:
		// 其余情况都走对象级提取。
		// 这里会在 writeDirectImage / writeDirectImageFast 里再细分：
		// 能快拷贝的快拷贝，不能快拷贝的再按颜色空间、遮罩和编码格式回退。
		return extractDirectImages(ctx, inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
	default:
		return fmt.Errorf("unsupported PDF route %v", route)
	}
}

type pdfDocumentRoute int

const (
	routeRenderCropComplexTransparency pdfDocumentRoute = iota
	routeDirectExtractTransparency
	routeDirectExtractMultiImageStack
	routeDirectExtractSingleObject
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

func dictAt(d types.Dict, key string) types.Dict {
	if d == nil {
		return nil
	}
	if v, ok := d[key]; ok {
		if dd, ok := v.(types.Dict); ok {
			return dd
		}
	}
	return nil
}

func classifyPDFDocument(ctx *model.Context, inputFile string) (pdfDocumentRoute, error) {
	var (
		anyGroup           bool
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

		// Group 表示页面有透明组或复合合成语义。
		// 一旦存在这类结构，说明直接按对象抠图更容易丢失层次或透明度。
		if g, _ := ctx.DereferenceDict(pageDict["Group"]); g != nil {
			anyGroup = true
		}

		// Form XObject 往往意味着页面里还有嵌套绘制或局部坐标系。
		// 这类内容更适合渲染后分析，而不适合只靠对象流直接复制。
		xobjDict := dictAt(dictAt(pageDict, "Resources"), "XObject")
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
	// 只要存在 Group，说明至少有透明组语义。
	// 对这类文件，直接提取通常比渲染裁剪更快，但前提是没有更复杂的 Form 结构。
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

func renderCropPDF(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	doc, err := fitz.New(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render crop: %w", err)
	}
	muteFitzWarnings(doc)
	defer doc.Close()

	pageCount := doc.NumPage()
	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	totalSaved := 0
	traceProgress(progressEnabled, 0)
	for pageIndex := 0; pageIndex < pageCount; pageIndex++ {
		pageStart := time.Now()
		// 渲染后做连通域分析，再裁出主体区域。
		// 这条路径更慢，但对复杂透明度或嵌套组合更稳。
		log.Printf("convert: render page=%d/%d path=render-crop", pageIndex+1, pageCount)
		traceTiming(timing, "page %d start", pageIndex+1)

		renderStart := time.Now()
		img, err := doc.ImageDPI(pageIndex, dpi)
		if err != nil {
			return fmt.Errorf("render page %d: %w", pageIndex+1, err)
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

		for cropIndex, cropRect := range crops {
			cropStart := time.Now()
			cropped := cropImage(img, cropRect)
			traceTiming(timing, "page %d crop %d crop-image=%s rect=%dx%d", pageIndex+1, cropIndex+1, time.Since(cropStart), cropRect.Dx(), cropRect.Dy())

			writeStart := time.Now()
			outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_%03d.%s", pageIndex+1, cropIndex+1, outputExtension(format)))
			if err := writeImageAtomically(outputPath, func(w io.Writer) error {
				switch format {
				case "png":
					return encodePNG(w, cropped)
				case "jpg", "jpeg":
					return jpeg.Encode(w, cropped, &jpeg.Options{Quality: quality})
				default:
					return fmt.Errorf("unsupported output format %q", format)
				}
			}); err != nil {
				return fmt.Errorf("save page %d crop %d: %w", pageIndex+1, cropIndex+1, err)
			}
			traceImageMeta(imageMetaRecord{
				Type:   "image-meta",
				Source: "render-crop",
				Page:   pageIndex + 1,
				Index:  cropIndex + 1,
				Width:  cropRect.Dx(),
				Height: cropRect.Dy(),
				Ext:    outputExtension(format),
				Time:   time.Since(cropStart).String(),
				Path:   outputPath,
			})
			traceTiming(timing, "page %d crop %d write=%s path=%s", pageIndex+1, cropIndex+1, time.Since(writeStart), outputPath)
			totalSaved++
		}
		traceTiming(timing, "page %d total=%s", pageIndex+1, time.Since(pageStart))
		traceProgress(progressEnabled, (pageIndex+1)*100/pageCount)
	}

	if totalSaved == 0 {
		return fmt.Errorf("no image crops were saved")
	}

	return nil
}

func renderWholePagePDF(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	ext := outputExtension(format)

	// 先用 pdfcpu 获取页数。
	conf := model.NewDefaultConfiguration()
	conf.Cmd = model.EXTRACTIMAGES
	file, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for page count: %w", err)
	}
	ctx, err := api.ReadValidateAndOptimize(file, conf)
	file.Close()
	if err != nil {
		return fmt.Errorf("read PDF for page count: %w", err)
	}
	pageCount := ctx.PageCount
	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	pdftoppmFormat := ext
	if pdftoppmFormat == "jpg" {
		pdftoppmFormat = "jpeg"
	}

	// 先尝试 2 路并行 pdftoppm（纯 os/exec 子进程，安全）。
	// 每块处理一段页范围，写到独立临时目录，完成后合并。
	convertStart := time.Now()
	if err := renderWholePagePDFParallel(inputFile, outputDir, pdftoppmFormat, ext, dpi, quality, pageCount); err == nil {
		traceTiming(timing, "pdftoppm conversion=%s all-pages=%d", time.Since(convertStart), pageCount)
		return renderWholePagePDFRename(outputDir, ext, pageCount, timing, progressEnabled)
	} else {
		log.Printf("parallel pdftoppm failed: %v, falling back to serial pdftoppm", err)
	}

	// 串行 pdftoppm 回退（与原来的逻辑一致）。
	if err := renderWholePagePDFSerial(inputFile, outputDir, pdftoppmFormat, ext, dpi, quality, pageCount); err != nil {
		log.Printf("serial pdftoppm failed: %v, falling back to go-fitz", err)
		return renderWholePagePDFGoFitz(inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
	}
	traceTiming(timing, "pdftoppm conversion=%s all-pages=%d", time.Since(convertStart), pageCount)
	return renderWholePagePDFRename(outputDir, ext, pageCount, timing, progressEnabled)
}

// renderWholePagePDFParallel 用多路并行 pdftoppm 分别渲染不同的页范围。
// pdftoppm 是独立 C 进程，无 CGo 信号栈问题，多路并行不会造成卡死。
func renderWholePagePDFParallel(inputFile, outputDir, pdftoppmFormat, ext string, dpi float64, quality, pageCount int) error {
	numWorkers := 4
	// 每块至少 1 页，避免空块
	chunkSize := (pageCount + numWorkers - 1) / numWorkers
	if chunkSize < 1 {
		chunkSize = 1
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numWorkers)

	for i := 0; i < numWorkers; i++ {
		first := i*chunkSize + 1
		last := (i + 1) * chunkSize
		if last > pageCount {
			last = pageCount
		}
		if first > pageCount || first > last {
			break
		}

		wg.Add(1)
		go func(first, last int) {
			defer wg.Done()

			// 每块写到独立临时目录，避免 pdftoppm 输出文件互相覆盖
			chunkDir, err := os.MkdirTemp(outputDir, ".pdftoppm-*")
			if err != nil {
				errCh <- fmt.Errorf("create chunk dir: %w", err)
				return
			}
			defer os.RemoveAll(chunkDir)

			args := []string{
				fmt.Sprintf("-%s", pdftoppmFormat),
				"-r", fmt.Sprintf("%.0f", dpi),
				"-f", strconv.Itoa(first),
				"-l", strconv.Itoa(last),
			}
			if pdftoppmFormat == "jpeg" {
				args = append(args, "-jpegopt", fmt.Sprintf("quality=%d", quality))
			}
			args = append(args, inputFile, filepath.Join(chunkDir, "page"))

			cmd := exec.Command("pdftoppm", args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				errCh <- fmt.Errorf("chunk pages %d-%d: %v: %s", first, last, err, strings.TrimSpace(string(out)))
				return
			}

			// 把 chunk 输出移到 outputDir
			entries, err := os.ReadDir(chunkDir)
			if err != nil {
				errCh <- fmt.Errorf("read chunk dir: %w", err)
				return
			}
			for _, entry := range entries {
				oldPath := filepath.Join(chunkDir, entry.Name())
				newPath := filepath.Join(outputDir, entry.Name())
				if err := os.Rename(oldPath, newPath); err != nil {
					errCh <- fmt.Errorf("move %s: %w", entry.Name(), err)
					return
				}
			}
		}(first, last)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			// 清理 outputDir 中已移入的部分文件
			pageDigits := len(strconv.Itoa(pageCount))
			for p := 1; p <= pageCount; p++ {
				os.Remove(filepath.Join(outputDir, fmt.Sprintf("page-%0*d.%s", pageDigits, p, ext)))
			}
			return err
		}
	}
	return nil
}

// renderWholePagePDFSerial 是串行 pdftoppm 回退路径。
func renderWholePagePDFSerial(inputFile, outputDir, pdftoppmFormat, ext string, dpi float64, quality, pageCount int) error {
	args := []string{
		fmt.Sprintf("-%s", pdftoppmFormat),
		"-r", fmt.Sprintf("%.0f", dpi),
	}
	if pdftoppmFormat == "jpeg" {
		args = append(args, "-jpegopt", fmt.Sprintf("quality=%d", quality))
	}
	args = append(args, inputFile, filepath.Join(outputDir, "page"))

	cmd := exec.Command("pdftoppm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// renderWholePagePDFRename 把 pdftoppm 输出的 page-XX.ext 重命名为 page_XXX_image_001.ext。
func renderWholePagePDFRename(outputDir, ext string, pageCount int, timing, progressEnabled bool) error {
	pageDigits := len(strconv.Itoa(pageCount))
	traceProgress(progressEnabled, 0)
	for pageNr := 1; pageNr <= pageCount; pageNr++ {
		pageStart := time.Now()
		oldName := fmt.Sprintf("page-%0*d.%s", pageDigits, pageNr, ext)
		oldPath := filepath.Join(outputDir, oldName)
		newName := fmt.Sprintf("page_%03d_image_001.%s", pageNr, ext)
		newPath := filepath.Join(outputDir, newName)
		if err := os.Rename(oldPath, newPath); err != nil {
			return fmt.Errorf("rename page %d: %w", pageNr, err)
		}
		log.Printf("convert: page=%d/%d path=render-whole-page", pageNr, pageCount)
		traceImageMeta(imageMetaRecord{
			Type:   "image-meta",
			Source: "render-whole-page",
			Page:   pageNr,
			Index:  1,
			Ext:    ext,
			Path:   newPath,
		})
		traceTiming(timing, "page %d total=%s", pageNr, time.Since(pageStart))
		traceProgress(progressEnabled, pageNr*100/pageCount)
	}
	return nil
}

// renderWholePagePDFGoFitz 是 pdftoppm 不可用时的 go-fitz 回退路径。
func renderWholePagePDFGoFitz(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	doc, err := fitz.New(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render whole page: %w", err)
	}
	muteFitzWarnings(doc)
	defer doc.Close()

	pageCount := doc.NumPage()
	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	traceProgress(progressEnabled, 0)
	for pageIndex := 0; pageIndex < pageCount; pageIndex++ {
		pageNr := pageIndex + 1
		pageStart := time.Now()
		log.Printf("convert: page=%d/%d path=render-whole-page", pageNr, pageCount)
		outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, outputExtension(format)))
		if err := renderWholePageImageWithDoc(doc, pageNr, dpi, outputPath, format, timing, quality, pageStart); err != nil {
			return err
		}
		traceImageMeta(imageMetaRecord{
			Type:   "image-meta",
			Source: "render-whole-page",
			Page:   pageNr,
			Index:  1,
			Ext:    outputExtension(format),
			Path:   outputPath,
		})
		traceTiming(timing, "page %d total=%s", pageNr, time.Since(pageStart))
		traceProgress(progressEnabled, pageNr*100/pageCount)
	}
	return nil
}

// extractDirectImages 走的是“对象级提取”路径。
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
	traceProgress(progressEnabled, 0)

	// 缓存 fitz.Document，供需要整页渲染的页面复用，避免每页重复 open/close PDF。
	var wholePageDoc *fitz.Document
	defer func() {
		if wholePageDoc != nil {
			wholePageDoc.Close()
		}
	}()

	// 这里按页处理，而不是一口气把所有页全部提上来。
	// 一方面可以在日志里清晰看到每页的耗时；另一方面，遇到单页异常时也更容易控制失败边界。
	// 逐页扫描对象号，页面内的图片对象会进入并发提取。
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
			traceProgress(progressEnabled, processedPages*100/ctx.PageCount)
			continue
		}

		// 如果这一页本身就是整图，直接走整页渲染。
		// 这比把一个巨大的图片对象再绕一圈对象级提取更直接，
		// 也更容易保证尺寸和视觉效果一致。
		if shouldRenderWholePageImage(ctx, pageNr, objNrs) {
			log.Printf("convert: page=%d/%d path=render-whole-page", pageNr, ctx.PageCount)
			outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, outputExtension(format)))
			// 延迟打开 fitz.Document，只在首次遇到需要整页渲染的页时初始化。
			// 之后的页复用同一个 doc，避免每页重复 open/close PDF。
			if wholePageDoc == nil {
				var openErr error
				wholePageDoc, openErr = fitz.New(inputFile)
				if openErr != nil {
					return fmt.Errorf("open PDF for whole page render: %w", openErr)
				}
				muteFitzWarnings(wholePageDoc)
			}
			if err := renderWholePageImageWithDoc(wholePageDoc, pageNr, dpi, outputPath, format, timing, quality, pageStart); err != nil {
				return err
			}
			traceImageMeta(imageMetaRecord{
				Type:   "image-meta",
				Source: "render-whole-page",
				Page:   pageNr,
				Index:  1,
				Ext:    outputExtension(format),
				Path:   outputPath,
			})
			totalWritten++
			traceTiming(timing, "direct-extract page %d=%s rendered-whole-page", pageNr, time.Since(pageStart))
			processedPages++
			traceProgress(progressEnabled, processedPages*100/ctx.PageCount)
			continue
		}
		log.Printf("convert: page=%d/%d path=direct-extract objects=%d", pageNr, ctx.PageCount, len(objNrs))

		// 串行处理该页的所有图片对象，不再使用 goroutine 池（之前的并发会导致电脑卡死）。
		pageWritten := 0
		for _, objNr := range objNrs {
			if err := writeDirectImage(ctx, inputFile, pageNr, objNr, inputStem, pageDigits, outputDir, format, dpi, timing, quality); err != nil {
				return err
			}
			pageWritten++
		}

		totalWritten += pageWritten

		traceTiming(timing, "direct-extract page %d=%s images=%d", pageNr, time.Since(pageStart), pageWritten)
		processedPages++
		traceProgress(progressEnabled, processedPages*100/ctx.PageCount)
	}

	if totalWritten == 0 {
		return fmt.Errorf("no images were written")
	}
	traceTiming(timing, "direct-extract total=%s", time.Since(start))
	return nil
}

func renderWholePageImage(inputFile string, pageNr int, dpi float64, outPath, format string, timing bool, quality int) error {
	pageStart := time.Now()
	doc, err := fitz.New(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render: %w", err)
	}
	muteFitzWarnings(doc)
	defer doc.Close()
	return renderWholePageImageWithDoc(doc, pageNr, dpi, outPath, format, timing, quality, pageStart)
}

// renderWholePageImageWithDoc 用已经打开的 doc 渲染指定页并写盘，避免重复打开 PDF。
func renderWholePageImageWithDoc(doc *fitz.Document, pageNr int, dpi float64, outPath, format string, timing bool, quality int, pageStart time.Time) error {
	img, err := doc.ImageDPI(pageNr-1, dpi)
	if err != nil {
		return fmt.Errorf("render page %d: %w", pageNr, err)
	}
	traceTiming(timing, "render-whole-page page %d render=%s", pageNr, time.Since(pageStart))

	if err := writeImageAtomically(outPath, func(w io.Writer) error {
		switch format {
		case "png":
			return encodePNG(w, img)
		case "jpg", "jpeg":
			return jpeg.Encode(w, img, &jpeg.Options{Quality: quality})
		default:
			return fmt.Errorf("unsupported output format %q", format)
		}
	}); err != nil {
		return fmt.Errorf("encode image %s: %w", outPath, err)
	}
	traceTiming(timing, "render-whole-page page %d write=%s path=%s", pageNr, time.Since(pageStart), outPath)
	return nil
}

// renderSinglePageCrop 渲染 PDF 单页后裁剪到最大前景区域。
// 用于 CMYK JPEG 等无法绕过 MuPDF 色彩管线的场景：
// 1. 用 go-fitz 渲染整页（确保颜色正确）
// 2. 连通域分析（flood fill）找到图片主体边界
// 3. 裁剪掉白边，输出仅图片区域
func renderSinglePageCrop(inputFile string, pageNr int, dpi float64, outPath, format string, timing bool, quality int) error {
	pageStart := time.Now()
	doc, err := fitz.New(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render+crop: %w", err)
	}
	muteFitzWarnings(doc)
	defer doc.Close()

	renderStart := time.Now()
	img, err := doc.ImageDPI(pageNr-1, dpi)
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
		return writeImageAtomically(outPath, func(w io.Writer) error {
			switch format {
			case "png":
				return encodePNG(w, img)
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
	cropped := cropImage(img, cropRect)
	traceTiming(timing, "render-crop page %d crop=%s", pageNr, time.Since(cropStart))

	if err := writeImageAtomically(outPath, func(w io.Writer) error {
		switch format {
		case "png":
			return encodePNG(w, cropped)
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
func writeDirectImage(ctx *model.Context, inputFile string, pageNr, objNr int, inputStem string, pageDigits int, outputDir, format string, dpi float64, timing bool, quality int) error {
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
	if ok, err := writeDirectImageFast(ctx, imageObj.ImageDict, objNr, inputStem, pageDigits, pageNr, resourceID, outputDir, objStart, timing, dpi, inputFile, format, quality); ok {
		return err
	}

	// 快路径没有命中，说明这个对象更复杂：
	// 可能是 JPX、可能是带软遮罩的 RGB，也可能是色彩空间太复杂，
	// 只能交给 pdfcpu 的通用提取逻辑兜底。
	decodeStart := time.Now()
	img, err := pdfcpu.ExtractImage(ctx, imageObj.ImageDict, false, resourceID, objNr, false)
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
		outputExt := outputExtension(format)
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
		if err := convertJPXToOutput(img.Reader, outPath, format, quality); err != nil {
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
		if err := writeImageAtomically(outPath, func(w io.Writer) error {
			return encodePNG(w, decodedImg)
		}); err != nil {
			traceTiming(timing, "direct-extract page %d obj %d encode-skip=%v", pageNr, objNr, err)
			return nil
		}
	} else {
		if err := writeImageAtomically(outPath, func(w io.Writer) error {
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
		if isCMYKJPEG(sd.Content) {
			// 分支：CMYK / Adobe YCCK JPEG。
			// 这类 JPEG 如果当普通 jpg 写出，大多数看图器会误按 YCbCr 解释，
			// 导致颜色完全偏色。常见库（sips、ImageMagick、Pillow）都不正确处理
			// Adobe APP14 transform=2 (YCCK) 标记，输出超级暗。
			// 这里用 go-fitz (MuPDF) 渲染整页后再裁剪到图片区域——
			// MuPDF 正确处理 PDF 所有颜色空间和 Overprint Mode (OPM=1)。
			// 直接独立转换 CMYK→RGB 会错误暗化，因为图片在页面上是和白色底色合成的。
			log.Printf("direct-extract page=%d obj=%d cmyk-jpeg detected, rendering+crop via muPDF", pageNr, objNr)
			outputExt := outputExtension(format)
			outPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, outputExt))
			traceImageMeta(imageMetaRecord{
				Type:   "image-meta",
				Source: "direct-fast",
				Page:   pageNr,
				Object: objNr,
				Width:  *w,
				Height: *h,
				Ext:    outputExt,
				Time:   time.Since(startedAt).String(),
				Path:   outPath,
			})
			writeStart := time.Now()
			if err := renderSinglePageCrop(inputFile, pageNr, dpi, outPath, format, timing, quality); err != nil {
				return false, fmt.Errorf("render+crop cmyk page %d obj %d: %w", pageNr, objNr, err)
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

	// 下面这段只处理 8-bit RGB / Gray，保证我们自己拼像素时数据布局是确定的。
	// 只要颜色空间、位深或遮罩条件不满足，就宁可回退，不做“猜测式输出”。
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
	softMask, err := extractSoftMask(ctx, sd, objNr, *w, *h, timing, pageNr)
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
	if err := writeImageAtomically(outPath, func(w io.Writer) error {
		enc := png.Encoder{CompressionLevel: png.NoCompression}
		return enc.Encode(w, img)
	}); err != nil {
		return false, fmt.Errorf("encode image %s: %w", outPath, err)
	}
	traceTiming(timing, "direct-extract page %d obj %d encode=%s", pageNr, objNr, time.Since(encodeStart))
	return true, nil
}

func writeImageAtomically(outPath string, write func(io.Writer) error) (err error) {
	// 所有真正的落盘都走临时文件 + rename。
	// 这样如果中途失败，不会在目标目录留下 0 字节或半成品文件。
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tempFile, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := tempFile.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close temp file: %w", closeErr)
			}
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()

	if err := write(tempFile); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, outPath); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

func convertJPXToOutput(reader io.Reader, outPath, format string, quality int) (err error) {
	// JPX 本身先落临时文件，是为了让系统转换器读取一个真实文件，
	// 这样 macOS 的 sips / ImageMagick 的兼容性都更高。
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	rawFile, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".jpx-*")
	if err != nil {
		return fmt.Errorf("create jpx temp file: %w", err)
	}
	rawPath := rawFile.Name()
	if _, err := io.Copy(rawFile, reader); err != nil {
		_ = rawFile.Close()
		_ = os.Remove(rawPath)
		return fmt.Errorf("write jpx temp file: %w", err)
	}
	if err := rawFile.Close(); err != nil {
		_ = os.Remove(rawPath)
		return fmt.Errorf("close jpx temp file: %w", err)
	}
	defer os.Remove(rawPath)

	outputExt := outputExtension(format)
	sipsFormat := "png"
	if outputExt == "jpg" {
		sipsFormat = "jpeg"
	}

	tempOutFile, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".out-*."+outputExt)
	if err != nil {
		return fmt.Errorf("create output temp file: %w", err)
	}
	tempOutPath := tempOutFile.Name()
	if err := tempOutFile.Close(); err != nil {
		_ = os.Remove(tempOutPath)
		return fmt.Errorf("close output temp file: %w", err)
	}
	if err := os.Remove(tempOutPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove output temp placeholder: %w", err)
	}

	// 再从 rawPath 转到最终目标格式。
	// 这一步会自动根据系统选择 sips / magick / convert。
	if err := convertJPXFile(rawPath, tempOutPath, outputExt, sipsFormat); err != nil {
		_ = os.Remove(tempOutPath)
		return err
	}
	if err := os.Rename(tempOutPath, outPath); err != nil {
		_ = os.Remove(tempOutPath)
		return fmt.Errorf("rename converted output: %w", err)
	}
	return nil
}

func convertJPXFile(rawPath, outPath, outputExt, sipsFormat string) error {
	type converter struct {
		path string
		name string
		args []string
	}

	var candidates []converter
	// 分支选择按平台优先级排列：
	// - macOS 优先用 sips，减少外部依赖；
	// - Windows 优先尝试随包放置的 ImageMagick，再回退到系统 PATH；
	// - 其他平台优先尝试 ImageMagick，作为可移植回退。
	// 这里不是"只选一个工具"，而是"按顺序尝试多个工具"，
	// 这样可在目标环境缺少某个命令时继续完成 JPX -> png/jpg 转换。
	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, converter{
			path: "sips",
			name: "sips",
			args: []string{"-s", "format", sipsFormat, rawPath, "--out", outPath},
		})
		candidates = append(candidates, converter{
			path: "magick",
			name: "magick",
			args: []string{rawPath, outPath},
		})
	case "windows":
		if bundledMagick := resolveBundledMagickExecutable(); bundledMagick != "" {
			candidates = append(candidates, converter{
				path: bundledMagick,
				name: filepath.Base(bundledMagick),
				args: []string{rawPath, outPath},
			})
		}
		candidates = append(candidates, converter{
			path: "magick",
			name: "magick",
			args: []string{rawPath, outPath},
		})
		candidates = append(candidates, converter{
			path: "convert",
			name: "convert",
			args: []string{rawPath, outPath},
		})
	default:
		candidates = append(candidates, converter{
			path: "magick",
			name: "magick",
			args: []string{rawPath, outPath},
		})
		candidates = append(candidates, converter{
			path: "convert",
			name: "convert",
			args: []string{rawPath, outPath},
		})
	}

	var errs []string
	// 逐个尝试候选转换器，直到有一个成功为止。
	// 失败信息保留到一起，便于在目标机器上定位到底是"命令不存在"
	// 还是"命令能执行但不支持当前 JPX 样本"。
	for _, candidate := range candidates {
		if candidate.path == "" {
			candidate.path = candidate.name
		}
		if _, lookErr := exec.LookPath(candidate.path); lookErr != nil {
			errs = append(errs, fmt.Sprintf("%s not found", candidate.name))
			continue
		}
		cmd := exec.Command(candidate.path, candidate.args...)
		combined, runErr := cmd.CombinedOutput()
		if runErr == nil {
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v: %s", candidate.name, runErr, strings.TrimSpace(string(combined))))
	}

	return fmt.Errorf("convert jpx to %s failed: %s", outputExt, strings.Join(errs, " | "))
}

func resolveBundledMagickExecutable() string {
	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		return ""
	}
	searchDir := filepath.Dir(exePath)
	matches, err := filepath.Glob(filepath.Join(searchDir, "ImageMagick*.exe"))
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, candidate := range matches {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// isCMYKJPEG 从 JPEG 数据流的 SOF marker 判断是否含 4 个分量（CMYK）。
// 比 sd.CSComponents 更可靠，因为它直接解析 JPEG 字节流，不依赖 pdfcpu 的元数据推断。
func isCMYKJPEG(data []byte) bool {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return false
	}
	// 扫描 SOF0(0xC0) / SOF1(0xC1) / SOF2(0xC2) marker
	i := 2
	for i < len(data)-1 {
		if data[i] != 0xFF {
			i++
			continue
		}
		if data[i+1] == 0x00 {
			i += 2
			continue
		}
		marker := data[i+1]
		if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 {
			// SOF 结构: FF C0 len(2) precision(1) height(2) width(2) numComponents(1)
			if i+9 < len(data) {
				return data[i+9] == 4
			}
			return false
		}
		// 跳过其他 marker
		if marker == 0xD8 || marker == 0xD9 {
			i += 2
			continue
		}
		if marker >= 0xD0 && marker <= 0xD7 {
			i += 2
			continue
		}
		if i+3 < len(data) {
			segLen := int(data[i+2])<<8 | int(data[i+3])
			i += 2 + segLen
		} else {
			break
		}
	}
	return false
}

// convertCMYKJPEGToOutput 把 CMYK JPEG 转成正确的 RGB 图片（PNG）。
// macOS 上用 sips 系统工具完成 CMYK→RGB 转换，颜色准确，零外部依赖。
func convertCMYKJPEGToOutput(data []byte, outPath string) (err error) {
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	// 写临时 CMYK JPEG 文件
	tmpJPEG, err := os.CreateTemp(dir, ".cmyk-*.jpg")
	if err != nil {
		return fmt.Errorf("create cmyk temp file: %w", err)
	}
	tmpJPEGPath := tmpJPEG.Name()
	if _, err := tmpJPEG.Write(data); err != nil {
		_ = tmpJPEG.Close()
		_ = os.Remove(tmpJPEGPath)
		return fmt.Errorf("write cmyk temp file: %w", err)
	}
	if err := tmpJPEG.Close(); err != nil {
		_ = os.Remove(tmpJPEGPath)
		return fmt.Errorf("close cmyk temp file: %w", err)
	}
	defer os.Remove(tmpJPEGPath)

	// 用 sips 把 CMYK JPEG 转为 PNG（sips 自动处理 CMYK→RGB 色彩空间转换）
	tmpOut, err := os.CreateTemp(dir, ".cmyk-out-*.png")
	if err != nil {
		return fmt.Errorf("create output temp file: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	_ = tmpOut.Close()
	_ = os.Remove(tmpOutPath)

	cmd := exec.Command("sips", "-s", "format", "png", tmpJPEGPath, "--out", tmpOutPath)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		_ = os.Remove(tmpOutPath)
		return fmt.Errorf("sips convert cmyk: %v: %s", runErr, strings.TrimSpace(string(output)))
	}

	if err := os.Rename(tmpOutPath, outPath); err != nil {
		_ = os.Remove(tmpOutPath)
		return fmt.Errorf("rename converted output: %w", err)
	}
	return nil
}

func encodePNG(w io.Writer, img image.Image) error {
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	return enc.Encode(w, img)
}

// extractSoftMask 读取并解码 SMask。
// 它只接受与主图像尺寸严格一致、且位深为 8 的软遮罩，
// 这样可以避免把错误尺寸的透明信息错误叠到图片上。
func extractSoftMask(ctx *model.Context, sd *types.StreamDict, objNr, w, h int, timing bool, pageNr int) ([]byte, error) {
	start := time.Now()
	o, _ := sd.Find("SMask")
	if o == nil {
		// 分支：没有软遮罩。
		// 这种情况下 alpha 直接视为 255，不需要额外处理。
		// 没有软遮罩时，直接返回空，外层会继续当前图像输出。
		return nil, nil
	}

	sm, _, err := ctx.XRefTable.DereferenceStreamDict(o)
	if err != nil {
		return nil, err
	}
	if sm == nil {
		return nil, nil
	}
	if err := sm.Decode(); err != nil {
		return nil, err
	}
	bpc := sm.IntEntry("BitsPerComponent")
	if bpc == nil || *bpc != 8 {
		// 分支：软遮罩不是 8-bit。
		// 当前快速路径只接受标准 8-bit alpha，其他情况直接放弃。
		return nil, nil
	}
	if len(sm.Content) != w*h {
		// 分支：软遮罩尺寸不对。
		// 一旦宽高不一致，alpha 和像素就会错位，所以不能继续用。
		// 软遮罩大小和图像尺寸不匹配，说明不能安全使用。
		// 这种情况如果继续合成，alpha 会错位。
		return nil, nil
	}
	traceTiming(timing, "direct-extract page %d obj %d softmask-decode=%s", pageNr, objNr, time.Since(start))
	return sm.Content, nil
}

// pdfHasClipPath 会扫描所有页面的内容流。
// 只要任意页面出现裁剪路径，就说明“直接抽图”可能不可靠，
// 需要切到渲染后裁剪的策略。
func pdfHasClipPath(ctx *model.Context) (bool, error) {
	// 只要任意页面的内容流里出现裁剪路径，就切换到渲染+裁剪策略。
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, false)
		if err != nil {
			return false, fmt.Errorf("page %d dict: %w", pageNr, err)
		}
		content, err := ctx.PageContent(pageDict, pageNr)
		if err != nil {
			if err == model.ErrNoContent {
				// 分支：当前页没有内容流。
				// 这是合法情况，说明这一页可能是空白页，直接扫描下一页。
				continue
			}
			return false, fmt.Errorf("page %d content: %w", pageNr, err)
		}
		if hasClipOperator(content) {
			return true, nil
		}
	}
	return false, nil
}

// pdfHasMultipleImageObjects 会检查是否存在“同一页里有多个图片对象”的情况。
// 这类 PDF 往往不适合只按对象级别直取，因为直取只能拿到嵌入对象，
// 无法反映页面最终渲染出来的图片实例数。
func pdfHasMultipleImageObjects(ctx *model.Context) (bool, error) {
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		objNrs := pdfcpu.ImageObjNrs(ctx, pageNr)
		if len(objNrs) > 1 {
			return true, nil
		}
	}
	return false, nil
}

// hasClipOperator 不是一个严格的 PDF 语法解析器，
// 它只是做一个快速启发式判断：既要看到裁剪指令，也要看到普通绘图指令。
// 这样可以减少把文本内容或无关字节误判成裁剪路径的概率。
func hasClipOperator(content []byte) bool {
	// 一些 PDF 会把很多图片放在重复的矩形裁剪框里，这种情况更适合直取。
	// 只有裁剪命中不多、而且同时存在普通绘图指令时，才按“需要裁剪”处理。
	if len(clipOperatorPattern.FindAllIndex(content, -1)) > maxClipOperatorCountForCrop {
		return false
	}
	return clipOperatorPattern.Match(content) && drawingOperatorPattern.Match(content)
}

var clipOperatorPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_/])W\*?([^A-Za-z0-9_]|$)`)

var drawingOperatorPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_/])(m|l|c|v|y|h|S|s|f|F|B|b|B\*|b\*)([^A-Za-z0-9_]|$)`)

const maxClipOperatorCountForCrop = 8

type region struct {
	rect image.Rectangle
	area int
}

const minRegionArea = 50000

// findLargestRegions 从整页渲染结果里提取最大的前景区域。
// 它的假设很简单：
// - 页面大部分背景接近白色；
// - 目标内容在像素上是连通的或者近似连通的；
// - 前景面积越大，越可能是我们想要的图像主体。
func findLargestRegions(img image.Image, maxRegions int) ([]image.Rectangle, error) {
	// 把渲染结果转成 RGBA 后，按背景阈值做 flood fill，提取最大的前景区域。
	rgba := toRGBA(img)
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
			if isBackground(rgba.RGBAAt(x, y)) {
				visited[idx] = true
				continue
			}

			rect, area := floodFillRegion(rgba, x, y, visited)
			if area >= minRegionArea {
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
			if isBackground(img.RGBAAt(n.X, n.Y)) {
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
func cropImage(img image.Image, rect image.Rectangle) *image.RGBA {
	// 裁剪时统一转换成 RGBA，避免不同图像类型之间的绘制差异。
	rgba := toRGBA(img)
	crop := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(crop, crop.Bounds(), rgba, rect.Min, draw.Src)
	return crop
}

func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	return rgba
}

// isBackground 用一个非常宽松的白色阈值判断背景像素。
// 阈值偏高的原因是：我们更愿意把浅灰边缘也算作背景，
// 这样主体区域的外接框会更稳定。
func isBackground(pixel color.RGBA) bool {
	// 这里把接近白色的像素都视为背景，便于把页面上的主体区域分离出来。
	return pixel.R >= 248 && pixel.G >= 248 && pixel.B >= 248
}

func traceTiming(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	if globalImageMetaCollector != nil && globalImageMetaCollector.enabled {
		return
	}
	msg := fmt.Sprintf("[timing] "+format, args...)
	fmt.Fprintln(os.Stderr, msg)
}

func traceImageMeta(meta imageMetaRecord) {
	if globalImageMetaCollector == nil || !globalImageMetaCollector.enabled {
		return
	}
	globalImageMetaCollector.add(meta)
}

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

func outputExtension(format string) string {
	if format == "jpg" {
		return "jpg"
	}
	return format
}

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

func isDirectCopyableImageType(fileType string) bool {
	switch strings.ToLower(strings.TrimSpace(fileType)) {
	case "jpg", "jpeg", "jpe", "png":
		return true
	default:
		return false
	}
}
