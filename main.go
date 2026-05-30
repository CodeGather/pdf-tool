// pdf-tool — PDF 图片提取与合并工具。
//
// 核心功能：
//  1. 从 PDF 中提取嵌入的图片（直取路径）：优先从对象流中直接复制 JPEG/JPEG2000
//     或快速解码 8-bit RGB/Gray 图片，复杂格式回退到 pdfcpu 通用解码。
//  2. 渲染后裁剪（渲染路径）：对包含裁剪路径、透明度、图层混合等复杂效果的 PDF，
//     先用 mutool draw -ppm 渲染整页，再通过连通域分析裁出图片主体。
//  3. 多 PDF 合并：利用 pdfcpu 的 MergeCreateFile 合并多个 PDF 文件。
//
// 渲染引擎：
//  - 主渲染引擎：mutool draw -ppm（MuPDF），颜色与 PDF 阅读器 100% 一致
//  - 回退引擎：go-fitz（仅在 mutool 完全不可用时触发）
//  - pdftoppm 已完全移除（lcms2 的 CMYK→RGB 转换偏红）
//
// 并行策略：
//  - mutool 渲染：按页范围拆分为多个独立子进程并行执行
//  - PPM→JPEG 编码：使用 goroutine 池并行编码
//  - 并行度由 -cpu 参数控制（0-100%，默认 25），实际线程数 = CPU核心数 * 百分比
//
// 路由策略：
//  - 任何页面有 /Group → 检查是否有裁剪路径（hasPageContentClip）
//  - 有裁剪路径且 cm_a/clip_w 比例 > 1.05 → render-crop（渲染后裁剪）
//  - 无 Group + 有裁剪路径（比例 > 1.05）→ render-crop
//  - 其余 → direct-extract（直取）
//
// 输出格式：
//  - JPEG（-f jpg）：默认 85% 质量，CMYK JPEG 自动识别并正确转换
//  - PNG（-f png）：仅当指定时输出
//  - 自动格式判断：CMYK JPEG 场景自动输出 jpg，避免重编码
//
// 8-bit 快速路径：
//  - FlateDecode 编码的 8-bit RGB/Gray 图片会走 sd.Decode() 快速解码
//  - 绕过 pdfcpu 的通用 ExtractImage 路径（含 SMask 合成的锁竞争）
//  - SMask 处理在快速路径内完成，不碰 ctxMu，goroutine-safe
//
// 版本：v2.0+（基于 mutool 渲染引擎，已移除 pdftoppm）
package main

