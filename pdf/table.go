package pdf

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/signintech/gopdf"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// ColumnDef 表格列定义
type ColumnDef struct {
	Label string
	Key   string
	Width *float64
	Align string
}

// TableRow 表格行数据
type TableRow map[string]string

// WriteTableToPDF 用 gopdf 生成原生 PDF 表格
func WriteTableToPDF(outputPath string, columns []ColumnDef, rows []TableRow, fontPath string, pageW float64) (float64, error) {
	marginL := 0.0
	usableW := pageW - marginL*2
	colWidths := calcColWidths(columns, usableW)

	headerH := 22.0
	bodyH := 20.0
	totalH := headerH + float64(len(rows))*bodyH

	tR, tG, tB := uint8(160), uint8(30), uint8(30)

	pdf := &gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: pageW, H: totalH}})
	pdf.AddTTFFont("hyzdx", fontPath)
	pdf.AddPage()

	pdf.SetFillColor(240, 240, 240)
	pdf.RectFromUpperLeftWithStyle(marginL, 0, usableW, headerH, "F")
	pdf.SetTextColor(0, 0, 0)
	x := marginL
	for i, col := range columns {
		pdf.SetFont("hyzdx", "", 9)
		cellW := colWidths[i]
		textW, _ := pdf.MeasureTextWidth(col.Label)
		tx := calcAlignX(x, cellW, textW, col.Align)
		pdf.SetXY(tx, 14)
		pdf.Text(col.Label)
		x += cellW
	}

	pdf.SetTextColor(tR, tG, tB)
	y := headerH
	for _, row := range rows {
		yBase := y + bodyH/2 + 9*0.38
		x = marginL
		for i, col := range columns {
			val := row[col.Key]
			pdf.SetFont("hyzdx", "", 9)
			textW, _ := pdf.MeasureTextWidth(val)
			cellW := colWidths[i]

			if textW > cellW {
				for len(val) > 0 {
					val = val[:len(val)-1]
					testW, _ := pdf.MeasureTextWidth(val + "...")
					if testW <= cellW {
						val = val + "..."
						textW = testW
						break
					}
				}
			}

			tx := calcAlignX(x, cellW, textW, col.Align)
			pdf.SetXY(tx, yBase)
			pdf.Text(val)
			x += colWidths[i]
		}
		y += bodyH
	}

	if err := pdf.WritePdf(outputPath); err != nil {
		return 0, fmt.Errorf("写入表格 PDF 失败: %w", err)
	}

	return totalH, nil
}

func calcColWidths(cols []ColumnDef, usableW float64) []float64 {
	widths := make([]float64, len(cols))
	var fixedTotal float64
	var flexCount int
	for _, col := range cols {
		if col.Width != nil {
			fixedTotal += *col.Width
		} else {
			flexCount++
		}
	}
	flexW := (usableW - fixedTotal) / float64(flexCount)
	if flexW < 30 {
		flexW = 30
	}
	for i, col := range cols {
		if col.Width != nil {
			widths[i] = *col.Width
		} else {
			widths[i] = flexW
		}
	}
	return widths
}

func calcAlignX(cellX, cellW, textW float64, align string) float64 {
	switch align {
	case "left":
		return cellX + 3
	case "right":
		return cellX + cellW - textW - 3
	default:
		return cellX + (cellW-textW)/2
	}
}

