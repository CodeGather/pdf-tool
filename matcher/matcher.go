package matcher

import (
	"math"
	"strings"
	"unicode/utf8"

	"example.com/pdf-tool/model"
)

// ---------- 方向判定 ----------

type ImgDirection int

const (
	DirLandscape ImgDirection = iota // 横图: W/H > 1
	DirPortrait                      // 竖图: W/H < 1
	DirSquare                        // 正方形: W/H ≈ 1
)

const squareTolerance = 0.01

func ClassifyDirection(ratio float64) ImgDirection {
	if math.Abs(ratio-1) <= squareTolerance {
		return DirSquare
	}
	if ratio > 1 {
		return DirLandscape
	}
	return DirPortrait
}

// ---------- 核心匹配算法 ----------

type MatchResult struct {
	Image       model.ImageMeta
	Found       bool
	TargetW     float64
	TargetH     float64
	ImgRatio    float64
	TargetRatio float64
}

func SelectBestImage(targetW, targetH float64, images []model.ImageMeta) MatchResult {
	result := MatchResult{
		TargetW:     targetW,
		TargetH:     targetH,
		TargetRatio: targetW / targetH,
	}

	if len(images) == 0 {
		return result
	}

	if len(images) == 1 {
		img := images[0]
		result.Image = img
		result.Found = true
		result.ImgRatio = img.Width / img.Height
		return result
	}

	targetRatio := targetW / targetH
	targetDir := ClassifyDirection(targetRatio)

	var candidates []model.ImageMeta

	switch targetDir {
	case DirLandscape:
		for _, img := range images {
			if img.Width/img.Height > 1 {
				candidates = append(candidates, img)
			}
		}
	case DirPortrait:
		for _, img := range images {
			if img.Width/img.Height < 1 {
				candidates = append(candidates, img)
			}
		}
	case DirSquare:
		for _, img := range images {
			imgRatio := img.Width / img.Height
			if ClassifyDirection(imgRatio) == DirSquare {
				candidates = append(candidates, img)
			}
		}
		if len(candidates) > 0 {
			return pickClosest(candidates, targetRatio, result)
		}
		var lands, ports []model.ImageMeta
		for _, img := range images {
			imgRatio := img.Width / img.Height
			if imgRatio > 1 {
				lands = append(lands, img)
			} else if imgRatio < 1 {
				ports = append(ports, img)
			}
		}
		if len(lands) > 0 && len(ports) > 0 {
			bestLand := pickClosest(lands, targetRatio, MatchResult{})
			bestPort := pickClosest(ports, targetRatio, MatchResult{})
			landDiff := math.Abs(bestLand.ImgRatio - targetRatio)
			portDiff := math.Abs(bestPort.ImgRatio - targetRatio)
			if landDiff <= portDiff {
				return bestLand
			}
			return bestPort
		}
		if len(lands) > 0 {
			candidates = lands
		} else if len(ports) > 0 {
			candidates = ports
		}
	}

	if len(candidates) == 0 {
		candidates = images
	}

	return pickClosest(candidates, targetRatio, result)
}

func pickClosest(candidates []model.ImageMeta, targetRatio float64, base MatchResult) MatchResult {
	best := base
	bestDiff := math.MaxFloat64

	for _, img := range candidates {
		imgRatio := img.Width / img.Height
		diff := math.Abs(imgRatio - targetRatio)
		if diff < bestDiff {
			bestDiff = diff
			best.Image = img
			best.Found = true
			best.ImgRatio = imgRatio
		}
	}
	return best
}

// ---------- 灯位备注 → file-data key 模糊匹配 ----------

func cleanKey(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '　':
			return ' '
		case r >= '０' && r <= '９':
			return '０' + (r - '０')
		case r >= 'Ａ' && r <= 'Ｚ':
			return 'Ａ' + (r - 'Ａ')
		case r >= 'ａ' && r <= 'ｚ':
			return 'ａ' + (r - 'ａ')
		case isHan(r):
			return r
		case isWhitespace(r):
			return -1
		default:
			return r
		}
	}, s)
	return strings.ToLower(strings.ReplaceAll(s, " ", ""))
}

func isHan(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF) || (r >= 0xF900 && r <= 0xFAFF)
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == 0x00A0
}

func MatchFileDataKey(lampNote string, fileData model.FileData) (string, bool) {
	cleanedNote := cleanKey(lampNote)

	for key := range fileData {
		cleanedKey := cleanKey(key)
		if cleanedKey == cleanedNote {
			return key, true
		}
		keyNoExt := strings.TrimSuffix(cleanedKey, ".pdf")
		if keyNoExt == cleanedNote {
			return key, true
		}
	}

	for key := range fileData {
		cleanedKey := cleanKey(key)
		keyNoExt := strings.TrimSuffix(cleanedKey, ".pdf")
		if levenshtein(keyNoExt, cleanedNote) <= 2 {
			return key, true
		}
	}

	return "", false
}

func levenshtein(a, b string) int {
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	ra := make([]rune, 0, la)
	for _, r := range a {
		ra = append(ra, r)
	}
	rb := make([]rune, 0, lb)
	for _, r := range b {
		rb = append(rb, r)
	}

	if la < lb {
		ra, rb = rb, ra
		la, lb = lb, la
	}

	prev := make([]int, lb+1)
	curr := make([]int, lb+1)

	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}