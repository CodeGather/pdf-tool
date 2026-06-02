package cmd

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"example.com/pdf-tool/config"
	"example.com/pdf-tool/matcher"
	"example.com/pdf-tool/model"
	"example.com/pdf-tool/pdf"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

const (
	tolerance  = 5.0
	basePDFDir = "/Users/Yau/Library/Application Support/com.lorealchina.mplus"
)

type tableRow struct {
	num  string
	item model.LampItem
}

type lampJob struct {
	numStr   string
	lampItem model.LampItem
	objNr    int
	srcPath  string
	targetW  float64
	targetH  float64
	isNew    bool
}

// Run 程序主入口
func Run(inputPath, outputPath, fontPath, baseDir string, cpu int) error {
	cfg, err := config.LoadConfig(inputPath)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	log.Printf("店铺: %s", cfg.ShopName)
	log.Printf("灯位数量: %d", len(cfg.ExcelData))
	log.Printf("素材数量: %d", len(cfg.FileData))

	// 确定基础目录
	actualPDFBase := basePDFDir
	if baseDir != "" {
		actualPDFBase = baseDir
	}
	// 确定字体路径
	actualFontPath := fontPath
	if actualFontPath == "" {
		actualFontPath = filepath.Join(filepath.Dir(inputPath), "..", "assets", "hyzdx.ttf")
		if _, err := os.Stat(actualFontPath); os.IsNotExist(err) {
			actualFontPath = "/Users/Yau/work/1.Resources/2.AI/pdf-replace/assets/hyzdx.ttf"
		}
	}

	// 1. 确定模板 PDF 路径
	if len(cfg.DbData.Lamps) == 0 {
		return fmt.Errorf("db-data.lamp 为空")
	}
	tmplPath := filepath.Join(actualPDFBase, cfg.DbData.Lamps[0].File)
	log.Printf("模板 PDF: %s", tmplPath)

	// 2. 打开模板
	tmpl, err := pdf.OpenTemplate(tmplPath)
	if err != nil {
		return fmt.Errorf("打开模板失败: %w", err)
	}
	defer tmpl.Close()

	// 3. 准备所有灯位的任务
	var newRows []tableRow
	var newObjNrPositions []struct {
		objNr int
		name  string
	}
	type prepJob struct {
		numStr   string
		lampItem model.LampItem
		objNr    int
		srcPath  string
		targetW  float64
		targetH  float64
		isNew    bool
		err      error
	}
	var prepJobs []prepJob

	for _, entry := range cfg.DbData.Lamps[0].NumList {
		// 3a. 灯位编号
		numStr := ""
		if len(entry.Nums) > 0 {
			numStr = entry.Nums[0].Str
		} else if entry.Num.Str != "" {
			numStr = entry.Num.Str
		}
		if numStr == "" {
			log.Printf("  [跳过] 无灯位编号")
			continue
		}

		// 3b. excel-data
		lampItem, ok := cfg.ExcelData[numStr]
		if !ok {
			log.Printf("  [跳过] 灯位 %s 不在 excel-data 中", numStr)
			continue
		}

		// 3c. 收集 isNew 表格行（独立于图片替换，无素材也有表格）
		if lampItem.IsNewValue() {
			newRows = append(newRows, tableRow{num: numStr, item: lampItem})
		}

		// 3d. 匹配 PDF 图片位置
		imgX := entry.Image.OriginalTransform.X
		imgY := entry.Image.OriginalTransform.Y
		imgW := entry.Image.OriginalTransform.Width
		imgH := entry.Image.OriginalTransform.Height
		if imgW <= 0 || imgH <= 0 {
			imgX = entry.Image.X
			imgY = entry.Image.Y
			imgW = entry.Image.Width
			imgH = entry.Image.Height
		}
		ip := tmpl.FindImageByRect(imgX, imgY, imgW, imgH, tolerance)
		if ip == nil {
			log.Printf("  [跳过] 灯位 %s 未找到匹配的 PDF 图片位置", numStr)
			continue
		}
		log.Printf("  灯位 %s: obj=%d", numStr, ip.ObjNr)

		// 3d. 匹配素材
		fileKey, found := matcher.MatchFileDataKey(lampItem.LampNote, cfg.FileData)
		if !found {
			log.Printf("  [跳过] 灯位 %s 无匹配素材", numStr)
			continue
		}
		fileEntry := cfg.FileData[fileKey]

		var allImages []model.ImageMeta
		for _, page := range fileEntry.Pages {
			allImages = append(allImages, page.Images...)
		}
		if len(allImages) == 0 {
			log.Printf("  [跳过] 灯位 %s 素材无图片", numStr)
			continue
		}

		targetW := lampItem.VisibleW
		targetH := lampItem.VisibleH
		if targetW <= 0 || targetH <= 0 {
			targetW = entry.Image.Width
			targetH = entry.Image.Height
		}
		match := matcher.SelectBestImage(targetW, targetH, allImages)
		if !match.Found {
			log.Printf("  [跳过] 灯位 %s 无法匹配图片", numStr)
			continue
		}
		log.Printf("  选中图片: %s (%.0fx%.0f)", match.Image.Path, match.Image.Width, match.Image.Height)

		prepJobs = append(prepJobs, prepJob{
			numStr:   numStr,
			lampItem: lampItem,
			objNr:    ip.ObjNr,
			srcPath:  match.Image.Path,
			targetW:  targetW,
			targetH:  targetH,
			isNew:    lampItem.IsNewValue(),
		})
	}

	// 4. 并行处理图片（打开→解码→文字叠加→JPEG 编码）
	log.Printf("并行处理 %d 张图片...", len(prepJobs))
	type processed struct {
		numStr   string
		objNr    int
		sd       *types.StreamDict
		isNew    bool
		lampItem model.LampItem
		err      error
	}

	jobs := make(chan prepJob, len(prepJobs))
	results := make(chan processed, len(prepJobs))
	var wg sync.WaitGroup

	workerCount := len(prepJobs)
	if cpu > 0 {
		workerCount = cpu
	} else if workerCount > 4 {
		workerCount = 4
	}
	if workerCount < 1 {
		workerCount = 1
	}

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				img, err := processImageDirect(job.srcPath, job.lampItem, cfg.BrandConf, job.targetW, job.targetH, actualFontPath)
				if err == nil && img != nil {
					sd := pdf.ImageToStreamDictJPEG(img, 85)
					results <- processed{numStr: job.numStr, objNr: job.objNr, sd: sd, isNew: job.isNew, lampItem: job.lampItem, err: nil}
				} else {
					results <- processed{numStr: job.numStr, objNr: job.objNr, sd: nil, isNew: job.isNew, lampItem: job.lampItem, err: err}
				}
			}
		}()
	}

	for _, j := range prepJobs {
		jobs <- j
	}
	close(jobs)
	wg.Wait()
	close(results)

	// 5. 串行替换 PDF 图片（直接 Flate 压缩像素，跳过二次解码）
	for r := range results {
		if r.err != nil {
			log.Printf("  [错误] 灯位 %s: %v", r.numStr, r.err)
			continue
		}
		if err := tmpl.ReplaceStreamDict(r.objNr, r.sd); err != nil {
			log.Printf("  [错误] 灯位 %s 替换失败: %v", r.numStr, err)
			continue
		}
		log.Printf("  [替换] 灯位 %s (obj=%d)", r.numStr, r.objNr)
		if r.isNew {
			newObjNrPositions = append(newObjNrPositions, struct {
				objNr int
				name  string
			}{objNr: r.objNr, name: r.numStr})
		}
	}

	// 5b. 独立 PDF 矢量边框
	bc := cfg.BrandConf
	for _, item := range newObjNrPositions {
		imgPos := tmpl.FindImageByObjNr(item.objNr)
		if imgPos == nil {
			log.Printf("  [警告] 灯位 %s(obj=%d) 无位置信息，跳过边框", item.name, item.objNr)
			continue
		}
		lw := 1.0
		borderR, borderG, borderB := 1.0, 0.0, 0.0
		if bc.Guide != nil {
			lw = bc.Guide.BorderSize
			borderR = bc.Guide.BorderColor.Red
			borderG = bc.Guide.BorderColor.Green
			borderB = bc.Guide.BorderColor.Blue
		}
		if lw < 1 {
			lw = 1
		}
		if err := tmpl.DrawRectBorder(*imgPos, lw,
			borderR, borderG, borderB); err != nil {
			log.Printf("  [警告] 灯位 %s 边框失败: %v", item.name, err)
		}
	}

	// 6. 构建 isNew 表格
	if _, err := renderTable(tmpl, cfg, newRows, actualFontPath); err != nil {
		return fmt.Errorf("渲染表格失败: %w", err)
	}

	// 7. 写入
	if err := tmpl.WriteToFile(outputPath); err != nil {
		return fmt.Errorf("写入 PDF 失败: %w", err)
	}
	log.Printf("完成: %s", outputPath)
	return nil
}

