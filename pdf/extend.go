package pdf

import (
	"fmt"
	"math"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var boxKeys = []string{"MediaBox", "CropBox", "TrimBox", "BleedBox", "ArtBox"}

// ExtendPageHeight 向上延长页面：所有内容上移 extraHeight，顶部留出新空间
func (t *Template) ExtendPageHeight(extraHeight float64) error {
	if extraHeight <= 0 {
		return nil
	}

	for pageNum := 1; pageNum <= t.ctx.PageCount; pageNum++ {
		pd, _, _, err := t.ctx.PageDict(pageNum, false)
		if err != nil {
			return fmt.Errorf("PageDict %d: %w", pageNum, err)
		}

		for _, key := range boxKeys {
			v, found := pd.Find(key)
			if !found {
				continue
			}
			arr, ok := v.(types.Array)
			if !ok || len(arr) < 4 {
				continue
			}
			ury := toFloat(arr[3])
			arr[3] = types.Float(ury + extraHeight)
		}

		contents, found := pd.Find("Contents")
		if !found {
			continue
		}

		type streamTarget struct {
			entry *model.XRefTableEntry
			objNr int
		}
		var allContent []byte
		var targets []streamTarget
		xt := t.ctx.XRefTable

		switch obj := contents.(type) {
		case types.StreamDict:
			continue

		case types.IndirectRef:
			objNr := obj.ObjectNumber.Value()
			genNr := obj.GenerationNumber.Value()
			entry, found := xt.FindTableEntry(objNr, genNr)
			if !found || entry.Object == nil {
				continue
			}
			// IndRef 可能指向 Array（多内容流）或 StreamDict（单流）
			if arr, ok := entry.Object.(types.Array); ok {
				for _, item := range arr {
					ir, ok := item.(types.IndirectRef)
					if !ok {
						continue
					}
					objNr := ir.ObjectNumber.Value()
					genNr := ir.GenerationNumber.Value()
					entry, found := xt.FindTableEntry(objNr, genNr)
					if !found || entry.Object == nil {
						continue
					}
					sd, ok := entry.Object.(types.StreamDict)
					if !ok {
						continue
					}
					if err := sd.Decode(); err != nil {
						return fmt.Errorf("解码子内容流失败: %w", err)
					}
					allContent = append(allContent, sd.Content...)
					allContent = append(allContent, '\n')
					targets = append(targets, streamTarget{entry: entry, objNr: objNr})
				}
			} else if sd, ok := entry.Object.(types.StreamDict); ok {
				if err := sd.Decode(); err != nil {
					return fmt.Errorf("解码内容流失败: %w", err)
				}
				allContent = sd.Content
				targets = append(targets, streamTarget{entry: entry, objNr: objNr})
			}

		case types.Array:
			for _, item := range obj {
				ir, ok := item.(types.IndirectRef)
				if !ok {
					continue
				}
				objNr := ir.ObjectNumber.Value()
				genNr := ir.GenerationNumber.Value()
				entry, found := xt.FindTableEntry(objNr, genNr)
				if !found || entry.Object == nil {
					continue
				}
				sd, ok := entry.Object.(types.StreamDict)
				if !ok {
					continue
				}
				if err := sd.Decode(); err != nil {
					return fmt.Errorf("解码子内容流失败: %w", err)
				}
				allContent = append(allContent, sd.Content...)
				allContent = append(allContent, '\n')
				targets = append(targets, streamTarget{entry: entry, objNr: objNr})
			}
		}

		if len(allContent) == 0 || len(targets) == 0 {
			continue
		}

		extra := math.Round(extraHeight)
		wrapped := fmt.Sprintf(" q 1 0 0 1 0 %f cm \n", extra)
		wrapped += string(allContent)
		wrapped += "\n Q "

		first := targets[0]
		firstSD := first.entry.Object.(types.StreamDict)
		firstSD.Content = []byte(wrapped)
		if err := firstSD.Encode(); err != nil {
			return fmt.Errorf("重新编码内容流失败: %w", err)
		}
		first.entry.Object = firstSD

		for i := 1; i < len(targets); i++ {
			tgt := targets[i]
			emptySD := tgt.entry.Object.(types.StreamDict)
			emptySD.Content = []byte(" ")
			if err := emptySD.Encode(); err != nil {
				return err
			}
			tgt.entry.Object = emptySD
		}
	}
	return nil
}