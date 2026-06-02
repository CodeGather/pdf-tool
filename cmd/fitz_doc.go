// Package main 的 fitz 文档抽象层。
// fitzWrapper 封装了对 MuPDF 文档的渲染操作，支持两种实现：
//   - go-fitz 实现（-tags gofitz）：通过 CGo 或 purego 调用 MuPDF
//   - 回退实现（默认）：始终返回错误，由 mutool 作为主渲染引擎

package cmd

import "image"

// fitzWrapper 封装 fitz.Document 的核心操作。
type fitzWrapper struct {
	doc fitzDocument
}

// fitzDocument 是 MuPDF 文档的抽象接口。
type fitzDocument interface {
	NumPage() int
	ImageDPI(page int, dpi float64) (image.Image, error)
	Close()
}

// openFitzDoc 打开 PDF 文件并返回 fitzWrapper。
// 实际实现由 build tag 决定：
//   - 默认：返回错误（需要 -tags gofitz 才能使用）
//   - gofitz：使用 go-fitz 库
func openFitzDoc(inputFile string) (*fitzWrapper, error) {
	doc, err := openFitzDocument(inputFile)
	if err != nil {
		return nil, err
	}
	return &fitzWrapper{doc: doc}, nil
}

func (w *fitzWrapper) NumPage() int {
	return w.doc.NumPage()
}

func (w *fitzWrapper) ImageDPI(page int, dpi float64) (image.Image, error) {
	return w.doc.ImageDPI(page, dpi)
}

func (w *fitzWrapper) Close() {
	w.doc.Close()
}