// processImageDirect 解码图片 → 缩放到灯位尺寸 → 叠加上市备注文字 → 返回 image.Image
func processImageDirect(srcPath string, lampItem model.LampItem, bc model.BrandConfig, targetW, targetH float64, fontPath string) (image.Image, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("打开图片失败: %w", err)
	}
	defer f.Close()

	srcImg, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("解码图片失败: %w", err)
	}

	// 缩放到灯位显示尺寸
	img := pdf.ScaleImageContain(srcImg, targetW, targetH)

	// 上市备注文字
	if lampItem.LaunchNote != "" {
		fontPt := 16.0
		descR, descG, descB, descA := 1.0, 0.0, 0.0, 1.0
		if bc.Guide != nil {
			fontPt = bc.Guide.DescFontSize
			descR = bc.Guide.DescColor.Red
			descG = bc.Guide.DescColor.Green
			descB = bc.Guide.DescColor.Blue
			descA = bc.Guide.DescColor.Opacity
		}
		if fontPt <= 0 {
			fontPt = 16
		}
		img = pdf.DrawTextOnTop(img, lampItem.LaunchNote,
			descR, descG, descB, descA,
			fontPt, fontPath)
	}

	return img, nil
}

// renderTable 渲染 isNew 表格并注入到页面底部
func renderTable(tmpl *pdf.Template, cfg *model.ReplaceConfig, rows []tableRow, fontPath string) (float64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	var cols []pdf.ColumnDef
	for _, tc := range cfg.TableConf {
		col := pdf.ColumnDef{Label: tc.Label, Key: tc.Key, Align: tc.Align}
		if tc.Width != nil {
			col.Width = tc.Width
		}
		cols = append(cols, col)
	}

	var tbRows []pdf.TableRow
	for _, r := range rows {
		row := pdf.TableRow{
			"柜台名称": cfg.ShopName,
			"灯位编号": r.num,
			"灯位位置": r.item.Position,
			"材质":   r.item.Material,
			"可见宽":  fmt.Sprintf("%.0f", r.item.VisibleW),
			"可见长":  fmt.Sprintf("%.0f", r.item.VisibleH),
			"灯位备注": r.item.LampNote,
			"画面内容": r.item.Content,
		}
		tbRows = append(tbRows, row)
	}

	pageW, _, err := tmpl.PageSize(1)
	if err != nil {
		return 0, fmt.Errorf("获取页面尺寸: %w", err)
	}

	gap := 0.0
	estTableH := 22.0 + float64(len(rows))*20.0
	extraH := estTableH + gap
	if err := tmpl.ExtendPageHeight(extraH); err != nil {
		return 0, fmt.Errorf("扩展页面高度: %w", err)
	}

	tmpPath := filepath.Join(os.TempDir(), fmt.Sprintf("pdf-replace-table-%d.pdf", os.Getpid()))
	tableH, err := pdf.WriteTableToPDF(tmpPath, cols, tbRows, fontPath, pageW)
	if err != nil {
		return 0, fmt.Errorf("生成表格 PDF: %w", err)
	}
	_ = tableH
	defer os.Remove(tmpPath)

	if err := pdf.InjectTableContent(tmpl, tmpPath, pageW, extraH); err != nil {
		return 0, fmt.Errorf("注入表格内容: %w", err)
	}

	log.Printf("表格已注入 (行数=%d, 字体=%s)", len(rows), filepath.Base(fontPath))
	return 0, nil
}

