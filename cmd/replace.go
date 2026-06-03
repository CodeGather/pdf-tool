package cmd

import (
	"encoding/json"
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

// progressMsg JSON 进度消息
type progressMsg struct {
	Code     int     `json:"code"`
	Message  string  `json:"message"`
	Progress float64 `json:"progress"`
}

// replaceResult 单次替换的最终 JSON 结果
type replaceResult struct {
	Code     int           `json:"code"`
	Message  string        `json:"message"`
	Progress float64       `json:"progress"`
	Data     []progressMsg `json:"data"`
}

var progressEvents []progressMsg
var progressMu sync.Mutex

func reportProgress(jsonEnabled bool, code int, msg string, progress float64) {
	if jsonEnabled {
		progressMu.Lock()
		progressEvents = append(progressEvents, progressMsg{Code: code, Message: msg, Progress: progress})
		progressMu.Unlock()
	} else {
		log.Printf(msg)
	}
}

func flushProgress(jsonEnabled bool, code int, msg string) {
	if jsonEnabled {
		result := replaceResult{
			Code:     code,
			Message:  msg,
			Progress: 100.0,
			Data:     progressEvents,
		}
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
		progressEvents = nil
	} else if code == 1 {
		log.Printf(msg)
	}
}

// lampLabel 返回"本档素材"或"上一档"
func lampLabel(isNew bool) string {
	if isNew {
		return "本档素材"
	}
	return "上一档"
}

// failJSON JSON 模式下遇到致命错误时输出错误结果并返回 nil
func failJSON(jsonEnabled bool, msg string) error {
	if jsonEnabled {
		reportProgress(true, 1, msg, 0)
		flushProgress(true, 1, msg)
		return nil
	}
	return fmt.Errorf("%s", msg)
}

// Run 程序主入口
func Run(inputPath, outputPath, fontPath, baseDir string, cpu int, jsonProgress bool, prevShow int) error {
	cfg, err := config.LoadConfig(inputPath)
	if err != nil {
		return failJSON(jsonProgress, fmt.Sprintf("加载配置失败: %v", err))
	}

	if !jsonProgress {
		log.Printf("店铺: %s", cfg.ShopName)
		log.Printf("灯位数量: %d", len(cfg.ExcelData))
		log.Printf("素材数量: %d", len(cfg.FileData))
	}

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
		return failJSON(jsonProgress, "db-data.lamp 为空")
	}
	tmplPath := filepath.Join(actualPDFBase, cfg.DbData.Lamps[0].File)
	if !jsonProgress {
		log.Printf("模板 PDF: %s", tmplPath)
	}

	// 2. 打开模板
	tmpl, err := pdf.OpenTemplate(tmplPath)
	if err != nil {
		return failJSON(jsonProgress, fmt.Sprintf("打开模板失败: %v", err))
	}
	defer tmpl.Close()

	// 3. 准备所有灯位的任务
	var newRows []tableRow
	var newItems []struct {
		objNr int
		name  string
		pos   pdf.ImagePosition
	}
	type prepJob struct {
		numStr   string
		lampItem model.LampItem
		objNr    int
		imgPos   pdf.ImagePosition
		srcPath  string
		targetW  float64
		targetH  float64
		isNew    bool
		err      error
	}
	var prepJobs []prepJob

	// 计算总灯位数用于进度
	allEntries := cfg.DbData.Lamps[0].NumList
	totalLamps := len(allEntries)
	if totalLamps == 0 {
		totalLamps = len(cfg.ExcelData) // 兜底
	}
	progressPerLamp := 100.0 / float64(totalLamps)
	processedCount := 0

	// JSON 模式：重置收集器
	if jsonProgress {
		progressMu.Lock()
		progressEvents = nil
		progressMu.Unlock()
	}

	for _, entry := range allEntries {
		// 3a. 灯位编号
		numStr := ""
		if len(entry.Nums) > 0 {
			numStr = entry.Nums[0].Str
		} else if entry.Num.Str != "" {
			numStr = entry.Num.Str
		}
		if numStr == "" {
			processedCount++
			reportProgress(jsonProgress, 1,
				fmt.Sprintf("灯位无编号，跳过"),
				float64(processedCount)*progressPerLamp)
			continue
		}

		// 3b. excel-data
		lampItem, ok := cfg.ExcelData[numStr]
		if !ok {
			processedCount++
			reportProgress(jsonProgress, 1,
				fmt.Sprintf("编号 %s 不在 excel-data 中", numStr),
				float64(processedCount)*progressPerLamp)
			continue
		}

		// 3c. 匹配 PDF 图片位置
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
		log.Printf("  [匹配] %s: 搜索 img ot=(%.0f,%.0f,%.0f,%.0f)", numStr, imgX, imgY, imgW, imgH)
		ip := tmpl.FindImageByRect(imgX, imgY, imgW, imgH, tolerance)
		if ip == nil {
			processedCount++
			reportProgress(jsonProgress, 1,
				fmt.Sprintf("编号 %s 未找到匹配的 PDF 图片位置", numStr),
				float64(processedCount)*progressPerLamp)
			continue
		}
		if !jsonProgress {
			log.Printf("  灯位 %s: obj=%d", numStr, ip.ObjNr)
		}
		log.Printf("  [匹配] %s: 找到 obj=%d xywh=(%.0f,%.0f,%.0f,%.0f)", numStr, ip.ObjNr, ip.X, ip.Y, ip.W, ip.H)

		// 3d. 匹配素材
		fileKey, found := matcher.MatchFileDataKey(lampItem.LampNote, cfg.FileData)
		if !found {
			processedCount++
			if lampItem.IsNewValue() || prevShow != 1 {
				reportProgress(jsonProgress, 1,
					fmt.Sprintf("点位 %s 没有找到%s素材", numStr, lampLabel(lampItem.IsNewValue())),
					float64(processedCount)*progressPerLamp)
			}
			continue
		}
		fileEntry := cfg.FileData[fileKey]

		var allImages []model.ImageMeta
		for _, page := range fileEntry.Pages {
			allImages = append(allImages, page.Images...)
		}
		if len(allImages) == 0 {
			processedCount++
			if lampItem.IsNewValue() || prevShow != 1 {
				reportProgress(jsonProgress, 1,
					fmt.Sprintf("点位 %s 没有找到%s素材", numStr, lampLabel(lampItem.IsNewValue())),
					float64(processedCount)*progressPerLamp)
			}
			continue
		}

		// 3e. 必须缩放到原始图片像素尺寸，否则 PDF CTM 矩阵缩放后贴不准灯位
		targetW := entry.Image.Width
		targetH := entry.Image.Height
		if targetW <= 0 || targetH <= 0 {
			targetW = lampItem.VisibleW
			targetH = lampItem.VisibleH
		}
		match := matcher.SelectBestImage(targetW, targetH, allImages)
		if !match.Found {
			processedCount++
			if lampItem.IsNewValue() || prevShow != 1 {
				reportProgress(jsonProgress, 1,
					fmt.Sprintf("点位 %s 没有找到%s素材", numStr, lampLabel(lampItem.IsNewValue())),
					float64(processedCount)*progressPerLamp)
			}
			continue
		}
		if !jsonProgress {
			log.Printf("  选中图片: %s (%.0fx%.0f)", match.Image.Path, match.Image.Width, match.Image.Height)
		}

		// 上一档且 prev=1 时跳过整个灯片（不替换图片，不输出消息）
		if !lampItem.IsNewValue() && prevShow == 1 {
			continue
		}

		prepJobs = append(prepJobs, prepJob{
			numStr:   numStr,
			lampItem: lampItem,
			objNr:    ip.ObjNr,
			imgPos:   *ip,
			srcPath:  match.Image.Path,
			targetW:  targetW,
			targetH:  targetH,
			isNew:    lampItem.IsNewValue(),
		})

		processedCount++
		reportProgress(jsonProgress, 0,
			fmt.Sprintf("点位 %s %s", numStr, lampLabel(lampItem.IsNewValue())),
			float64(processedCount)*progressPerLamp)
	}

	// 4. 并行处理图片（打开→解码→文字叠加→JPEG 编码）
	if len(prepJobs) == 0 {
		reportProgress(jsonProgress, 1, "没有可处理的灯位，直接输出模板", 95.0)
		// 仍然写入输出
		if err := tmpl.WriteToFile(outputPath); err != nil {
			return fmt.Errorf("写入 PDF 失败: %w", err)
		}
		reportProgress(jsonProgress, 0, "完成", 100.0)
		flushProgress(jsonProgress, 0, "完成")
		return nil
	}

	if !jsonProgress {
		log.Printf("并行处理 %d 张图片...", len(prepJobs))
	}
	type processed struct {
		numStr   string
		objNr    int
		sd       *types.StreamDict
		isNew    bool
		imgPos   pdf.ImagePosition
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
					results <- processed{numStr: job.numStr, objNr: job.objNr, sd: sd, isNew: job.isNew, imgPos: job.imgPos, lampItem: job.lampItem, err: nil}
				} else {
					results <- processed{numStr: job.numStr, objNr: job.objNr, sd: nil, isNew: job.isNew, imgPos: job.imgPos, lampItem: job.lampItem, err: err}
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

	// 5. 串行替换 PDF 图片
	totalJobs := len(prepJobs)
	jobDone := 0
	for r := range results {
		if r.err != nil {
			jobDone++
			reportProgress(jsonProgress, 1,
				fmt.Sprintf("编号 %s 处理失败: %v", r.numStr, r.err),
				90.0+float64(jobDone)/float64(totalJobs)*5.0)
			continue
		}
		if err := tmpl.ReplaceStreamDict(r.objNr, r.sd); err != nil {
			jobDone++
			reportProgress(jsonProgress, 1,
				fmt.Sprintf("编号 %s 替换 PDF 失败: %v", r.numStr, err),
				90.0+float64(jobDone)/float64(totalJobs)*5.0)
			continue
		}
		if !jsonProgress {
			log.Printf("  [替换] 灯位 %s (obj=%d)", r.numStr, r.objNr)
		}
		if r.isNew {
			newItems = append(newItems, struct {
				objNr int
				name  string
				pos   pdf.ImagePosition
			}{objNr: r.objNr, name: r.numStr, pos: r.imgPos})
			newRows = append(newRows, tableRow{num: r.numStr, item: r.lampItem})
		}
		jobDone++
		if !r.isNew && prevShow == 1 {
			continue
		}
		reportProgress(jsonProgress, 0,
			fmt.Sprintf("点位 %s %s", r.numStr, lampLabel(r.isNew)),
			90.0+float64(jobDone)/float64(totalJobs)*5.0)
	}

	// 5b. 独立 PDF 矢量边框
	bc := cfg.BrandConf
	for _, item := range newItems {
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
		log.Printf("  [边框] %s: obj=%d page=%d xywh=%.0f,%.0f,%.0f,%.0f lw=%.1f rgb=%.2f,%.2f,%.2f",
			item.name, item.objNr, item.pos.Page, item.pos.X, item.pos.Y, item.pos.W, item.pos.H, lw, borderR, borderG, borderB)
		if err := tmpl.DrawRectBorder(item.pos, lw,
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
	if !jsonProgress {
		log.Printf("完成: %s", outputPath)
	}
	flushProgress(jsonProgress, 0, "完成")
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
	img := pdf.ScaleImageFill(srcImg, targetW, targetH)

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

	if !jsonProgress {
		log.Printf("表格已注入 (行数=%d, 字体=%s)", len(rows), filepath.Base(fontPath))
	}
	return 0, nil
}

// 包级变量：JSON 进度模式开关（供 renderTable 使用）
var jsonProgress bool

// RunReplaceDir 批量合成模式：扫描目录中所有 *.json 文件，依次处理
func RunReplaceDir(mergeDir, outputDir string, cpu int, jsonProg bool, prevShow int) error {
	jsonProgress = jsonProg

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

	if !jsonProg {
		log.Printf("批量合成: %s → %s (%d 个文件)", mergeDir, outputDir, len(jsonFiles))
	}
	startTime := time.Now()

	success, fail := 0, 0
	for i, name := range jsonFiles {
		inputPath := filepath.Join(mergeDir, name)
		outName := strings.TrimSuffix(name, ".json") + ".pdf"
		outputPath := filepath.Join(outputDir, outName)

		fileStart := time.Now()
		if err := Run(inputPath, outputPath, "", "", cpu, jsonProg, prevShow); err != nil {
			if jsonProg {
				reportProgress(true, 1, fmt.Sprintf("文件 %s 处理失败: %v", name, err),
					float64(i+1)/float64(len(jsonFiles))*100.0)
			} else {
				log.Printf("  [失败 %s] %v", name, err)
			}
			fail++
			continue
		}
		if !jsonProg {
			elapsed := time.Since(fileStart)
			if fi, err := os.Stat(outputPath); err == nil {
				log.Printf("  [%d/%d] %s → %s (%.1fMB, %v)",
					i+1, len(jsonFiles), name, outName,
					float64(fi.Size())/1024/1024, elapsed.Round(time.Millisecond))
			} else {
				log.Printf("  [%d/%d] %s → %s (%v)",
					i+1, len(jsonFiles), name, outName, elapsed.Round(time.Millisecond))
			}
		}
		success++
	}

	totalTime := time.Since(startTime)
	if jsonProg {
		reportProgress(true, 0, fmt.Sprintf("批量合成完成: 成功=%d 失败=%d 总耗时=%v", success, fail, totalTime.Round(time.Second)), 100.0)
	} else {
		log.Printf("批量合成完成: 成功=%d 失败=%d 总耗时=%v", success, fail, totalTime.Round(time.Second))
	}
	return nil
}