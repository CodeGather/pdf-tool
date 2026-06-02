package pdf

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// ImagePosition 表示 PDF 页面上一个图片的位置
type ImagePosition struct {
	ObjNr int     // PDF 内部对象编号
	Name  string  // XObject 资源名称
	Page  int     // 页码（1-based）
	X     float64 // 页面上的 x 坐标（左下角原点）
	Y     float64 // 页面上的 y 坐标
	W     float64 // 显示宽度
	H     float64 // 显示高度
}

// Template 封装对模板 PDF 的操作
type Template struct {
	ctx  *model.Context
	path string
	imgs []ImagePosition
}

// OpenTemplate 打开模板 PDF 并提取所有图片位置
func OpenTemplate(fullPath string) (*Template, error) {
	ctx, err := api.ReadContextFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("打开模板 PDF 失败: %w", err)
	}

	t := &Template{ctx: ctx, path: fullPath}
	if err := t.extractImagePositions(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Template) extractImagePositions() error {
	for pageNum := 1; pageNum <= t.ctx.PageCount; pageNum++ {
		pd, _, _, err := t.ctx.PageDict(pageNum, false)
		if err != nil {
			return fmt.Errorf("PageDict %d: %w", pageNum, err)
		}

		resRaw, ok := pd.Find("Resources")
		if !ok {
			continue
		}
		resObj, err := t.ctx.Dereference(resRaw)
		if err != nil {
			continue
		}
		resDict, ok := resObj.(types.Dict)
		if !ok {
			continue
		}
		xoRaw, ok := resDict.Find("XObject")
		if !ok {
			continue
		}
		xoObj, err := t.ctx.Dereference(xoRaw)
		if err != nil {
			continue
		}
		xoDict, ok := xoObj.(types.Dict)
		if !ok {
			continue
		}

		nameToObjNr := make(map[string]int)
		for name, obj := range xoDict {
			ir, ok := obj.(types.IndirectRef)
			if !ok {
				continue
			}
			nameToObjNr[name] = ir.ObjectNumber.Value()
		}

		contents, ok := pd.Find("Contents")
		if !ok {
			continue
		}

		var streams []*types.StreamDict
		switch c := contents.(type) {
		case types.StreamDict:
			streams = append(streams, &c)
		case types.Array:
			for _, item := range c {
				ir, ok := item.(types.IndirectRef)
				if !ok {
					continue
				}
				sd, _, err := t.ctx.DereferenceStreamDict(ir)
				if err != nil || sd == nil {
					continue
				}
				streams = append(streams, sd)
			}
		case types.IndirectRef:
			sd, _, err := t.ctx.DereferenceStreamDict(c)
			if err == nil && sd != nil {
				streams = append(streams, sd)
			}
		}

		for _, sd := range streams {
			t.parseStream(sd, nameToObjNr, pageNum)
		}
	}
	return nil
}

func (t *Template) parseStream(sd *types.StreamDict, nameToObjNr map[string]int, pageNum int) {
	if len(sd.Content) == 0 {
		if err := sd.Decode(); err != nil {
			return
		}
	}
	t.parseContent(string(sd.Content), nameToObjNr, pageNum)
}