// RunReplaceDir 批量合成模式：扫描目录中所有 *.json 文件，依次处理
func RunReplaceDir(mergeDir, outputDir string, cpu int) error {
	entries, err := os.ReadDir(mergeDir)
	if err != nil {
		return fmt.Errorf("读取合成目录失败: %w", err)
	}

	// 收集并排序所有 .json 文件
	var jsonFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		jsonFiles = append(jsonFiles, e.Name())
	}
	if len(jsonFiles) == 0 {
		return fmt.Errorf("合成目录中无 JSON 文件: %s", mergeDir)
	}

	// 默认输出目录
	if outputDir == "" {
		outputDir = filepath.Join(mergeDir, "_output_")
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("创建输出目录失败: %w", err)
	}

	log.Printf("批量合成: %s → %s (%d 个文件)", mergeDir, outputDir, len(jsonFiles))
	startTime := time.Now()

	success, fail := 0, 0
	for i, name := range jsonFiles {
		inputPath := filepath.Join(mergeDir, name)
		outName := strings.TrimSuffix(name, ".json") + ".pdf"
		outputPath := filepath.Join(outputDir, outName)

		fileStart := time.Now()
		if err := Run(inputPath, outputPath, "", "", cpu); err != nil {
			log.Printf("  [失败 %s] %v", name, err)
			fail++
			continue
		}
		elapsed := time.Since(fileStart)
		if fi, err := os.Stat(outputPath); err == nil {
			log.Printf("  [%d/%d] %s → %s (%.1fMB, %v)",
				i+1, len(jsonFiles), name, outName,
				float64(fi.Size())/1024/1024, elapsed.Round(time.Millisecond))
		} else {
			log.Printf("  [%d/%d] %s → %s (%v)",
				i+1, len(jsonFiles), name, outName, elapsed.Round(time.Millisecond))
		}
		success++
	}

	totalTime := time.Since(startTime)
	log.Printf("批量合成完成: 成功=%d 失败=%d 总耗时=%v", success, fail, totalTime.Round(time.Second))
	return nil
}