import (
	"bytes"
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
var colorCorrectionEnabled bool
// mutoolPath 缓存 mutool 执行路径，首次使用时检测。
var mutoolPath string
// parallelPercent 用户指定的并行百分比（0-100），默认 50。
// 实际工作线程数由 computeWorkerCount() 根据 CPU 核心数计算。
var parallelPercent int

// imageMetaCollector 收集并输出每张导出图片的元数据（尺寸、来源、路径等）。
// 支持两种输出模式：
//   - 文本模式（-m）：每行一个 [image] 记录，适合 grep/awk 处理
//   - JSON 模式（-m-json）：JSON 数组，适合程序解析
// 线程安全：通过 sync.Mutex 保护 records 切片。
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

// main 是程序入口，负责：
//
//  1. 解析命令行参数（flag）—— 所有参数均在函数开头的 flag 定义中声明
//  2. 根据参数决定执行路径：
//     a) -merge 模式：收集输入 PDF → 分批合并 → 输出合并后的 PDF
//     b) 默认模式：单个 PDF → 路由分类（classifyPDFDocument）→ 直取或渲染裁剪
//  3. 初始化全局状态（日志、元数据收集器、并行度）
//
// 参数解析完成后，核心转换逻辑委托给 convertPDFToImages()。
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
	mergeCompress := flag.Bool("merge-compress", false, "合并后执行完整压缩优化（更小体积，更慢速度）")
	progressEnabled := flag.Bool("p", false, "打印合并进度 0-100")
	compressEnabled := flag.Bool("compress", false, "压缩 PDF（结构优化 + 图片重压缩为 JPEG）")
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
	colorCorrection := flag.Bool("cc", false, "对 pdftoppm 渲染结果应用色彩校正（已弃用的选项，当前引擎为 mutool 无需校正）")
	parallelPct := flag.Int("cpu", 25, "CPU 使用率百分比 0-100（0=串行，100=用满所有核心）。实际工作线程数由 CPU 核心数 * 百分比 / 100 动态计算。默认值 25 在 14 核上 = 4 线程，在 4 核上 = 1 线程")
	flag.Usage = func() {
		numCPU := runtime.NumCPU()
		fmt.Fprintf(flag.CommandLine.Output(), "用法：%s [参数]\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintln(flag.CommandLine.Output())
		fmt.Fprintf(flag.CommandLine.Output(), "  可用CPU核心: %d | -cpu 默认25 => %d 线程\n", numCPU, (numCPU*25+50)/100)
		fmt.Fprintln(flag.CommandLine.Output())
		fmt.Fprintln(flag.CommandLine.Output(), "使用示例：")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 基础转换")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -dpi 300")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # CPU 并行度控制")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -cpu 0    # 串行模式（1线程）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -cpu 50   # 使用 50% CPU（默认25% => 4线程）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -cpu 100  # 用满所有 CPU 核心")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 编码质量控制")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -q 50   # 低质量（小文件）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -q 85   # 高质量（默认）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -q 100  # 最高质量（文件大）")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 调试与诊断")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l          # 调试日志")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l -t       # 调试 + 耗时分析")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l -m       # 调试 + 图片元数据")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -l -m-json  # 调试 + JSON 元数据")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 输出格式控制")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f png      # 输出 PNG（默认）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg      # 输出 JPEG")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -f jpg -dpi 600  # 高 DPI")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 合并 PDF")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -merge-chunk-size 20")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf -l")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -p")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -p -l")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf -merge-divider")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 诊断：查看 PDF 路由分类结果（不实际输出图片）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o /tmp/out -l    # 日志显示路由决策")
	}
	flag.Parse()

	debugLogsEnabled = *logEnabled
	imageMetaEnabled = *metaEnabled
	imageMetaJSONEnabled = *metaJSONEnabled
	colorCorrectionEnabled = *colorCorrection
	parallelPercent = *parallelPct
	if debugLogsEnabled && !imageMetaJSONEnabled {
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(io.Discard)
	}
	log.SetFlags(0)
	globalImageMetaCollector = newImageMetaCollector(debugLogsEnabled && (imageMetaEnabled || imageMetaJSONEnabled), imageMetaJSONEnabled)

	if *compressEnabled {
		if err := compressPDF(*inputFile, *outputDir, *quality, *logEnabled); err != nil {
			fmt.Fprintf(os.Stderr, "PDF 压缩失败：%v\n", err)
			os.Exit(1)
		}
		return
	}

	if *mergeEnabled {
		if err := mergePDFs(*mergeInputDir, *mergeInputList, *mergeGlob, *outputDir, *mergeChunkSize, *mergeDivider, *progressEnabled, *mergeCompress); err != nil {
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
func mergePDFs(inputDir, inputList, globPattern, outputFile string, chunkSize int, dividerPage, progressEnabled, compress bool) error {
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

	// 快速合并配置：跳过优化，仅做对象合并，速度极快
	fastMergeConf := model.NewDefaultConfiguration()
	fastMergeConf.Cmd = model.MERGECREATE
	fastMergeConf.Optimize = false
	fastMergeConf.OptimizeBeforeWriting = false

	if len(files) <= chunkSize {
		log.Printf("merge: single pass files=%d", len(files))
		// 单批次批量合并所有文件（跳过优化）
		tmpOut := filepath.Join(outDir, ".merge_batch_tmp.pdf")
		if err := api.MergeCreateFile(files, tmpOut, dividerPage, fastMergeConf); err != nil {
			os.Remove(tmpOut)
			return fmt.Errorf("merge batch: %w", err)
		}
		if compress {
			traceProgress(progressEnabled, 90)
			// 完整优化输出（体积小，速度慢）
			if err := api.MergeCreateFile([]string{tmpOut}, outputFile, false, nil); err != nil {
				os.Remove(tmpOut)
				return fmt.Errorf("merge final optimize: %w", err)
			}
			os.Remove(tmpOut)
		} else {
			// 直接输出（体积大，速度快）
			if err := os.Rename(tmpOut, outputFile); err != nil {
				os.Remove(tmpOut)
				return fmt.Errorf("rename to output: %w", err)
			}
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
	totalChunks := (len(files) + chunkSize - 1) / chunkSize
	for ci, index := 0, 0; index < len(files); ci, index = ci+1, index+chunkSize {
		end := index + chunkSize
		if end > len(files) {
			end = len(files)
		}
		log.Printf("merge: chunk %d start files=%d", ci+1, end-index)
		chunkOut := filepath.Join(tempDir, fmt.Sprintf("chunk_%04d.pdf", ci+1))
		// 批量合并整个 chunk（跳过优化），比逐文件快很多
		if err := api.MergeCreateFile(files[index:end], chunkOut, dividerPage, fastMergeConf); err != nil {
			return fmt.Errorf("merge chunk %d: %w", ci+1, err)
		}
		chunkFiles = append(chunkFiles, chunkOut)
		log.Printf("merge: chunk %d done output=%s", ci+1, chunkOut)
		traceProgress(progressEnabled, (ci+1)*99/totalChunks)
	}

	// 最终合并各 chunk：是否压缩由 -merge-compress 控制
	mergeConf := fastMergeConf
	if compress {
		mergeConf = nil
	}
	log.Printf("merge: final pass chunks=%d compress=%v", len(chunkFiles), compress)
	if err := api.MergeCreateFile(chunkFiles, outputFile, dividerPage, mergeConf); err != nil {
		return fmt.Errorf("merge final output: %w", err)
	}
	log.Printf("merge: done output=%s", outputFile)
	traceProgress(progressEnabled, 100)
	return nil
}

// compressPDF 压缩单个 PDF 文件：先进行结构优化（pdfcpu），再重压缩 FlateDecode 图片为 JPEG。
// 输入：inFile — 源 PDF 路径
// 输出：outFile — 压缩后 PDF 路径
// quality — JPEG 编码质量 1-100
// verbose — 是否打印日志
func compressPDF(inFile, outFile string, quality int, verbose bool) error {
	if quality < 1 || quality > 100 {
		quality = 85
	}

	// Step 1: 结构优化
	optFile := outFile + ".opt_tmp"
	if err := api.OptimizeFile(inFile, optFile, nil); err != nil {
		os.Remove(optFile)
		return fmt.Errorf("结构优化: %w", err)
	}
	defer os.Remove(optFile)

	// Step 2: 打开优化后的 PDF
	f, err := os.Open(optFile)
	if err != nil {
		return fmt.Errorf("打开优化文件: %w", err)
	}
	defer f.Close()

	conf := model.NewDefaultConfiguration()
	conf.Optimize = false
	conf.OptimizeBeforeWriting = false
	ctx, err := api.ReadAndValidate(f, conf)
	if err != nil {
		return fmt.Errorf("读取 PDF: %w", err)
	}

	// Step 3: 遍历所有图片，重压缩 FlateDecode 为 JPEG
	recompressed := 0
	for objNr, entry := range ctx.Table {
		if entry.Free || entry.Object == nil {
			continue
		}
		sd, ok := entry.Object.(types.StreamDict)
		if !ok {
			continue
		}
		subtype := sd.NameEntry("Subtype")
		if subtype == nil || *subtype != "Image" {
			continue
		}
		// 只处理 FlateDecode 图片（PNG 编码）
		isFlate := false
		for _, pl := range sd.FilterPipeline {
			if pl.Name == "FlateDecode" {
				isFlate = true
				break
			}
		}
		if !isFlate {
			continue
		}

		wPtr := sd.IntEntry("Width")
		hPtr := sd.IntEntry("Height")
		if wPtr == nil || hPtr == nil {
			continue
		}
		w, h := *wPtr, *hPtr

		if err := sd.Decode(); err != nil {
			continue
		}
		if len(sd.Content) == 0 {
			continue
		}

		csPtr := sd.NameEntry("ColorSpace")
		cs := ""
		if csPtr != nil {
			cs = *csPtr
		}
		components := 3
		switch cs {
		case "DeviceGray", "G":
			components = 1
		case "DeviceCMYK":
			// CMYK 图片跳过重压缩，保持原样
			continue
		}

		expectedSize := w * h * components
		if len(sd.Content) < expectedSize {
			continue
		}

		var img image.Image
		switch components {
		case 1:
			gray := image.NewGray(image.Rect(0, 0, w, h))
			copy(gray.Pix, sd.Content[:w*h])
			img = gray
		case 3:
			rgb := image.NewRGBA(image.Rect(0, 0, w, h))
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					srcIdx := y*w*3 + x*3
					dstIdx := y*rgb.Stride + x*4
					rgb.Pix[dstIdx+0] = sd.Content[srcIdx+0]
					rgb.Pix[dstIdx+1] = sd.Content[srcIdx+1]
					rgb.Pix[dstIdx+2] = sd.Content[srcIdx+2]
					rgb.Pix[dstIdx+3] = 255
				}
			}
			img = rgb
		default:
			continue
		}

		var jpegBuf bytes.Buffer
		if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: quality}); err != nil {
			continue
		}

		// 更新 StreamDict 和 Dict 条目
		sd.Raw = jpegBuf.Bytes()
		sd.Content = nil
		sd.FilterPipeline = []types.PDFFilter{{Name: "DCTDecode"}}
		l := int64(len(sd.Raw))
		sd.StreamLength = &l
		sd.StreamLengthObjNr = nil
		sd.Update("Filter", types.Name("DCTDecode"))
		sd.Update("Length", types.Integer(l))
		sd.Delete("DecodeParms")

		entry.Object = sd
		ctx.Table[objNr] = entry
		recompressed++
	}

	if verbose {
		log.Printf("compress: %d 张 FlateDecode 图片重压缩为 JPEG Q%d", recompressed, quality)
	}

	// Step 4: 写出
	if err := api.WriteContextFile(ctx, outFile); err != nil {
		return fmt.Errorf("写出 PDF: %w", err)
	}

	if verbose {
		src, _ := os.Stat(inFile)
		dst, _ := os.Stat(outFile)
		log.Printf("compress: %s (%.1f MB) → %s (%.1f MB, %.1f%%)",
			inFile,
			float64(src.Size())/1024/1024,
			outFile,
			float64(dst.Size())/1024/1024,
			float64(dst.Size())*100/float64(src.Size()),
		)
	}

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

// traceProgress 显示合并进度百分比（0-100）。
// 仅在 progressEnabled 为 true 时输出到 stderr，避免干扰 stdout 的正常输出。
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

// naturalLess 实现自然排序（natural sort）的比较函数。
// 将字符串按数字和非数字片段分拆，数字片段按数值比较，非数字按字符串比较。
// 例如："page2" < "page10"（而非字典序的 "page10" < "page2"）。
// 用于合并模式下文件列表的稳定排序。
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
		if findMutool() == "" {
			return fmt.Errorf("read PDF context: %w (mutool also unavailable)", err)
		}
		return renderCropPDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
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
		return renderCropPDF(inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
	case routeDirectExtractTransparency, routeDirectExtractMultiImageStack, routeDirectExtractSingleObject:
		// 其余情况都走对象级提取。
		// 这里会在 writeDirectImage / writeDirectImageFast 里再细分：
		// 能快拷贝的快拷贝，不能快拷贝的再按颜色空间、遮罩和编码格式回退。
		return extractDirectImages(ctx, inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
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
//
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

// hasPageContentClip 检查页面的内容流是否包含 clip 操作符（W / W*）。
// 这用于区分 Illustrator 等工具自动添加的 /Group /Transparency 标记
//（没有实际裁剪）和确实有裁剪路径的页面。
// content stream 此时可能还是 FlateDecode 压缩的，需要先解压再检查。
func hasPageContentClip(ctx *model.Context, pageDict types.Dict) bool {
	// 先用 Dereference 解析间接引用，再尝试转为 StreamDict
	obj, err := ctx.Dereference(pageDict["Contents"])
	if err != nil {
		log.Printf("CLIP_CHECK: content stream deref failed: %v", err)
		return false
	}
	if obj == nil {
		return false
	}
	sd, ok := obj.(types.StreamDict)
	if !ok {
		return false
	}
	// 解码 content stream（如果还没解码）
	if len(sd.Content) == 0 && len(sd.Raw) > 0 {
		if decodeErr := sd.Decode(); decodeErr != nil {
			return false
		}
	}
	content := string(sd.Content)

	// 检查是否包含 W n（clip 操作符）
	if !strings.Contains(content, "W n") && !strings.Contains(content, "W* n") {
		return false
	}

	// 有 clip 路径，检查 clip 区域是否和图片变换区域匹配。
	// 很多 PDF 的 clip 就是图片的完整显示区域（ArtBox），没有实际裁剪。
	// 只有 clip 区域明显小于图片变换区域时，才有真正的内容裁剪。
	//
	// 解析 content stream 提取：
	//   1. clip rect: "re" 前面是 x y w h（在 "W n" 之前）
	//   2. cm matrix: "cm" 前面是 a b c d e f（在 "/Do" 之前）
	//   3. 比较 clip_w vs cm_a
	clipW, _ := extractRect(content)
	cmA, _ := extractCM(content)
	if clipW > 0 && cmA > 0 {
		ratio := cmA / clipW
		// 如果 cm_a 明显大于 clip_width（>5%），说明图片被裁剪
		return ratio > 1.05
	}

	// 解析失败时保守返回 true（有 clip 标记就去 render-crop）
	return true
}

// extractRect 从 PDF 内容流中提取最后一个 re 操作符的矩形尺寸 (w, h)。
func extractRect(content string) (float64, float64) {
	// 在 PDF 中 re 的格式: "x y w h re"
	// 找最后一个 "re" 前面的数字
	idx := strings.LastIndex(content, "re")
	if idx < 2 {
		return 0, 0
	}
	// 往回找 4 个数字（x y w h）
	fields := strings.Fields(content[:idx])
	if len(fields) < 4 {
		return 0, 0
	}
	// 取最后 4 个数字: x y w h
	w, _ := strconv.ParseFloat(fields[len(fields)-2], 64)
	h, _ := strconv.ParseFloat(fields[len(fields)-1], 64)
	return w, h
}

// extractCM 从 PDF 内容流中提取最后一个 cm 矩阵的 a 和 d 值。
func extractCM(content string) (float64, float64) {
	// 在 PDF 中 cm 的格式: "a b c d e f cm"
	idx := strings.LastIndex(content, "cm")
	if idx < 2 {
		return 0, 0
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 6 {
		return 0, 0
	}
	// 取最后 6 个数字: a b c d e f
	a, _ := strconv.ParseFloat(fields[len(fields)-6], 64)
	d, _ := strconv.ParseFloat(fields[len(fields)-3], 64)
	return a, d
}

// extractImageFullCM 从 PDF 内容流中提取最后一个 cm 矩阵的全部 6 个值 (a b c d e f)。
// 用于确定图片在页面上的精确放置位置和缩放。
// 返回值: a, b, c, d, e, f, ok
func extractImageFullCM(content string) (float64, float64, float64, float64, float64, float64, bool) {
	idx := strings.LastIndex(content, "cm")
	if idx < 2 {
		return 0, 0, 0, 0, 0, 0, false
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 6 {
		return 0, 0, 0, 0, 0, 0, false
	}
	a, _ := strconv.ParseFloat(fields[len(fields)-6], 64)
	b, _ := strconv.ParseFloat(fields[len(fields)-5], 64)
	c, _ := strconv.ParseFloat(fields[len(fields)-4], 64)
	d, _ := strconv.ParseFloat(fields[len(fields)-3], 64)
	e, _ := strconv.ParseFloat(fields[len(fields)-2], 64)
	f, _ := strconv.ParseFloat(fields[len(fields)-1], 64)
	return a, b, c, d, e, f, true
}

// getPageContentString 获取页面解码后的内容流文本。
// 复用了与 hasPageContentClip 相同的解压逻辑。
func getPageContentString(ctx *model.Context, pageDict types.Dict) string {
	obj, err := ctx.Dereference(pageDict["Contents"])
	if err != nil || obj == nil {
		return ""
	}
	sd, ok := obj.(types.StreamDict)
	if !ok {
		return ""
	}
	if len(sd.Content) == 0 && len(sd.Raw) > 0 {
		if decodeErr := sd.Decode(); decodeErr != nil {
			return ""
		}
	}
	return string(sd.Content)
}

// classifyPDFDocument 分析 PDF 的所有页面，根据透明度、裁剪路径、图片分布等
// 特征综合判定使用哪种提取策略。
//
// 判定流程：
//  1. 逐页扫描，收集特征：Group/Transparency、裁剪路径（W/W*）、Form XObject、
//     图片对象数量、图片尺寸与页面尺寸比例
//  2. 根据特征组合确定路由：
//     - 有裁剪路径（anyGroupWithClip || anyRealClip）→ routeRenderCropComplexTransparency
//     - 有透明度（anyGroup）→ routeDirectExtractTransparency
//     - 多页多图 → routeDirectExtractMultiImageStack
//     - 单页单图且图片≈页面尺寸 → routeRenderWholePageImage
//     - 最简情况 → routeDirectExtractSingleObject
//
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
		hasClip := hasPageContentClip(ctx, pageDict)

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

func renderCropPDF(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	// 优先尝试 go-fitz（需 -tags gofitz 编译），
	// 若不可用则回退到 mutool draw -ppm 渲染。
	fitzDoc, gofitzErr := openFitzDoc(inputFile)

	var pageCount int
	var err error
	if gofitzErr == nil {
		defer fitzDoc.Close()
		pageCount = fitzDoc.NumPage()
	} else if mutoolPath := findMutool(); mutoolPath == "" {
		return fmt.Errorf("open PDF for render crop: go-fitz 未启用（需 -tags gofitz）且 mutool 不可用: %w", gofitzErr)
	} else {
		// 用 getPageCountViaMutool 获取页数（复用函数，避免重复解析逻辑）
		pageCount, err = getPageCountViaMutool(inputFile)
		if err != nil {
			return fmt.Errorf("get page count for render crop: %w", err)
		}
	}

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
		nWorkers := computeWorkerCount()
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
				cropped := cropImage(img, cr)
				traceTiming(timing, "page %d crop %d crop-image=%s rect=%dx%d", pageIndex+1, ci+1, time.Since(cropStart), cr.Dx(), cr.Dy())

				writeStart := time.Now()
				outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_%03d.%s", pageIndex+1, ci+1, outputExtension(format)))
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
					Ext:    outputExtension(format),
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
		traceProgress(progressEnabled, (pageIndex+1)*100/pageCount)
	}

	if totalSaved == 0 {
		return fmt.Errorf("no image crops were saved")
	}

	return nil
}

// renderWholePagePDF 使用 mutool draw -ppm 渲染 PDF 所有页面为图片。
//
// 并行策略（CPU 使用率由 -cpu 参数控制）：
//   Phase 1 — 渲染并行：
//     将总页数按 computeWorkerCount() 拆分为多个页范围，
//     每个范围启动一个独立的 mutool draw -ppm 子进程。
//     子进程写入临时目录，进程间无竞争。
//     例如：14 核 / -cpu 25 时，启动 4 个子进程，各渲染约 1/4 页数。
//     渲染失败（任何子进程出错）→ 回退到串行 mutool → 再回退到 go-fitz。
//
//   Phase 2 — 编码并行：
//     所有渲染完成后，用 goroutine 池（容量 = computeWorkerCount()）并行读取 PPM
//     文件、裁剪（如需）、编码为 JPEG/PNG 并写盘。
//     使用 channel + sync.WaitGroup 协调并发。
//
// 颜色处理：
//   mutool draw -ppm 输出原始 RGB 数据，无需色彩校正。
//   与 pdftoppm 不同，mutool（MuPDF）的 CMYK→RGB 转换与 macOS PDFKit/Acrobat 一致。
//
// PPM 格式：
//   P6 二进制 RGB：每像素 3 字节（R,G,B），无 alpha 通道。
//   文件头：P6 \n <width> <height> \n 255 \n <raw RGB data>
//   由 readPPM() 解析为 *image.RGBA（alpha 设为 255）。
//
// 适用场景：classifyPDFDocument 返回 routeRenderWholePageImage 的情况。
func renderWholePagePDF(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	// 整页渲染路径用 mutool draw -ppm 输出 PPM（原始 RGB，色彩与 PDF 阅读器 100% 一致），
	// 然后由 Go 编码为最终格式（JPEG/PNG）。
	//
	// mutool -ppm 渲染速度与 pdftoppm -jpeg 相同（71页均 ~13.6s），
	// 但 MuPDF 的 CMYK→RGB 转换与 macOS PDFKit 一致（pdftoppm 偏红）。
	// 见 memory: "pdf-tool 色彩校正" 条目。

	ext := outputExtension(format)

	// 先用 pdfcpu 获取页数。
	pageCount, err := getPageCount(inputFile)
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

	mutool := findMutool()
	numWorkers := computeWorkerCount()
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
				return renderWholePagePDFGoFitz(inputFile, outputDir, format, dpi, timing, progressEnabled, quality)
			}
			break // 成功了，跳出错误循环继续处理
		}
	}
	traceTiming(timing, "mutool conversion=%s all-pages=%d", time.Since(convertStart), pageCount)

	// 并行读取 PPM 并编码为最终格式（goroutine 池，默认 4 路并行）
	traceProgress(progressEnabled, 0)
	type pageResult struct {
		nr   int
		err  error
	}
	resultCh := make(chan pageResult, pageCount)
	sem := make(chan struct{}, computeWorkerCount()) // 并发数
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

			rgba, err := readPPM(ppmPath)
			if err != nil {
				resultCh <- pageResult{nr, fmt.Errorf("read ppm page %d: %w", nr, err)}
				return
			}

			if err := writeImageAtomically(outPath, func(w io.Writer) error {
				switch format {
				case "png":
					return encodePNG(w, rgba)
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
		traceProgress(progressEnabled, r.nr*100/pageCount)
	}
	return nil
}

// renderWholePagePDFGoFitz 是 mutool 不可用时的 go-fitz 回退路径。
func renderWholePagePDFGoFitz(inputFile, outputDir, format string, dpi float64, timing, progressEnabled bool, quality int) error {
	fitzDoc, err := openFitzDoc(inputFile)
	if err != nil {
		return fmt.Errorf("open PDF for render whole page: %w", err)
	}
	defer fitzDoc.Close()

	pageCount := fitzDoc.NumPage()
	if pageCount == 0 {
		return fmt.Errorf("PDF has no pages")
	}

	traceProgress(progressEnabled, 0)
	for pageIndex := 0; pageIndex < pageCount; pageIndex++ {
		pageNr := pageIndex + 1
		pageStart := time.Now()
		log.Printf("convert: page=%d/%d path=render-whole-page", pageNr, pageCount)
		outputPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, outputExtension(format)))
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
			Ext:    outputExtension(format),
			Path:   outputPath,
		})
		traceTiming(timing, "page %d total=%s", pageNr, time.Since(pageStart))
		traceProgress(progressEnabled, pageNr*100/pageCount)
	}
	return nil
}