// InjectTableContent 将表格 PDF 的内容流和字体资源注入到主 PDF 页面
func InjectTableContent(tmpl *Template, tempTablePath string, pageW, extraH float64) error {
	tableCtx, err := api.ReadContextFile(tempTablePath)
	if err != nil {
		return fmt.Errorf("读取表格 PDF 失败: %w", err)
	}

	tp, _, _, err := tableCtx.PageDict(1, false)
	if err != nil {
		return fmt.Errorf("获取表格页面字典失败: %w", err)
	}

	mainPd, _, _, err := tmpl.ctx.PageDict(1, false)
	if err != nil {
		return fmt.Errorf("获取主页面字典失败: %w", err)
	}

	tRes, found := tp.Find("Resources")
	if !found {
		return fmt.Errorf("表格页面无 Resources")
	}
	tResDict, err := tableCtx.DereferenceDict(tRes)
	if err != nil {
		return fmt.Errorf("解引用表格 Resources 失败: %w", err)
	}

	tFont, found := tResDict.Find("Font")
	if !found {
		return fmt.Errorf("表格页面无 Font 资源")
	}
	tFontDict, err := tableCtx.DereferenceDict(tFont)
	if err != nil {
		return fmt.Errorf("解引用表格 Font 失败: %w", err)
	}

	mRes, found := mainPd.Find("Resources")
	if !found {
		return fmt.Errorf("主页面无 Resources")
	}
	mResDict, err := tmpl.ctx.DereferenceDict(mRes)
	if err != nil {
		return fmt.Errorf("解引用主 Resources 失败: %w", err)
	}

	mFontObj, found := mResDict.Find("Font")
	var mFontDict types.Dict
	if found {
		mFontDict, err = tmpl.ctx.DereferenceDict(mFontObj)
		if err != nil {
			return fmt.Errorf("解引用主 Font 失败: %w", err)
		}
	} else {
		mFontDict = types.NewDict()
	}

	fontNameMap := make(map[string]string)
	nameIdx := 0
	for k, v := range tFontDict {
		newName := fmt.Sprintf("FT%d", nameIdx)
		nameIdx++
		if ir, ok := v.(types.IndirectRef); ok {
			newRef, err := deepCopyRef(tableCtx.XRefTable, tmpl.ctx.XRefTable, ir)
			if err != nil {
				return fmt.Errorf("复制字体 %s 失败: %w", k, err)
			}
			mFontDict[newName] = *newRef
			fontNameMap[k] = newName
		}
	}
	mResDict["Font"] = mFontDict

	tContents, found := tp.Find("Contents")
	if !found {
		return fmt.Errorf("表格页面无 Contents")
	}

	var contentBytes []byte
	switch obj := tContents.(type) {
	case types.StreamDict:
		if err := obj.Decode(); err != nil {
			return fmt.Errorf("解码表格内容流失败: %w", err)
		}
		contentBytes = obj.Content
	case types.IndirectRef:
		sd, _, err := tableCtx.DereferenceStreamDict(obj)
		if err != nil {
			return fmt.Errorf("解引用表格内容流失败: %w", err)
		}
		if err := sd.Decode(); err != nil {
			return fmt.Errorf("解码表格内容流失败: %w", err)
		}
		contentBytes = sd.Content
	default:
		return fmt.Errorf("不支持的内容流类型: %T", tContents)
	}

	content := string(contentBytes)
	for oldName, newName := range fontNameMap {
		content = strings.ReplaceAll(content, "/"+oldName, "/"+newName)
	}
	contentBytes = []byte(content)

	if err := tmpl.ctx.XRefTable.AppendContent(mainPd, contentBytes); err != nil {
		return fmt.Errorf("追加表格内容流失败: %w", err)
	}

	return nil
}

func deepCopyRef(srcXRef, dstXRef *model.XRefTable, ir types.IndirectRef) (*types.IndirectRef, error) {
	objNr := ir.ObjectNumber.Value()
	genNr := ir.GenerationNumber.Value()

	entry, found := srcXRef.FindTableEntry(objNr, genNr)
	if !found || entry.Object == nil {
		return &ir, nil
	}

	copiedObj, err := deepCopyObject(srcXRef, dstXRef, entry.Object)
	if err != nil {
		return nil, err
	}

	return dstXRef.IndRefForNewObject(copiedObj)
}

func deepCopyObject(srcXRef, dstXRef *model.XRefTable, obj interface{}) (types.Object, error) {
	if obj == nil {
		return nil, nil
	}

	switch o := obj.(type) {
	case types.Dict:
		newDict := types.NewDict()
		for k, v := range o {
			cp, err := deepCopyValue(srcXRef, dstXRef, v)
			if err != nil {
				return nil, fmt.Errorf("copy dict key %s: %w", k, err)
			}
			newDict[k] = cp
		}
		return newDict, nil

	case types.StreamDict:
		cp := o.Clone().(types.StreamDict)
		for k, v := range o.Dict {
			replaced, err := deepCopyValue(srcXRef, dstXRef, v)
			if err != nil {
				return nil, fmt.Errorf("copy stream dict key %s: %w", k, err)
			}
			cp.Dict[k] = replaced
		}
		return cp, nil

	case types.IndirectRef:
		newRef, err := deepCopyRef(srcXRef, dstXRef, o)
		if err != nil {
			return nil, err
		}
		return *newRef, nil

	case types.Array:
		newArr := make(types.Array, len(o))
		for i, item := range o {
			cp, err := deepCopyValue(srcXRef, dstXRef, item)
			if err != nil {
				return nil, fmt.Errorf("copy array [%d]: %w", i, err)
			}
			newArr[i] = cp
		}
		return newArr, nil

	default:
		if objClone, ok := obj.(types.Object); ok {
			return objClone.Clone(), nil
		}
		return nil, fmt.Errorf("unsupported type for deep copy: %T", obj)
	}
}