func (t *Template) parseContent(content string, nameToObjNr map[string]int, pageNum int) {
	ctmStack := [][6]float64{}
	ctmStack = append(ctmStack, [6]float64{1, 0, 0, 1, 0, 0})
	ctm := ctmStack[0]

	toks := tokenize(content)

	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "q":
			m := ctm
			ctmStack = append(ctmStack, m)
		case "Q":
			if len(ctmStack) > 1 {
				ctmStack = ctmStack[:len(ctmStack)-1]
				ctm = ctmStack[len(ctmStack)-1]
			}
		case "cm":
			if i >= 6 {
				a, _ := strconv.ParseFloat(toks[i-6], 64)
				b, _ := strconv.ParseFloat(toks[i-5], 64)
				c, _ := strconv.ParseFloat(toks[i-4], 64)
				d, _ := strconv.ParseFloat(toks[i-3], 64)
				e, _ := strconv.ParseFloat(toks[i-2], 64)
				f, _ := strconv.ParseFloat(toks[i-1], 64)
				ctm = [6]float64{
					ctm[0]*a + ctm[1]*c,
					ctm[0]*b + ctm[1]*d,
					ctm[2]*a + ctm[3]*c,
					ctm[2]*b + ctm[3]*d,
					ctm[4]*a + ctm[5]*c + e,
					ctm[4]*b + ctm[5]*d + f,
				}
				ctmStack[len(ctmStack)-1] = ctm
			}
		case "Do":
			if i >= 1 {
				name := toks[i-1]
				objNr, exists := nameToObjNr[name]
				if exists {
					t.imgs = append(t.imgs, ImagePosition{
						ObjNr: objNr,
						Name:  name,
						Page:  pageNum,
						X:     ctm[4],
						Y:     ctm[5],
						W:     ctm[0],
						H:     ctm[3],
					})
				}
			}
		}
	}
}

func tokenize(s string) []string {
	var toks []string
	var cur strings.Builder
	readingName := false

	for _, r := range s {
		switch {
		case r == '(' || r == ')':
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
			readingName = false
			cur.WriteRune(r)
			toks = append(toks, cur.String())
			cur.Reset()
		case r == '<':
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
			readingName = false
			cur.WriteRune(r)
		case r == '>':
			cur.WriteRune(r)
			toks = append(toks, cur.String())
			cur.Reset()
			readingName = false
		case r == ' ' || r == '\n' || r == '\r' || r == '\t':
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
			readingName = false
		case r == '/':
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
			toks = append(toks, "/")
			readingName = true
		default:
			if !readingName && cur.Len() > 0 && isOperatorChar(r) != isOperatorChar(rune(cur.String()[0])) {
				toks = append(toks, cur.String())
				cur.Reset()
			}
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

func isOperatorChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '*' || r == '\'' || r == '"'
}

// FindImageByRect 通过坐标匹配图片（容差 tolerance）
func (t *Template) FindImageByRect(tx, ty, tw, th, tolerance float64) *ImagePosition {
	for _, img := range t.imgs {
		if math.Abs(img.X-tx) <= tolerance &&
			math.Abs(img.Y-ty) <= tolerance &&
			math.Abs(img.W-tw) <= tolerance &&
			math.Abs(img.H-th) <= tolerance {
			cp := img
			return &cp
		}
	}
	return nil
}

// FindImageByObjNr 通过对象编号查找图片位置
func (t *Template) FindImageByObjNr(objNr int) *ImagePosition {
	for _, img := range t.imgs {
		if img.ObjNr == objNr {
			cp := img
			return &cp
		}
	}
	return nil
}

// ImageToStreamDictJPEG 将 image.Image 转为 JPEG 压缩的 PDF 图片流
func ImageToStreamDictJPEG(img image.Image, quality int) *types.StreamDict {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})

	sd := types.StreamDict{
		Dict:    types.NewDict(),
		Content: buf.Bytes(),
	}
	sd.Dict["Type"] = types.Name("XObject")
	sd.Dict["Subtype"] = types.Name("Image")
	sd.Dict["Width"] = types.Integer(w)
	sd.Dict["Height"] = types.Integer(h)
	sd.Dict["ColorSpace"] = types.Name("DeviceRGB")
	sd.Dict["BitsPerComponent"] = types.Integer(8)
	sd.Dict["Filter"] = types.Name("DCTDecode")
	return &sd
}

// ReplaceStreamDict 替换指定 objNr 的图片流
func (t *Template) ReplaceStreamDict(objNr int, sd *types.StreamDict) error {
	if err := sd.Encode(); err != nil {
		return fmt.Errorf("编码图片流失败: %w", err)
	}
	genNr := 0
	entry, ok := t.ctx.FindTableEntry(objNr, genNr)
	if !ok {
		return fmt.Errorf("未找到 objNr=%d", objNr)
	}
	entry.Object = *sd
	return nil
}