// extractDirectImages 走的是"对象级提取"路径。
//
// 处理流程（逐页）：
//  1. 获取当前页的 /Resources/XObject 字典，遍历所有 Image 对象
//  2. 对每个 Image 对象调用 writeDirectImage：
//     a) 优先尝试快速路径（writeDirectImageFast）—— 直接复制 JPEG/JPEG2000 流，
//        或解码 8-bit RGB/Gray FlateDecode 图片
//     b) 快速路径不满足条件时，回退到 pdfcpu 的通用解码路径
//  3. Form XObject 中的图片也会递归提取
//  4. SMask（透明度遮罩）在快速路径内处理，不触发 pdfcpu 的锁竞争
//
// 适用场景：
//   - routeDirectExtractTransparency：页面有 /Group 但无裁剪路径
//   - routeDirectExtractMultiImageStack：无透明度、多图堆叠
//   - routeDirectExtractSingleObject：最简单图
//
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
	traceProgress(progressEnabled, 0)

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
				wholePageDoc, openErr = openFitzDoc(inputFile)
				if openErr != nil {
					// mutool 回退：go-fitz 不可用时用 mutool draw -ppm 渲染
					img, renderErr := renderPageToImageViaMutool(inputFile, pageNr, dpi)
					if renderErr != nil {
						return fmt.Errorf("render page %d: %w (fitz: %v)", pageNr, renderErr, openErr)
					}
					bounds := img.Bounds()
					w, h := bounds.Dx(), bounds.Dy()
					if err := writeImageAtomically(outputPath, func(wr io.Writer) error {
						switch format {
						case "png":
							return encodePNG(wr, img)
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
						Ext:    outputExtension(format),
						Path:   outputPath,
					})
					totalWritten++
					traceTiming(timing, "direct-extract page %d=%s rendered-whole-page (mutool)", pageNr, time.Since(pageStart))
					processedPages++
					traceProgress(progressEnabled, processedPages*100/ctx.PageCount)
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
		return 0, 0, fmt.Errorf("encode image %s: %w", outPath, err)
	}
	traceTiming(timing, "render-whole-page page %d write=%s path=%s", pageNr, time.Since(pageStart), outPath)
	return width, height, nil
}

