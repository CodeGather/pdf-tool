package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"example.com/pdf-tool/util"
)

// RunCompress 是压缩功能的入口函数，根据 dirMode 选择单文件压缩或目录批量压缩。
func RunCompress(inputFile, outputDir string, dirMode bool, preset string, jpegQuality, resolution int) error {
	if dirMode {
		return compressPDFDir(inputFile, outputDir, preset, jpegQuality, resolution)
	}
	// 单文件模式
	if outputDir == "" {
		outputDir = filepath.Dir(inputFile)
	}
	base := filepath.Base(inputFile)
	name := base[:len(base)-4] // 去掉 .pdf
	outputFile := filepath.Join(outputDir, name+"_compressed.pdf")
	return compressPDF(inputFile, outputFile, preset, jpegQuality, resolution)
}

// compressPDF 压缩单个 PDF 文件。
func compressPDF(inputFile, outputFile, preset string, jpegQuality, resolution int) error {
	// 检查输入文件
	if _, err := os.Stat(inputFile); err != nil {
		return fmt.Errorf("输入文件不存在: %s", inputFile)
	}

	// 检查 Ghostscript
	if util.FindGS() == "" {
		return fmt.Errorf("Ghostscript (gs) 未找到，请先安装")
	}

	// 映射预设名称
	presetMap := map[string]string{
		"screen":   "/screen",
		"ebook":    "/ebook",
		"printer":  "/printer",
		"prepress": "/prepress",
		"high":     "",
	}
	gsPreset := "/" + preset
	disableDownsample := false
	if mapped, ok := presetMap[preset]; ok {
		gsPreset = mapped
		if mapped == "" {
			disableDownsample = true
		}
	}

	// 获取输入文件大小
	inputInfo, _ := os.Stat(inputFile)
	inputSize := inputInfo.Size()

	// 构建 Ghostscript 参数
	args := []string{
		"-sDEVICE=pdfwrite",
		"-dNOPAUSE", "-dSAFER", "-dBATCH",
		"-dQUIET",
	}

	// PDF 预设
	if gsPreset != "" {
		args = append(args, fmt.Sprintf("-dPDFSETTINGS=%s", gsPreset))
	}
	if disableDownsample {
		args = append(args,
			"-dDownsampleColorImages=false",
			"-dDownsampleGrayImages=false",
			"-dDownsampleMonoImages=false",
			"-dColorImageResolution=1200",
			"-dGrayImageResolution=1200",
			"-dMonoImageResolution=2400",
		)
	}

	// 用户指定降采样 DPI（覆盖预设，也覆盖 high 预设的默认 1200）
	if resolution > 0 {
		args = append(args,
			fmt.Sprintf("-dColorImageResolution=%d", resolution),
			fmt.Sprintf("-dGrayImageResolution=%d", resolution),
			fmt.Sprintf("-dMonoImageResolution=%d", resolution*2),
		)
	}

	// JPEG 质量
	if jpegQuality > 0 && jpegQuality <= 100 {
		args = append(args, fmt.Sprintf("-dJPEGQ=%d", jpegQuality))
	}

	// 强制 JPEG 编码
	args = append(args,
		"-dAutoFilterColorImages=false",
		"-dColorImageFilter=/DCTEncode",
		"-dAutoFilterGrayImages=false",
		"-dGrayImageFilter=/DCTEncode",
	)

	// 字体子集化
	args = append(args, "-dSubsetFonts=true", "-dMaxSubsetPct=100")

	// 流压缩
	args = append(args, "-dCompressPages=true", "-dUseFlateCompression=true")

	// 输入输出
	args = append(args,
		fmt.Sprintf("-sOutputFile=%s", outputFile),
		inputFile,
	)

	// 执行 Ghostscript
	cmd := exec.Command(util.FindGS(), args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Ghostscript 压缩失败: %w", err)
	}

	// 获取输出文件大小并打印结果
	outputInfo, err := os.Stat(outputFile)
	if err != nil {
		return fmt.Errorf("读取输出文件失败: %w", err)
	}
	outputSize := outputInfo.Size()

	ratio := float64(outputSize) / float64(inputSize) * 100
	reduction := (1 - float64(outputSize)/float64(inputSize)) * 100

	fmt.Printf("PDF 压缩完成\n")
	fmt.Printf("  输入: %s (%s)\n", inputFile, util.FormatSize(inputSize))
	fmt.Printf("  输出: %s (%s)\n", outputFile, util.FormatSize(outputSize))
	fmt.Printf("  压缩比: %.1f%% (减少了 %.1f%%)\n", ratio, reduction)

	return nil
}

