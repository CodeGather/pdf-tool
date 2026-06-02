//go:build !gofitz

package cmd

import (
	"errors"
	"image"
)

// 默认实现（无 go-fitz）：所有操作返回错误。
// 主渲染引擎为 mutool，此回退仅在 mutool 不可用时被调用。

type fitzDocStub struct{}

func openFitzDocument(inputFile string) (fitzDocument, error) {
	return nil, errors.New("go-fitz 未启用（使用 -tags gofitz 编译以启用）")
	// 注意：fitzDocStub 未使用，因为我们需要返回 error
	// 但仍然定义它以满足接口
}

func (d *fitzDocStub) NumPage() int                            { return 0 }
func (d *fitzDocStub) ImageDPI(page int, dpi float64) (image.Image, error) { return nil, errors.New("go-fitz 未启用") }
func (d *fitzDocStub) Close()                                   {}