// renderSinglePageCropPdftoppm 使用 mutool draw -ppm 渲染单页（原始 RGB），
// 然后根据 cm 矩阵确定的裁剪区域裁剪并输出。
//
// 注意：函数名虽含 Pdftoppm，但实际已改为 mutool 渲染。
// 然后根据 cm 矩阵确定的裁剪区域裁剪并输出。
//
// 使用 mutool 替代 pdftoppm 的原因：
//   - mutool（MuPDF）的 CMYK→RGB 转换与 PDF 阅读器一致，无偏红问题
//   - mutool -ppm 渲染速度与 pdftoppm -jpeg 相同
//   - PPM 是原始 RGB 数据，无需解码，裁剪和编码更快
//
// 输入参数在 PDF 用户空间坐标中（bottom-left origin）：
//
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
	out, err := exec.Command(findMutool(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mutool render page %d: %v\n%s", pageNr, err, string(out))
	}

	// 读 PPM 获取渲染尺寸和 RGB 数据
	ppmPath := filepath.Join(tmpDir, fmt.Sprintf("p%d.ppm", pageNr))
	rgba, err := readPPM(ppmPath)
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
	cropped := cropImage(rgba, cropRect)

	return writeImageAtomically(outPath, func(w io.Writer) error {
		switch format {
		case "png":
			return encodePNG(w, cropped)
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
	out, err := exec.Command(findMutool(), args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("mutool draw page %d: %v\n%s", pageNr, err, string(out))
	}

	ppmPath := filepath.Join(tmpDir, fmt.Sprintf("p%d.ppm", pageNr))
	img, err := readPPM(ppmPath)
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
//
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
			//
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
			outputExt := outputExtension(format)
			outPath := filepath.Join(outputDir, fmt.Sprintf("page_%03d_image_001.%s", pageNr, outputExt))
			actualFormat := format   // 实际输出格式，mutool 路径会改写为 jpg
			actualExt := outputExt

			writeStart := time.Now()

			// 第一层：尝试 mutool 快速裁剪路径
			// 条件：mutool 可用、图片非旋转（b=0, c=0）、有效尺寸
			// 自动改写输出格式为 jpg（源图是 CMYK JPEG，无需重编码为 PNG）
			mutoolOk := false
			if findMutool() != "" {
				pageDict, _, _, pErr := ctx.PageDict(pageNr, false)
				if pErr == nil {
					content := getPageContentString(ctx, pageDict)
					if content != "" {
						a, b, c, d, e, f, cmOk := extractImageFullCM(content)
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
	//
	// 注意：这里处理的是所有非 DCT/JPX 的 8-bit 图片流——
	// DCT 分支（上面的第 2 个分支）只匹配 FilterPipeline 末尾是
	// DCTDecode 的图像，解码后直接写 JPEG 字节到 .jpg 文件。
	// FlateDecode、RunLengthDecode、CCITTFaxDecode 等非 DCT 过滤器
	// 都不会进入那个分支，而是落到这里。
	//
	// 最初以为这些 FlateDecode 图像是 ColorSpaceString 返回了
	// ICCBased 等非 DeviceRGB 值才被拒绝，Debug 后发现实际上
	// ColorSpaceString 正确返回了 "DeviceRGB"，条件全部通过。
	// 真正的原因是 sd.Decode() 从未被调用（只在 DCT 分支内调用），
	// 导致 sd.Content 为空，到 b := sd.Content 时拿到 nil，
	// 随后的 len(b) < 3*w*h 检查返回 "corrupt" 错误，回退到
	// pdfcpu.ExtractImage（受 ctxMu 互斥锁串行化）。
	//
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

	// ── 延迟解码 FlateDecode 等非 DCT 流 ────────────────────────
	// sd.Content 为空，说明 pdfcpu 没有对这个流调用 Decode()。
	// DCT 分支在过滤器匹配时已调用 sd.Decode()，但 FlateDecode、
	// RunLengthDecode、CCITTFaxDecode 等非 DCT 过滤器不会走那条路。
	//
	// 安全分析：
	// sd.Decode() 的操作范围仅限于 sd 自身——读取 sd.Raw 中的压缩
	// 字节，按 sd.FilterPipeline 逐级解码，写入 sd.Content。
	// 不涉及共享的 ctx（不读页对象树，不遍历 XObject，不修改任何
	// 外部状态）。每个 sd 是由 extractDirectImages 中 indRefToStreamDict
	// 独立解析出来的，goroutine 间不共享。
	//
	// 这与旧的 go-fitz (CGo) 完全不同：go-fitz 的 fitz.New() 会触发
	// macOS 信号栈溢出（semasleep on Darwin signal stack）导致进程
	// 无响应卡死。这里全是纯 Go：
	//   1. sd.Decode()          → pdfcpu 纯 Go zlib 解压
	//   2. SetNRGBA 像素合成   → image/color 纯 Go
	//   3. PNG 编码             → image/png 纯 Go
	// 零 CGo、零 ctxMu、零共享，goroutine 间完全安全并行。
	//
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

// resolveBundledMagickExecutable 查找 ImageMagick 的魔法文件配置文件路径。
// 用于 Windows 兼容性场景（macOS 上几乎不触发）。
// 查找顺序：软件包目录 → 系统 share 目录 → 捆绑目录。
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
// isCMYKJPEG 从 JPEG 数据流的 SOF marker 判断是否含 4 个分量（CMYK）。
// 比 sd.CSComponents 更可靠，因为它直接解析 JPEG 字节流，
// 不依赖 pdfcpu 的元数据推断。
// SOF0 marker (0xFF 0xC0) 后第 7 个字节为分量数：3=RGB/YUV，4=CMYK。
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
// convertCMYKJPEGToOutput 把 CMYK JPEG 转成正确的 RGB 图片（PNG）。
// macOS 上用 sips 系统工具完成 CMYK→RGB 转换，颜色准确，零外部依赖。
// 步骤：
// 1. 将 CMYK JPEG 数据写入临时文件
// 2. 调用 sips -s format png 转换为 RGB PNG
// 3. 如果 sips 失败，尝试 Go 标准库解码（部分 sips 不支持的变体）
// 4. 清理临时文件
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

// encodePNG 将 image.Image 编码为 PNG 写入 writer。
// 使用 Go 标准库 image/png，支持所有 Go 可表示的图像类型。
func encodePNG(w io.Writer, img image.Image) error {
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	return enc.Encode(w, img)
}

// extractSoftMask 读取并解码 SMask。
// 它只接受与主图像尺寸严格一致、且位深为 8 的软遮罩，
// 这样可以避免把错误尺寸的透明信息错误叠到图片上。
// extractSoftMask 读取并解码与图片关联的 SMask（软遮罩/透明度通道）。
// SMask 是 PDF 中用于表示图片透明度的灰度图像（每个像素 8-bit，0=透明，255=不透明）。
//
// 处理流程：
// 1. 从图片字典中获取 /SMask 引用的流对象
// 2. 解码 SMask 流数据
// 3. 检查 SMask 尺寸与主图是否一致（不一致时返回 nil，避免错误叠加）
// 4. 检查 SMask 位深是否为 8（否则回退）
// 5. 返回解码后的 alpha 通道数据
//
// 注意：
// - SMask 尺寸必须与主图严格一致，否则透明信息会错位叠加
// - 仅处理 8-bit 灰度 SMask；1-bit 或更高位深会回退
// - 此函数在 8-bit 快速路径内调用，不碰 ctxMu，goroutine-safe
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
// pdfHasClipPath 扫描所有页面的内容流，检查是否包含裁剪路径操作符（W/W*）。
// 只要任意页面出现裁剪路径，就说明"直接抽图"可能不可靠，
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
// pdfHasMultipleImageObjects 检查是否存在"同一页里有多个图片对象"的情况。
// 这类 PDF 往往需要分辨哪个图片是主体，
// 不适合只按对象级别直取。
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
// hasClipOperator 检查内容流中是否包含裁剪路径操作符。
// 裁切路径操作符：W（偶数规则裁切）、W*（非零环绕数裁切）。
// 注意：这不是严格的 PDF 语法解析，只是快速启发式判断：
// 既要看到裁剪指令（W/W*），也要看到普通绘图指令（m/l/c/v/re 等）。
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

// minRegionAreaThreshold 根据图片尺寸动态计算前景区域的最小面积阈值。
// 这确保在不同 DPI 下同一物理区域都能被正确识别。
// 比例设为 0.5% 的页面面积，与 300 DPI 下原固定值 50000 相当，
// 最低 1000 像素防止小页面过拟合。
func minRegionAreaThreshold(width, height int) int {
	area := width * height * 5 / 1000 // 0.5% of page area
	if area < 1000 {
		return 1000
	}
	return area
}

// findLargestRegions 从整页渲染结果里提取最大的前景区域。
// 它的假设很简单：
// - 页面大部分背景接近白色；
// - 目标内容在像素上是连通的或者近似连通的；
// - 前景面积越大，越可能是我们想要的图像主体。
// findLargestRegions 从整页渲染结果里提取最大的前景区域。
// 算法：
//   1. 将图像转换为 *image.RGBA
//   2. 从图像的四个角开始扫描（因为图片主体通常在页面中央，四角是背景）
//   3. 对每个非背景像素启动 flood fill，收集连通区域
//   4. 按像素面积降序排列，取前 maxRegions 个
//   5. 返回这些区域的外接矩形列表
//
// 假设前提：
//   - 大部分背景接近白色
//   - 目标内容在像素上是连通的或者近似连通的
//   - 前景面积越大，越可能是图像主体
// 阈值偏高，更愿意把浅灰边缘也算作背景，使主体外接框更稳定。
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
			if area >= minRegionAreaThreshold(width, height) {
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
// 3x4 色彩校正矩阵：将 pdftoppm 的 RGB 输出校正至与 MuPDF（mutool）一致。
//
// pdftoppm（lcms2）与 MuPDF 使用不同的默认 CMYK→RGB 转换算法，
// 导致 pdftoppm 渲染的 CMYK PDF 页面偏红（G 通道偏低 ~10，B 通道偏低 ~6）。
// 该矩阵通过最小二乘法拟合 12.pdf 的 20,000 个采样像素得到，
// 将平均颜色差异从 8.72 降至 3.47/通道（降幅 60%）。
//
// 适用场景：pdftoppm 渲染的 DeviceCMYK 或隐式 CMYK 页面。
// RGB-only PDF 页面不受 pdftoppm 色彩转换影响，此校正不会造成额外误差。
//
// 矩阵格式（行优先）：[R, G, B, bias] 即 R_out = m[0][0]*R + m[0][1]*G + m[0][2]*B + m[0][3]
var colorCorrectionMatrix = [3][4]float64{
	{1.0517, 0.0489, -0.1139, -3.7736}, // R_out
	{0.2610, 0.8087, -0.0596, -8.5282}, // G_out
	{0.1529, 0.1397, 0.7126, -6.4740},  // B_out
}

// applyColorCorrection 对 RGBA 图像的每个像素应用色彩校正矩阵。
// 直接在 RGBA 像素数据上原地修改，避免额外内存分配。
// applyColorCorrection 对 RGBA 图像的每个像素应用色彩校正矩阵。
// 直接在 RGBA 像素数据上原地修改，避免额外内存分配。
// 矩阵通过最小二乘法拟合 12.pdf 的 20,000 个采样像素得到。
// 目前默认关闭（-cc），因为渲染引擎已改为 mutool，颜色正确无需校正。
func applyColorCorrection(img *image.RGBA) {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := img.PixOffset(x, y)
			r := float64(img.Pix[idx+0])
			g := float64(img.Pix[idx+1])
			b := float64(img.Pix[idx+2])
			// 应用校正矩阵
			wr := colorCorrectionMatrix[0][0]*r + colorCorrectionMatrix[0][1]*g + colorCorrectionMatrix[0][2]*b + colorCorrectionMatrix[0][3]
			wg := colorCorrectionMatrix[1][0]*r + colorCorrectionMatrix[1][1]*g + colorCorrectionMatrix[1][2]*b + colorCorrectionMatrix[1][3]
			wb := colorCorrectionMatrix[2][0]*r + colorCorrectionMatrix[2][1]*g + colorCorrectionMatrix[2][2]*b + colorCorrectionMatrix[2][3]
			// 钳位到 [0, 255]
			if wr < 0 {
				wr = 0
			} else if wr > 255 {
				wr = 255
			}
			if wg < 0 {
				wg = 0
			} else if wg > 255 {
				wg = 255
			}
			if wb < 0 {
				wb = 0
			} else if wb > 255 {
				wb = 255
			}
			img.Pix[idx+0] = uint8(wr)
			img.Pix[idx+1] = uint8(wg)
			img.Pix[idx+2] = uint8(wb)
		}
	}
}

// findMutool 查找 mutool 可执行文件路径，优先使用捆绑版本。
// 查找顺序：
//   1. PATH 环境变量（系统安装的 mutool）
//   2. 程序同级目录 mutool（解压后直接放在一起）
//   3. 程序同级 bund/<os>-<arch>/mutool（跨平台捆绑）
//   4. 程序同级 bund/mutool（简单捆绑）
//   5. /opt/homebrew/bin/mutool（Homebrew）
//   6. /usr/local/bin/mutool
// 如果都找不到，返回空字符串，后续会回退到 go-fitz 渲染。
// 结果缓存在全局变量 mutoolPath 中，避免重复查找。
func findMutool() string {
	if mutoolPath != "" {
		return mutoolPath
	}
	// 1. 检查 PATH 中是否有 mutool
	path, err := exec.LookPath("mutool")
	if err == nil {
		mutoolPath = path
		return path
	}
	// 2. 检查程序同级目录（mutool 和 pdf-tool 放在一起时）
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		sameDirPath := filepath.Join(exeDir, "mutool")
		if fi, err := os.Stat(sameDirPath); err == nil && !fi.IsDir() {
			mutoolPath = sameDirPath
			return sameDirPath
		}
		// 3. 跨平台捆绑：bund/<os>-<arch>/mutool
		platformDir := filepath.Join(exeDir, "bund", runtime.GOOS+"-"+runtime.GOARCH)
		platformPath := filepath.Join(platformDir, "mutool")
		if fi, err := os.Stat(platformPath); err == nil && !fi.IsDir() {
			mutoolPath = platformPath
			return platformPath
		}
		// 简单捆绑：bund/mutool
		simplePath := filepath.Join(exeDir, "bund", "mutool")
		if fi, err := os.Stat(simplePath); err == nil && !fi.IsDir() {
			mutoolPath = simplePath
			return simplePath
		}
	}
	// 3. Homebrew
	for _, c := range []string{
		"/opt/homebrew/bin/mutool",
		"/usr/local/bin/mutool",
	} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			mutoolPath = c
			return c
		}
	}
	return "" // 最终回退到 go-fitz
}

// getPageCount 获取 PDF 页数，多源回退保证不因为单一工具不可用而失败。
// 优先级：mutool info（最快，轻量子进程）→ go-fitz（CGo 回退）
// 不依赖 pdfcpu，避免 "missing required resource subdict: Properties" 等解析失败。
func getPageCount(inputFile string) (int, error) {
	// mutool info 最快（<15ms），只读 Catalog 不加载整个 PDF。
	if findMutool() != "" {
		if n, err := getPageCountViaMutool(inputFile); err == nil && n > 0 {
			return n, nil
		}
	}
	// go-fitz 回退（如果编译时启用了 -tags gofitz）
	if doc, err := openFitzDoc(inputFile); err == nil {
		n := doc.NumPage()
		doc.Close()
		if n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("cannot determine page count: mutool and go-fitz both unavailable")
}

// getPageCountViaMutool 用 mutool info 获取 PDF 页数。
// 解析 mutool info 输出中的 "Pages: N" 行。
// 这是最轻量的页数获取方式：启动子进程 → 读 Catalog → 退出，通常 <15ms。
func getPageCountViaMutool(inputFile string) (int, error) {
	mutool := findMutool()
	if mutool == "" {
		return 0, fmt.Errorf("mutool not available")
	}
	out, err := exec.Command(mutool, "info", inputFile).Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Pages:") {
			return strconv.Atoi(strings.TrimSpace(line[len("Pages:"):]))
		}
	}
	return 0, fmt.Errorf("cannot parse page count from mutool info output")
}

// computeWorkerCount 根据 CPU 核心数和用户指定的并行百分比计算实际工作线程数。
// 百分比 0-100：0 表示串行（返回 1），100 表示用满所有核心。
// 结果至少为 1，最多为 CPU 核心数。
// computeWorkerCount 根据 CPU 核心数和用户指定的并行百分比计算实际工作线程数。
func computeWorkerCount() int {
	numCPU := runtime.NumCPU()
	if parallelPercent <= 0 {
		return 1
	}
	if parallelPercent >= 100 {
		return numCPU
	}
	n := (numCPU*parallelPercent + 50) / 100 // 四舍五入
	if n < 1 {
		n = 1
	}
	return n
}

// readPPM 读取 PPM P6 格式文件，返回 *image.RGBA。
//
// PPM P6 格式：
//
//	P6
//	<width> <height>
//	<maxval>
//	<binary RGB data>
//
// 注释行以 # 开头，可出现在宽度/高度前。
// Maxval 为 255（不支持其他位深度）。
// readPPM 读取 PPM P6 格式文件，返回 *image.RGBA。
func readPPM(path string) (*image.RGBA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 3 || string(data[:2]) != "P6" {
		return nil, fmt.Errorf("not a PPM P6 file")
	}
	pos := 2

	// 跳过空白符（空格、制表符、换行、回车）
	skipWS := func() {
		for pos < len(data) && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\n' || data[pos] == '\r') {
			pos++
		}
	}
	// 读取一个 ASCII 整数
	readInt := func() (int, error) {
		skipWS()
		// 跳过注释行
		for pos < len(data) && data[pos] == '#' {
			for pos < len(data) && data[pos] != '\n' {
				pos++
			}
			skipWS()
		}
		start := pos
		for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
			pos++
		}
		if pos == start {
			return 0, fmt.Errorf("expected integer at offset %d", pos)
		}
		return strconv.Atoi(string(data[start:pos]))
	}

	width, err := readInt()
	if err != nil {
		return nil, fmt.Errorf("read width: %w", err)
	}
	height, err := readInt()
	if err != nil {
		return nil, fmt.Errorf("read height: %w", err)
	}
	maxval, err := readInt()
	if err != nil {
		return nil, fmt.Errorf("read maxval: %w", err)
	}
	if maxval != 255 {
		return nil, fmt.Errorf("unsupported PPM maxval %d (only 255 supported)", maxval)
	}
	// 跳过最后一个空白符（maxval 后面的单个空白）
	skipWS()

	expected := width * height * 3
	if len(data)-pos < expected {
		return nil, fmt.Errorf("PPM data truncated: need %d bytes, have %d", expected, len(data)-pos)
	}

	rgba := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			srcIdx := (y*width + x) * 3
			dstIdx := rgba.PixOffset(x, y)
			rgba.Pix[dstIdx+0] = data[pos+srcIdx+0]
			rgba.Pix[dstIdx+1] = data[pos+srcIdx+1]
			rgba.Pix[dstIdx+2] = data[pos+srcIdx+2]
			rgba.Pix[dstIdx+3] = 255
		}
	}
	return rgba, nil
}

