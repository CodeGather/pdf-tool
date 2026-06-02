package cmd

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"example.com/pdf-tool/util"
	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// RunMerge 合并多个 PDF 文件为一个。支持分块合并、合并前压缩、分隔页等特性。
func RunMerge(inputDir, inputList, globPattern, outputFile string, chunkSize int, dividerPage, progressEnabled, mergeCompress bool, compressPreset string, compressJPEGQ, compressResolution int) error {
	files, err := collectMergeInputs(inputDir, inputList, globPattern)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no pdf files found for merge")
	}

	// 合并前压缩
	if mergeCompress {
		// 检查 Ghostscript
		if util.FindGS() == "" {
			return fmt.Errorf("需要安装 Ghostscript (gs) 才能启用合并前压缩，请先安装后重试或使用 -merge-compress=false 关闭")
		}

		// 映射预设名称
		presetMap := map[string]string{
			"screen":   "/screen",
			"ebook":    "/ebook",
			"printer":  "/printer",
			"prepress": "/prepress",
			"high":     "",
		}
		gsPreset := "/" + compressPreset
		disableDownsample := false
		if mapped, ok := presetMap[compressPreset]; ok {
			gsPreset = mapped
			if mapped == "" {
				disableDownsample = true
			}
		}

		// 创建临时压缩目录
		outDir := filepath.Dir(outputFile)
		compressDir, err := os.MkdirTemp(outDir, "pdf-tool-merge-compress-*")
		if err != nil {
			return fmt.Errorf("创建压缩临时目录: %w", err)
		}
		defer os.RemoveAll(compressDir)

		log.Printf("merge-compress: 开始压缩 %d 个 PDF 文件", len(files))

		// 并发压缩
		numWorkers := util.ComputeWorkerCount(100)
		type compResult struct {
			idx        int
			compressed string
			inputSize  int64
			outputSize int64
			err        error
		}
		results := make(chan compResult, len(files))

		var wg sync.WaitGroup
		wg.Add(len(files))
		sem := make(chan struct{}, numWorkers)
		util.TraceProgress(progressEnabled, 0, fmt.Sprintf("正在压缩 PDF（共 %d 个）", len(files)))
		// 后台启动 goroutine（与收结果并行，避免 sem 阻塞延迟进度）
		go func() {
			for i, f := range files {
				sem <- struct{}{}
				go func(idx int, inputPath string) {
					defer wg.Done()
					defer func() { <-sem }()
					base := filepath.Base(inputPath)
					outPath := filepath.Join(compressDir, base)
					inputInfo, _ := os.Stat(inputPath)
					inputSize := int64(0)
					if inputInfo != nil {
						inputSize = inputInfo.Size()
					}

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
					if compressResolution > 0 {
						args = append(args,
							fmt.Sprintf("-dColorImageResolution=%d", compressResolution),
							fmt.Sprintf("-dGrayImageResolution=%d", compressResolution),
							fmt.Sprintf("-dMonoImageResolution=%d", compressResolution*2),
						)
					}
					if compressJPEGQ > 0 && compressJPEGQ <= 100 {
						args = append(args, fmt.Sprintf("-dJPEGQ=%d", compressJPEGQ))
					}
					args = append(args,
						"-dAutoFilterColorImages=false",
						"-dColorImageFilter=/DCTEncode",
						"-dAutoFilterGrayImages=false",
						"-dGrayImageFilter=/DCTEncode",
						"-dSubsetFonts=true", "-dMaxSubsetPct=100",
						"-dCompressPages=true", "-dUseFlateCompression=true",
						fmt.Sprintf("-sOutputFile=%s", outPath),
						inputPath,
					)

					cmd := exec.Command(util.FindGS(), args...)
					if err := cmd.Run(); err != nil {
						results <- compResult{idx: idx, err: fmt.Errorf("压缩 %s 失败: %w", inputPath, err)}
						return
					}

					outputInfo, _ := os.Stat(outPath)
					outputSize := int64(0)
					if outputInfo != nil {
						outputSize = outputInfo.Size()
					}

					results <- compResult{idx: idx, compressed: outPath, inputSize: inputSize, outputSize: outputSize}
				}(i, f)
			}
		}()

		// 另起 goroutine 等待完成，主 goroutine 边收结果边报进度
		go func() {
			wg.Wait()
			close(results)
		}()

		compressedFiles := make([]string, len(files))
		var totalInputSize, totalOutputSize int64
		completed := 0
		for r := range results {
			if r.err != nil {
				return r.err
			}
			compressedFiles[r.idx] = r.compressed
			totalInputSize += r.inputSize
			totalOutputSize += r.outputSize
			completed++
			util.TraceProgress(progressEnabled, completed*100/len(files), "正在压缩 PDF")
		}

		// 替换文件列表为压缩版本
		files = compressedFiles

		ratio := float64(totalOutputSize) / float64(totalInputSize) * 100
		reduction := (1 - float64(totalOutputSize)/float64(totalInputSize)) * 100
		log.Printf("merge-compress: 完成 输入=%s 输出=%s 压缩比=%.1f%% (减少 %.1f%%)",
			util.FormatSize(totalInputSize), util.FormatSize(totalOutputSize), ratio, reduction)
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

	util.TraceProgress(progressEnabled, 0, "正在合并 PDF")
	if len(files) <= chunkSize {
		log.Printf("merge: single pass files=%d", len(files))
		if err := api.MergeCreateFile(files, outputFile, dividerPage, nil); err != nil {
			return fmt.Errorf("merge pdfs: %w", err)
		}
		log.Printf("merge: done output=%s", outputFile)
		util.TraceProgress(progressEnabled, 100, "合并完成")
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
		util.TraceProgress(progressEnabled, progress, "正在合并 PDF")
	}

	log.Printf("merge: final pass chunks=%d", len(chunkFiles))
	if err := api.MergeCreateFile(chunkFiles, outputFile, dividerPage, nil); err != nil {
		return fmt.Errorf("merge final output: %w", err)
	}
	log.Printf("merge: done output=%s", outputFile)
	util.TraceProgress(progressEnabled, 100, "合并完成")
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
		return util.NaturalLess(filepath.Base(matches[i]), filepath.Base(matches[j]))
	})
	return matches, nil
}
