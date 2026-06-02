// pdf-tool — PDF 图片提取与合并工具。
//
// 核心功能：
//  1. 从 PDF 中提取嵌入的图片（直取路径）：优先从对象流中直接复制 JPEG/JPEG2000
//     或快速解码 8-bit RGB/Gray 图片，复杂格式回退到 pdfcpu 通用解码。
//  2. 渲染后裁剪（渲染路径）：对包含裁剪路径、透明度、图层混合等复杂效果的 PDF，
//     先用 mutool draw -ppm 渲染整页，再通过连通域分析裁出图片主体。
//  3. 多 PDF 合并：利用 pdfcpu 的 MergeCreateFile 合并多个 PDF 文件。
//  4. 替换模式：根据 JSON 配置替换模板 PDF 中的灯位图片。
//
// 渲染引擎：
//   - 主渲染引擎：mutool draw -ppm（MuPDF），颜色与 PDF 阅读器 100% 一致
//   - 回退引擎：go-fitz（仅在 mutool 完全不可用时触发）
//
// 并行策略：
//   - mutool 渲染：按页范围拆分为多个独立子进程并行执行
//   - PPM→JPEG 编码：使用 goroutine 池并行编码
//   - 并行度由 -cpu 参数控制（0-100%，默认 25），实际线程数 = CPU核心数 * 百分比
//
// 路由策略：
//   - 任何页面有 /Group → 检查是否有裁剪路径（hasPageContentClip）
//   - 有裁剪路径且 cm_a/clip_w 比例 > 1.05 → render-crop（渲染后裁剪）
//   - 无 Group + 有裁剪路径（比例 > 1.05）→ render-crop
//   - 其余 → direct-extract（直取）
//
// 输出格式：
//   - JPEG（-f jpg）：默认 85% 质量，CMYK JPEG 自动识别并正确转换
//   - PNG（-f png）：仅当指定时输出
//   - 自动格式判断：CMYK JPEG 场景自动输出 jpg，避免重编码
//
// 8-bit 快速路径：
//   - FlateDecode 编码的 8-bit RGB/Gray 图片会走 sd.Decode() 快速解码
//   - 绕过 pdfcpu 的通用 ExtractImage 路径（含 SMask 合成的锁竞争）
//   - SMask 处理在快速路径内完成，不碰 ctxMu，goroutine-safe
//
// 版本：v2.0+（基于 mutool 渲染引擎）
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"strings"

	"example.com/pdf-tool/cmd"
	"example.com/pdf-tool/util"
)

var debugLogsEnabled bool
var imageMetaEnabled bool
var colorCorrectionEnabled bool

// parallelPercent 用户指定的并行百分比（0-100），默认 50。
// 实际工作线程数由 computeWorkerCount() 根据 CPU 核心数计算。
var parallelPercent int

