package pdf

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

func loadFontFace(fontPath string) (*sfnt.Font, error) {
	data, err := os.ReadFile(fontPath)
	if err != nil {
		return nil, err
	}
	return sfnt.Parse(data)
}

func rgbaToColor(r, g, b, a float64) color.Color {
	return color.RGBA{
		R: uint8(math.Round(r * 255)),
		G: uint8(math.Round(g * 255)),
		B: uint8(math.Round(b * 255)),
		A: uint8(math.Round(a * 255)),
	}
}

// ScaleImageContain 将图片缩放到目标尺寸（contain 模式，保持比例）
func ScaleImageContain(img image.Image, targetW, targetH float64) image.Image {
	bounds := img.Bounds()
	srcW := float64(bounds.Dx())
	srcH := float64(bounds.Dy())

	scaleW := targetW / srcW
	scaleH := targetH / srcH
	scale := math.Min(scaleW, scaleH)

	newW := int(math.Round(srcW * scale))
	newH := int(math.Round(srcH * scale))

	canvas := image.NewRGBA(image.Rect(0, 0, int(targetW), int(targetH)))

	offsetX := (int(targetW) - newW) / 2
	offsetY := (int(targetH) - newH) / 2

	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			srcX := int(math.Round(float64(x) / scale))
			srcY := int(math.Round(float64(y) / scale))
			if srcX >= bounds.Dx() {
				srcX = bounds.Dx() - 1
			}
			if srcY >= bounds.Dy() {
				srcY = bounds.Dy() - 1
			}
			canvas.Set(offsetX+x, offsetY+y, img.At(bounds.Min.X+srcX, bounds.Min.Y+srcY))
		}
	}

	return canvas
}

// DrawTextOnTop 在图片上方绘制文本，自动缩字号以适应图片宽度
func DrawTextOnTop(img image.Image, text string, r, g, b, a float64, fontSizePt float64, fontPath string) image.Image {
	if text == "" {
		return img
	}

	bounds := img.Bounds()
	imgW := bounds.Dx()

	f, err := loadFontFace(fontPath)
	if err != nil {
		return img
	}

	var sfntBuf sfnt.Buffer
	buf := &sfntBuf
	adjustedSize := fontSizePt
	for adjustedSize >= 4 {
		totalW := measureSimple(f, buf, text, adjustedSize)
		if totalW <= float64(imgW) {
			break
		}
		adjustedSize -= 0.5
	}
	if adjustedSize < 4 {
		adjustedSize = 4
	}

	lineHeight := adjustedSize + 4
	paddingTop := 2.0
	textAreaH := int(math.Ceil(lineHeight + paddingTop))

	newH := bounds.Dy() + textAreaH
	canvas := image.NewRGBA(image.Rect(0, 0, imgW, newH))

	draw.Draw(canvas,
		image.Rect(0, textAreaH, imgW, newH),
		img, image.Point{}, draw.Src)

	drawTextAt(canvas, f, buf, text, adjustedSize, paddingTop, imgW, rgbaToColor(r, g, b, a))

	return canvas
}

func measureSimple(f *sfnt.Font, buf *sfnt.Buffer, text string, sizePt float64) float64 {
	ppem := fixed.I(int(sizePt))
	var totalW fixed.Int26_6
	for _, r := range text {
		gi, err := f.GlyphIndex(buf, r)
		if err != nil || gi == 0 {
			totalW += ppem / 2
			continue
		}
		advance, err := f.GlyphAdvance(buf, gi, ppem, font.HintingNone)
		if err != nil {
			totalW += ppem / 2
			continue
		}
		totalW += advance
	}
	return float64(totalW) / 64.0
}

func drawTextAt(canvas *image.RGBA, f *sfnt.Font, buf *sfnt.Buffer, text string, sizePt float64, paddingTop float64, canvasW int, clr color.Color) {
	ppem := fixed.I(int(sizePt))

	var totalW fixed.Int26_6
	for _, r := range text {
		gi, err := f.GlyphIndex(buf, r)
		if err != nil {
			continue
		}
		adv, err := f.GlyphAdvance(buf, gi, ppem, font.HintingNone)
		if err != nil {
			continue
		}
		totalW += adv
	}

	startX := fixed.I(canvasW/2) - totalW/2
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: sizePt, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return
	}
	defer face.Close()

	d := font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot: fixed.Point26_6{
			X: startX,
			Y: fixed.I(int(paddingTop)) + ppem,
		},
	}
	d.DrawString(text)
}

// EncodePNG 将图片编码为 PNG bytes
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}