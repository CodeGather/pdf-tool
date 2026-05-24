//go:build gofitz

package main

import (
	"image"

	"github.com/gen2brain/go-fitz"
)

// go-fitz 实现：通过 CGo 或 purego 调用 MuPDF 渲染。

type fitzDocGo struct {
	doc *fitz.Document
}

func openFitzDocument(inputFile string) (fitzDocument, error) {
	doc, err := fitz.New(inputFile)
	if err != nil {
		return nil, err
	}
	return &fitzDocGo{doc: doc}, nil
}

func (d *fitzDocGo) NumPage() int {
	return d.doc.NumPage()
}

func (d *fitzDocGo) ImageDPI(page int, dpi float64) (image.Image, error) {
	return d.doc.ImageDPI(page, int(dpi))
}

func (d *fitzDocGo) Close() {
	d.doc.Close()
}