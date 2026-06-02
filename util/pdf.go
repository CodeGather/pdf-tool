package util

import (
	"strconv"
	"strings"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

// DictAt 从字典中安全获取子字典。
func DictAt(d types.Dict, key string) types.Dict {
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

// ExtractRect 从 PDF 内容流中提取最后一个 re 矩形的宽高。
func ExtractRect(content string) (float64, float64) {
	idx := strings.LastIndex(content, "re")
	if idx < 2 {
		return 0, 0
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 4 {
		return 0, 0
	}
	w, _ := strconv.ParseFloat(fields[len(fields)-2], 64)
	h, _ := strconv.ParseFloat(fields[len(fields)-1], 64)
	return w, h
}

// ExtractCM 从 PDF 内容流中提取最后一个 cm 矩阵的 a 和 d 值。
func ExtractCM(content string) (float64, float64) {
	idx := strings.LastIndex(content, "cm")
	if idx < 2 {
		return 0, 0
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 6 {
		return 0, 0
	}
	a, _ := strconv.ParseFloat(fields[len(fields)-6], 64)
	d, _ := strconv.ParseFloat(fields[len(fields)-3], 64)
	return a, d
}

// ExtractImageFullCM 从 PDF 内容流中提取最后一个 cm 矩阵的全部 6 个值。
func ExtractImageFullCM(content string) (a, b, c, d, e, f float64, ok bool) {
	idx := strings.LastIndex(content, "cm")
	if idx < 2 {
		return 0, 0, 0, 0, 0, 0, false
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 6 {
		return 0, 0, 0, 0, 0, 0, false
	}
	a, _ = strconv.ParseFloat(fields[len(fields)-6], 64)
	b, _ = strconv.ParseFloat(fields[len(fields)-5], 64)
	c, _ = strconv.ParseFloat(fields[len(fields)-4], 64)
	d, _ = strconv.ParseFloat(fields[len(fields)-3], 64)
	e, _ = strconv.ParseFloat(fields[len(fields)-2], 64)
	f, _ = strconv.ParseFloat(fields[len(fields)-1], 64)
	return a, b, c, d, e, f, true
}

// GetPageContentString 获取页面的内容流文本。
// 支持单个 StreamDict 和多个 StreamDict 组成的数组（如 Contents [8 0 R 9 0 R ...]）。
func GetPageContentString(ctx *model.Context, pageDict types.Dict) string {
	obj, err := ctx.Dereference(pageDict["Contents"])
	if err != nil || obj == nil {
		return ""
	}

	// 处理单个 StreamDict 的情况
	if sd, ok := obj.(types.StreamDict); ok {
		if len(sd.Content) == 0 && len(sd.Raw) > 0 {
			if decodeErr := sd.Decode(); decodeErr != nil {
				return ""
			}
		}
		return string(sd.Content)
	}

	// 处理数组 Contents [ref1 ref2 ...] 的情况
	if arr, ok := obj.(types.Array); ok {
		var parts []string
		for _, item := range arr {
			elem, err := ctx.Dereference(item)
			if err != nil || elem == nil {
				continue
			}
			sd, ok := elem.(types.StreamDict)
			if !ok {
				continue
			}
			if len(sd.Content) == 0 && len(sd.Raw) > 0 {
				if decodeErr := sd.Decode(); decodeErr != nil {
					continue
				}
			}
			parts = append(parts, string(sd.Content))
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// extractRect 从 PDF 内容流中提取最后一个 re 操作符的矩形尺寸 (w, h)。
// 在 PDF 中 re 的格式: "x y w h re"
func extractRect(content string) (float64, float64) {
	idx := strings.LastIndex(content, "re")
	if idx < 2 {
		return 0, 0
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 4 {
		return 0, 0
	}
	w, _ := strconv.ParseFloat(fields[len(fields)-2], 64)
	h, _ := strconv.ParseFloat(fields[len(fields)-1], 64)
	return w, h
}

// extractCM 从 PDF 内容流中提取最后一个 cm 矩阵的 a 和 d 值。
// 在 PDF 中 cm 的格式: "a b c d e f cm"
func extractCM(content string) (float64, float64) {
	idx := strings.LastIndex(content, "cm")
	if idx < 2 {
		return 0, 0
	}
	fields := strings.Fields(content[:idx])
	if len(fields) < 6 {
		return 0, 0
	}
	a, _ := strconv.ParseFloat(fields[len(fields)-6], 64)
	d, _ := strconv.ParseFloat(fields[len(fields)-3], 64)
	return a, d
}

// HasPageContentClip 检查页面的内容流是否包含 clip 操作符。
func HasPageContentClip(ctx *model.Context, pageDict types.Dict) bool {
	content := GetPageContentString(ctx, pageDict)
	if content == "" {
		return false
	}

	// 检测 W n / W* n（clip + 结束路径）。PDF 中 W 总是跟 n 一起出现。
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

// PDFHasClipPath 检查 PDF 任意页是否存在裁剪路径。
func PDFHasClipPath(ctx *model.Context) (bool, error) {
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		pageDict, _, _, err := ctx.PageDict(pageNr, false)
		if err != nil {
			return false, err
		}
		content, err := ctx.PageContent(pageDict, pageNr)
		if err != nil {
			if err == model.ErrNoContent {
				continue
			}
			return false, err
		}
		if HasClipOperator(content) {
			return true, nil
		}
	}
	return false, nil
}

// PDFHasMultipleImageObjects 检查是否存在同一页里有多个图片对象。
func PDFHasMultipleImageObjects(ctx *model.Context) (bool, error) {
	for pageNr := 1; pageNr <= ctx.PageCount; pageNr++ {
		objNrs := pdfcpu.ImageObjNrs(ctx, pageNr)
		if len(objNrs) > 1 {
			return true, nil
		}
	}
	return false, nil
}