func main() {
	inputFile := flag.String("i", "input.pdf", "输入 PDF 文件")
	outputDir := flag.String("o", "output", "输出目录或合并后的 PDF")
	format := flag.String("f", "png", "输出图片格式：png 或 jpg")
	dpi := flag.Float64("dpi", 300, "渲染 DPI")
	mergeEnabled := flag.Bool("merge", false, "合并 PDF 文件")
	mergeInputDir := flag.String("merge-dir", "", "待合并 PDF 所在目录")
	mergeInputList := flag.String("merge-inputs", "", "逗号分隔的待合并 PDF 文件列表")
	mergeGlob := flag.String("merge-glob", "*.pdf", "合并模式下的文件匹配模式")
	mergeChunkSize := flag.Int("merge-chunk-size", 50, "合并模式下每批处理的 PDF 数量")
	mergeDivider := flag.Bool("merge-divider", false, "在合并文件之间插入分隔页")
	compressEnabled := flag.Bool("compress", false, "压缩 PDF 文件")
	compressPreset := flag.String("compress-preset", "prepress", "压缩预设: screen(72dpi)/ebook(150dpi)/printer(300dpi)/prepress/ high(不降采样)")
	compressResolution := flag.Int("compress-resolution", 1200, "覆盖预设的降采样 DPI（0=使用预设默认值）")
	compressJPEGQ := flag.Int("compress-jpegq", 95, "压缩 JPEG 质量 1-100（默认 95）")
	compressDir := flag.String("compress-dir", "", "压缩目录下所有 PDF 文件")
	mergeCompress := flag.Bool("merge-compress", true, "合并前先压缩每个 PDF 文件（默认开启）")
	progressEnabled := flag.Bool("p", false, "打印合并与压缩进度 0-100")
	logEnabled := flag.Bool("l", false, "打印调试日志")
	flag.BoolVar(logEnabled, "log", false, "打印调试日志")
	metaEnabled := flag.Bool("m", false, "打印图片宽高信息")
	flag.BoolVar(metaEnabled, "meta", false, "打印图片宽高信息")
	util.ImageMetaJSONEnabled = false
	metaJSONEnabled := flag.Bool("m-json", false, "以 JSON 形式打印图片宽高信息")
	flag.BoolVar(metaJSONEnabled, "meta-json", false, "以 JSON 形式打印图片宽高信息")
	timing := flag.Bool("t", false, "打印每个阶段的耗时信息")
	flag.BoolVar(timing, "timing", false, "打印每个阶段的耗时信息")
	quality := flag.Int("q", 85, "JPEG 编码质量 1-100（默认 85）")
	flag.IntVar(quality, "quality", 85, "JPEG 编码质量 1-100（默认 85）")
	colorCorrection := flag.Bool("cc", false, "对 pdftoppm 渲染结果应用色彩校正（已弃用的选项，当前引擎为 mutool 无需校正）")
	parallelPct := flag.Int("cpu", 25, "CPU 使用率百分比 0-100")
	// replace 模式参数
	replaceEnabled := flag.Bool("replace", false, "替换模式：根据 JSON 配置替换模板 PDF 中的灯位图片")
	replaceJSON := flag.String("replace-json", "", "替换模式：JSON 配置文件路径")
	replaceOutput := flag.String("replace-output", "", "替换模式：输出 PDF 路径")
	replaceFont := flag.String("replace-font", "", "替换模式：中文字体文件路径（TTF）")
	replaceBaseDir := flag.String("replace-base-dir", "", "替换模式：模板 PDF 基础目录（默认用户数据目录）")
	replaceDir := flag.String("replace-dir", "", "替换模式：批量合成目录（扫描所有 *.json 文件依次处理）")
	flag.Usage = func() {
		numCPU := runtime.NumCPU()
		fmt.Fprintf(flag.CommandLine.Output(), "用法：%s [参数]\n", os.Args[0])
		flag.PrintDefaults()
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
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -cpu 50   # 使用 50% CPU")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -i input.pdf -o output -cpu 100  # 用满所有 CPU 核心")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 合并 PDF")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -merge-chunk-size 20")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf -merge-divider")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 合并前压缩（默认开启）")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -merge-compress=true -compress-preset ebook")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -merge-compress=false")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 压缩 PDF")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -compress -i input.pdf -o output.pdf")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -compress -compress-dir /path/to/pdfs -o /output/dir")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -compress -i input.pdf -o output.pdf -compress-preset ebook")
		fmt.Fprintln(flag.CommandLine.Output(), "")
		fmt.Fprintln(flag.CommandLine.Output(), "  # 替换模式")
		fmt.Fprintln(flag.CommandLine.Output(), "  ./pdf-tool -replace -replace-json config.json -replace-output result.pdf")
	}
	flag.Parse()

	debugLogsEnabled = *logEnabled
	imageMetaEnabled = *metaEnabled
	util.ImageMetaJSONEnabled = *metaJSONEnabled
	colorCorrectionEnabled = *colorCorrection
	parallelPercent = *parallelPct

	if debugLogsEnabled {
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(io.Discard)
	}
	log.SetFlags(0)

	if *mergeEnabled {
		if err := cmd.RunMerge(*mergeInputDir, *mergeInputList, *mergeGlob, *outputDir, *mergeChunkSize, *mergeDivider, *progressEnabled, *mergeCompress, *compressPreset, *compressJPEGQ, *compressResolution); err != nil {
			fmt.Fprintf(os.Stderr, "PDF 合并失败：%v\n", err)
			os.Exit(1)
		}
		return
	}

	if *replaceEnabled {
		if *replaceDir != "" {
			if err := cmd.RunReplaceDir(*replaceDir, *replaceOutput, *parallelPct); err != nil {
				fmt.Fprintf(os.Stderr, "批量合成失败：%v\n", err)
				os.Exit(1)
			}
		} else {
			if err := cmd.Run(*replaceJSON, *replaceOutput, *replaceFont, *replaceBaseDir, *parallelPct); err != nil {
				fmt.Fprintf(os.Stderr, "替换失败：%v\n", err)
				os.Exit(1)
			}
		}
		return
	}

	if *compressEnabled {
		dir := strings.TrimSpace(*compressDir)
		if dir != "" {
			if err := cmd.RunCompress(dir, *outputDir, true, *compressPreset, *compressJPEGQ, *compressResolution); err != nil {
				fmt.Fprintf(os.Stderr, "PDF 目录压缩失败：%v\n", err)
				os.Exit(1)
			}
		} else {
			if err := cmd.RunCompress(*inputFile, *outputDir, false, *compressPreset, *compressJPEGQ, *compressResolution); err != nil {
				fmt.Fprintf(os.Stderr, "PDF 压缩失败：%v\n", err)
				os.Exit(1)
			}
		}
		return
	}

	// 默认：提取图片
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败：%v\n", err)
		os.Exit(1)
	}
	if err := cmd.RunExtract(*inputFile, *outputDir, *format, *dpi, *timing, *progressEnabled, *quality, parallelPercent, *colorCorrection); err != nil {
		fmt.Fprintf(os.Stderr, "PDF 转图片失败：%v\n", err)
		os.Exit(1)
	}
}