func deepCopyValue(srcXRef, dstXRef *model.XRefTable, v interface{}) (types.Object, error) {
	return deepCopyObject(srcXRef, dstXRef, v)
}

// RenderTableAsImage 渲染表格为 RGBA 图片
func RenderTableAsImage(spec TableSpec) (*image.RGBA, float64, error) {
	fontData, err := os.ReadFile(spec.FontPath)
	if err != nil {
		return nil, 0, fmt.Errorf("读取字体失败: %w", err)
	}
	f, err := sfnt.Parse(fontData)
	if err != nil {
		return nil, 0, fmt.Errorf("解析字体失败: %w", err)
	}

	marginL, marginR := 20.0, 20.0
	usableW := spec.PageWidth - marginL - marginR

	colWidths := make([]float64, len(spec.Columns))
	var fixedTotal float64
	var flexCount int
	for _, col := range spec.Columns {
		if col.Width != nil {
			fixedTotal += *col.Width
		} else {
			flexCount++
		}
	}
	flexW := (usableW - fixedTotal) / float64(flexCount)
	if flexW < 30 {
		flexW = 30
	}

	totalW := marginL + marginR
	for i, col := range spec.Columns {
		if col.Width != nil {
			colWidths[i] = *col.Width
		} else {
			colWidths[i] = flexW
		}
		totalW += colWidths[i]
	}

	headerH := spec.HeaderFontSz + 12
	bodyH := spec.BodyFontSz + 10
	padding := 4.0
	totalH := headerH + float64(len(spec.Rows))*bodyH + 4

	canvasW := int(math.Ceil(totalW))
	canvasH := int(math.Ceil(totalH))
	canvas := image.NewRGBA(image.Rect(0, 0, canvasW, canvasH))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)

	yPos := 2.0
	draw.Draw(canvas, image.Rect(0, int(yPos), canvasW, int(yPos+headerH)),
		&image.Uniform{spec.HeaderColor}, image.Point{}, draw.Src)
	xPos := marginL
	for i, col := range spec.Columns {
		cw := colWidths[i]
		drawCellText(canvas, f, col.Label, spec.HeaderFontSz,
			xPos+padding, yPos+padding, cw-padding*2, headerH-padding*2, color.White)
		xPos += cw
	}
	yPos += headerH

	for _, row := range spec.Rows {
		draw.Draw(canvas, image.Rect(0, int(yPos), canvasW, int(yPos+bodyH)),
			&image.Uniform{spec.BodyColor}, image.Point{}, draw.Src)
		xPos = marginL
		for i, col := range spec.Columns {
			cw := colWidths[i]
			val := row[col.Key]
			cellColor := color.RGBA{160, 30, 30, 255}
			drawCellText(canvas, f, val, spec.BodyFontSz,
				xPos+padding, yPos+padding, cw-padding*2, bodyH-padding*2, cellColor)
			xPos += cw
		}
		yPos += bodyH
	}
	return canvas, totalH, nil
}

// TableSpec 表格配置
type TableSpec struct {
	Columns      []ColumnDef
	Rows         []TableRow
	HeaderColor  color.Color
	BodyColor    color.Color
	HeaderFontSz float64
	BodyFontSz   float64
	FontPath     string
	PageWidth    float64
}

func drawCellText(canvas *image.RGBA, f *sfnt.Font, text string, sizePt, x, y, maxW, maxH float64, clr color.Color) {
	if text == "" {
		return
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: sizePt, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return
	}
	defer face.Close()
	var totalW fixed.Int26_6
	var buf sfnt.Buffer
	for _, r := range text {
		gi, err := f.GlyphIndex(&buf, r)
		if err != nil {
			continue
		}
		ppem := fixed.I(int(sizePt))
		adv, err := f.GlyphAdvance(&buf, gi, ppem, font.HintingNone)
		if err != nil {
			continue
		}
		totalW += adv
	}
	startX := fixed.I(int(x + (maxW-float64(totalW)/64.0)/2))
	startY := fixed.I(int(y + (maxH-sizePt)/2 + sizePt))
	d := font.Drawer{
		Dst: canvas, Src: image.NewUniform(clr), Face: face,
		Dot: fixed.Point26_6{X: startX, Y: startY},
	}
	d.DrawString(text)
}