// DrawRectBorder 在指定图片位置上绘制独立矩形边框
func (t *Template) DrawRectBorder(imgPos ImagePosition, lineWidth float64, r, g, b float64) error {
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("q\n"))
	buf.WriteString(fmt.Sprintf("%.4f %.4f %.4f RG\n", r, g, b))
	buf.WriteString(fmt.Sprintf("%.4f w\n", lineWidth))
	buf.WriteString(fmt.Sprintf("%.4f %.4f %.4f %.4f re\n", imgPos.X, imgPos.Y, imgPos.W, imgPos.H))
	buf.WriteString("S\nQ\n")

	sd := types.StreamDict{
		Dict:    types.NewDict(),
		Content: []byte(buf.String()),
	}
	if err := sd.Encode(); err != nil {
		return fmt.Errorf("编码边框内容流失败: %w", err)
	}

	objNr, err := t.ctx.XRefTable.InsertObject(sd)
	if err != nil {
		return fmt.Errorf("插入边框流对象失败: %w", err)
	}

	ir := types.IndirectRef{
		ObjectNumber:     types.Integer(objNr),
		GenerationNumber: types.Integer(0),
	}

	pageNum := imgPos.Page
	pageDict, _, _, err := t.ctx.PageDict(pageNum, false)
	if err != nil {
		return fmt.Errorf("获取页面 %d 字典失败: %w", pageNum, err)
	}

	contents, ok := pageDict.Find("Contents")
	if !ok {
		return fmt.Errorf("页面 %d 无 Contents", pageNum)
	}

	switch c := contents.(type) {
	case types.StreamDict:
		pageDict.Update("Contents", types.Array{ir})
	case types.Array:
		pageDict.Update("Contents", append(c, ir))
	case types.IndirectRef:
		refd, err := t.ctx.Dereference(c)
		if err != nil {
			return fmt.Errorf("解引用 Contents 失败: %w", err)
		}
		switch v := refd.(type) {
		case types.StreamDict:
			pageDict.Update("Contents", types.Array{c, ir})
		case types.Array:
			pageDict.Update("Contents", append(v, ir))
		default:
			return fmt.Errorf("不支持的 Contents 类型: %T", v)
		}
	}

	return nil
}

// WriteToFile 写入修改后的 PDF
func (t *Template) WriteToFile(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %w", err)
	}
	defer f.Close()
	return api.WriteContext(t.ctx, f)
}

// Context 返回底层的 pdfcpu Context
func (t *Template) Context() *model.Context {
	return t.ctx
}

// SetContext 替换底层的 pdfcpu Context
func (t *Template) SetContext(ctx *model.Context) {
	t.ctx = ctx
}

// Path 返回模板 PDF 路径
func (t *Template) Path() string {
	return t.path
}

// PageSize 返回第 n 页的宽度和高度
func (t *Template) PageSize(pageNum int) (width, height float64, err error) {
	pd, _, _, err := t.ctx.PageDict(pageNum, false)
	if err != nil {
		return 0, 0, err
	}
	mb, ok := pd.Find("MediaBox")
	if !ok {
		return 0, 0, fmt.Errorf("no MediaBox")
	}
	rect, ok := mb.(types.Array)
	if !ok || len(rect) < 4 {
		return 0, 0, fmt.Errorf("invalid MediaBox")
	}
	urx := toFloat(rect[2])
	ury := toFloat(rect[3])
	return urx, ury, nil
}

// ImagePositions 返回所有已提取的图片位置
func (t *Template) ImagePositions() []ImagePosition {
	return t.imgs
}

// Close 释放资源
func (t *Template) Close() {
	t.ctx = nil
}

func toFloat(o types.Object) float64 {
	switch v := o.(type) {
	case types.Float:
		return float64(v)
	case types.Integer:
		return float64(v)
	default:
		return 0
	}
}