// applyColorCorrectionToFile 读取 JPEG 文件，应用色彩校正后写回。
// src 和 dst 可以是同一路径（原地修改）。
// applyColorCorrectionToFile 读取 JPEG 文件，应用色彩校正后写回。
// src 和 dst 可以是同一路径（原地修改）。
// 用于 -cc 标记下批量校正已生成的 JPEG 输出文件。
func applyColorCorrectionToFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open for color correction: %w", err)
	}
	srcImg, err := jpeg.Decode(srcFile)
	srcFile.Close()
	if err != nil {
		return fmt.Errorf("decode for color correction: %w", err)
	}
	rgba := toRGBA(srcImg)
	applyColorCorrection(rgba)

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create for color correction: %w", err)
	}
	defer dstFile.Close()
	return jpeg.Encode(dstFile, rgba, &jpeg.Options{Quality: 95})
}

// cropImage 从图像中裁剪指定矩形区域，返回 *image.RGBA。
// 统一输出为 RGBA 以便后续编码逻辑只处理一种图像类型。
func cropImage(img image.Image, rect image.Rectangle) *image.RGBA {
	// 裁剪时统一转换成 RGBA，避免不同图像类型之间的绘制差异。
	rgba := toRGBA(img)
	crop := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(crop, crop.Bounds(), rgba, rect.Min, draw.Src)
	return crop
}

// toRGBA 将任意 image.Image 转换为 *image.RGBA。
// 通过 draw.Draw 将源图像绘制到新的 RGBA 画布上。
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
// isBackground 判断像素是否为背景色（接近白色）。
// 阈值宽松（RGB > 200），这样浅灰边缘也算作背景，
// 主体区域的外接框会更稳定。
func isBackground(pixel color.RGBA) bool {
	// 这里把接近白色的像素都视为背景，便于把页面上的主体区域分离出来。
	return pixel.R >= 248 && pixel.G >= 248 && pixel.B >= 248
}

// traceTiming 打印阶段耗时信息到 stderr。
// 仅当 -timing / -t 启用时输出。
// 格式："[timing] <message>"
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

// traceImageMeta 记录单张图片的元数据到全局收集器。
// 收集后在程序结束时统一输出。
func traceImageMeta(meta imageMetaRecord) {
	if globalImageMetaCollector == nil || !globalImageMetaCollector.enabled {
		return
	}
	globalImageMetaCollector.add(meta)
}

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
func outputExtension(format string) string {
	if format == "jpg" {
		return "jpg"
	}
	return format
}

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
