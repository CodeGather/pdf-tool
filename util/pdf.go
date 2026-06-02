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

// GetPageContentString 获取页面解码后的内容流文本。
func GetPageContentString(ctx *model.Context, pageDict types.Dict) string {
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

// HasPageContentClip 检查页面的内容流是否包含 clip 操作符。
func HasPageContentClip(ctx *model.Context, pageDict types.Dict) bool {
	content := GetPageContentString(ctx, pageDict)
	if content == "" {
		return false
	}
	clipCount := 0
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "W" || trimmed == "W*" {
			clipCount++
		}
	}
	hasClip := strings.Contains(content, " W ") || strings.Contains(content, " W* ") ||
		strings.HasSuffix(strings.TrimSpace(content), "W") ||
		strings.HasSuffix(strings.TrimSpace(content), "W*")
	if !hasClip {
		return false
	}
	if clipCount > MaxClipOperatorCountForCrop {
		return false
	}
	hasDraw := strings.Contains(content, " re ") ||
		strings.Contains(content, " m ") ||
		strings.Contains(content, " l ")
	return hasDraw
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
