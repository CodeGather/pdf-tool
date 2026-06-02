package util

import "regexp"

var NaturalTokenPattern = regexp.MustCompile(`\d+|\D+`)
var ClipOperatorPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_/])W\*?([^A-Za-z0-9_]|$)`)
var DrawingOperatorPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_/])(m|l|c|v|y|h|S|s|f|F|B|b|B\*|b\*)([^A-Za-z0-9_]|$)`)

const MaxClipOperatorCountForCrop = 8

// NaturalLess 实现自然排序（natural sort）的比较函数。
// 将字符串按数字和非数字片段分拆，数字片段按数值比较，非数字按字符串比较。
// 例如："page2" < "page10"（而非字典序的 "page10" < "page2"）。
func NaturalLess(left, right string) bool {
	leftParts := NaturalTokenPattern.FindAllString(left, -1)
	rightParts := NaturalTokenPattern.FindAllString(right, -1)
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		leftPart := leftParts[index]
		rightPart := rightParts[index]
		leftIsNumber := leftPart[0] >= '0' && leftPart[0] <= '9'
		rightIsNumber := rightPart[0] >= '0' && rightPart[0] <= '9'
		if leftIsNumber && rightIsNumber {
			leftTrimmed := trimLeftZeros(leftPart)
			rightTrimmed := trimLeftZeros(rightPart)
			if len(leftTrimmed) != len(rightTrimmed) {
				return len(leftTrimmed) < len(rightTrimmed)
			}
			if leftTrimmed != rightTrimmed {
				return leftTrimmed < rightTrimmed
			}
			if len(leftPart) != len(rightPart) {
				return len(leftPart) < len(rightPart)
			}
			continue
		}
		if leftPart != rightPart {
			return leftPart < rightPart
		}
	}
	return len(leftParts) < len(rightParts)
}

func trimLeftZeros(s string) string {
	result := s
	for len(result) > 1 && result[0] == '0' {
		result = result[1:]
	}
	if result == "" {
		return "0"
	}
	return result
}

// HasClipOperator 检查内容流中是否包含裁剪路径操作符。
func HasClipOperator(content []byte) bool {
	if len(ClipOperatorPattern.FindAllIndex(content, -1)) > MaxClipOperatorCountForCrop {
		return false
	}
	return ClipOperatorPattern.Match(content) && DrawingOperatorPattern.Match(content)
}