// compressPDFDir 压缩目录下所有 PDF 文件，支持并发处理。
func compressPDFDir(inputDir, outputDir, preset string, jpegQuality, resolution int) error {
	// 检查输入目录
	info, err := os.Stat(inputDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("输入目录不存在: %s", inputDir)
	}

	// 创建输出目录
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	// 检查 Ghostscript
	if util.FindGS() == "" {
		return fmt.Errorf("Ghostscript (gs) 未找到，请先安装")
	}

	// 收集 PDF 文件
	files, err := filepath.Glob(filepath.Join(inputDir, "*.pdf"))
	if err != nil {
		return fmt.Errorf("扫描目录失败: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("目录下没有找到 PDF 文件")
	}

	// 排序文件
	sort.Slice(files, func(i, j int) bool {
		return util.NaturalLess(filepath.Base(files[i]), filepath.Base(files[j]))
	})

	numWorkers := util.ComputeWorkerCount(100)
	fmt.Printf("找到 %d 个 PDF 文件，开始并发压缩 (%d 线程)...", len(files), numWorkers)
	startTime := time.Now()

	// 映射预设名称
	presetMap := map[string]string{
		"screen":   "/screen",
		"ebook":    "/ebook",
		"printer":  "/printer",
		"prepress": "/prepress",
		"high":     "",
	}
	gsPreset := "/" + preset
	disableDownsample := false
	if mapped, ok := presetMap[preset]; ok {
		gsPreset = mapped
		if mapped == "" {
			disableDownsample = true
		}
	}

	// 并发控制
	sem := make(chan struct{}, numWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var totalInputSize int64
	var totalOutputSize int64
	successCount := 0
	failCount := 0

	for _, inputFile := range files {
		wg.Add(1)
		sem <- struct{}{} // 获取信号量

		go func(inputFile string) {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号量

			// 生成输出文件名
			base := filepath.Base(inputFile)
			name := base[:len(base)-4] // 去掉 .pdf
			outputFile := filepath.Join(outputDir, name+"_compressed.pdf")

			// 获取输入文件大小
			inputInfo, err := os.Stat(inputFile)
			if err != nil {
				mu.Lock()
				failCount++
				mu.Unlock()
				return
			}
			inputSize := inputInfo.Size()

			// 构建 Ghostscript 参数
			args := []string{
				"-sDEVICE=pdfwrite",
				"-dNOPAUSE", "-dSAFER", "-dBATCH",
				"-dQUIET",
			}
			if gsPreset != "" {
				args = append(args, fmt.Sprintf("-dPDFSETTINGS=%s", gsPreset))
			}
			if disableDownsample {
				args = append(args,
					"-dDownsampleColorImages=false",
					"-dDownsampleGrayImages=false",
					"-dDownsampleMonoImages=false",
					"-dColorImageResolution=1200",
					"-dGrayImageResolution=1200",
					"-dMonoImageResolution=2400",
				)
			}

			// 用户指定降采样 DPI（覆盖预设，也覆盖 high 预设的默认 1200）
			if resolution > 0 {
				args = append(args,
					fmt.Sprintf("-dColorImageResolution=%d", resolution),
					fmt.Sprintf("-dGrayImageResolution=%d", resolution),
					fmt.Sprintf("-dMonoImageResolution=%d", resolution*2),
				)
			}

			if jpegQuality > 0 && jpegQuality <= 100 {
				args = append(args, fmt.Sprintf("-dJPEGQ=%d", jpegQuality))
			}

			args = append(args,
				"-dAutoFilterColorImages=false",
				"-dColorImageFilter=/DCTEncode",
				"-dAutoFilterGrayImages=false",
				"-dGrayImageFilter=/DCTEncode",
				"-dSubsetFonts=true", "-dMaxSubsetPct=100",
				"-dCompressPages=true", "-dUseFlateCompression=true",
				fmt.Sprintf("-sOutputFile=%s", outputFile),
				inputFile,
			)

			// 执行压缩
			cmd := exec.Command(util.FindGS(), args...)
			if err := cmd.Run(); err != nil {
				mu.Lock()
				failCount++
				mu.Unlock()
				return
			}

			// 获取输出文件大小
			outputInfo, err := os.Stat(outputFile)
			if err != nil {
				mu.Lock()
				failCount++
				mu.Unlock()
				return
			}
			outputSize := outputInfo.Size()

			// 更新统计
			mu.Lock()
			totalInputSize += inputSize
			totalOutputSize += outputSize
			successCount++
			mu.Unlock()
		}(inputFile)
	}

	wg.Wait()

	elapsed := time.Since(startTime)

	// 打印结果
	fmt.Printf("\n压缩完成！\n")
	fmt.Printf("  成功: %d 个文件\n", successCount)
	if failCount > 0 {
		fmt.Printf("  失败: %d 个文件\n", failCount)
	}
	fmt.Printf("  总耗时: %.1f 秒\n", elapsed.Seconds())
	fmt.Printf("  输入总量: %s\n", util.FormatSize(totalInputSize))
	fmt.Printf("  输出总量: %s\n", util.FormatSize(totalOutputSize))
	if totalInputSize > 0 {
		ratio := float64(totalOutputSize) / float64(totalInputSize) * 100
		fmt.Printf("  压缩比: %.1f%% (减少了 %.1f%%)\n", ratio, 100-ratio)
	}

	return nil
}