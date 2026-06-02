package util

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var (
	// ImageMetaJSONEnabled 控制 traceProgress 的 JSON 输出格式
	ImageMetaJSONEnabled bool
	// ProgressMu 保护进度输出
	ProgressMu sync.Mutex
)

// TraceProgress 显示进度百分比（0-100）。
func TraceProgress(enabled bool, progress int, message ...string) {
	if !enabled {
		return
	}
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	msg := ""
	if len(message) > 0 {
		msg = message[0]
	}
	if ImageMetaJSONEnabled {
		b, _ := json.Marshal(map[string]interface{}{
			"message": msg,
			"percent": progress,
		})
		os.Stderr.Write(b)
		os.Stderr.Write([]byte{'\n'})
	} else {
		fmt.Fprintln(os.Stderr, progress)
	}
}

// IsCMYKJPEG 检测 JPEG 数据是否为 CMYK 色彩空间。
func IsCMYKJPEG(data []byte) bool {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return false
	}
	i := 2
	for i < len(data)-1 {
		if data[i] != 0xFF {
			i++
			continue
		}
		if data[i+1] == 0x00 {
			i += 2
			continue
		}
		marker := data[i+1]
		if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 {
			if i+9 < len(data) {
				return data[i+9] == 4
			}
			return false
		}
		if marker == 0xD8 || marker == 0xD9 {
			i += 2
			continue
		}
		if marker >= 0xD0 && marker <= 0xD7 {
			i += 2
			continue
		}
		if i+3 < len(data) {
			segLen := int(data[i+2])<<8 | int(data[i+3])
			i += 2 + segLen
		} else {
			break
		}
	}
	return false
}

// ConvertCMYKJPEGToOutput 把 CMYK JPEG 转成正确的 RGB 图片（PNG）。
func ConvertCMYKJPEGToOutput(data []byte, outPath string) (err error) {
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	tmpJPEG, err := os.CreateTemp(dir, ".cmyk-*.jpg")
	if err != nil {
		return fmt.Errorf("create cmyk temp file: %w", err)
	}
	tmpJPEGPath := tmpJPEG.Name()
	if _, err := tmpJPEG.Write(data); err != nil {
		_ = tmpJPEG.Close()
		_ = os.Remove(tmpJPEGPath)
		return fmt.Errorf("write cmyk temp file: %w", err)
	}
	if err := tmpJPEG.Close(); err != nil {
		_ = os.Remove(tmpJPEGPath)
		return fmt.Errorf("close cmyk temp file: %w", err)
	}
	defer os.Remove(tmpJPEGPath)

	tmpOut, err := os.CreateTemp(dir, ".cmyk-out-*.png")
	if err != nil {
		return fmt.Errorf("create output temp file: %w", err)
	}
	tmpOutPath := tmpOut.Name()
	_ = tmpOut.Close()
	_ = os.Remove(tmpOutPath)

	cmd := exec.Command("sips", "-s", "format", "png", tmpJPEGPath, "--out", tmpOutPath)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		_ = os.Remove(tmpOutPath)
		return fmt.Errorf("sips convert cmyk: %v: %s", runErr, strings.TrimSpace(string(output)))
	}

	if err := os.Rename(tmpOutPath, outPath); err != nil {
		_ = os.Remove(tmpOutPath)
		return fmt.Errorf("rename converted output: %w", err)
	}
	return nil
}

// EncodePNG 将 image.Image 编码为 PNG 写入 writer。
func EncodePNG(w io.Writer, img image.Image) error {
	enc := png.Encoder{CompressionLevel: png.NoCompression}
	return enc.Encode(w, img)
}

// WriteImageAtomically 原子写入图片文件（临时文件 + rename）。
func WriteImageAtomically(outPath string, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tempFile, err := os.CreateTemp(dir, "."+filepath.Base(outPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := tempFile.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close temp file: %w", closeErr)
			}
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := write(tempFile); err != nil {
		return err
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	closed = true
	if err := os.Rename(tempPath, outPath); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// ConvertJPXToOutput 将 JPX 数据转换为目标格式图片。
func ConvertJPXToOutput(reader io.Reader, outPath, format string, quality int) (err error) {
	dir := filepath.Dir(outPath)
	tmpRaw, err := os.CreateTemp(dir, ".jpx-raw-*.jpx")
	if err != nil {
		return fmt.Errorf("create jpx temp file: %w", err)
	}
	tmpRawPath := tmpRaw.Name()
	if _, err := io.Copy(tmpRaw, reader); err != nil {
		_ = tmpRaw.Close()
		_ = os.Remove(tmpRawPath)
		return fmt.Errorf("write jpx temp file: %w", err)
	}
	if err := tmpRaw.Close(); err != nil {
		_ = os.Remove(tmpRawPath)
		return fmt.Errorf("close jpx temp file: %w", err)
	}
	defer os.Remove(tmpRawPath)

	sipsFormat := "png"
	if format == "jpg" || format == "jpeg" {
		sipsFormat = "jpeg"
	}

	return ConvertJPXFile(tmpRawPath, outPath, format, sipsFormat)
}

// ConvertJPXFile 将 JPX/JPEG2000 文件转换为目标格式。
func ConvertJPXFile(rawPath, outPath, outputExt, sipsFormat string) error {
	type converter struct {
		path string
		name string
		args []string
	}

	var candidates []converter
	switch {
	case len(sipsFormat) > 0:
		if magick := ResolveBundledMagickExecutable(); magick != "" {
			candidates = append(candidates, converter{
				path: magick,
				name: filepath.Base(magick),
				args: []string{rawPath, outPath},
			})
		}
		candidates = append(candidates, converter{
			path: "sips",
			name: "sips",
			args: []string{"-s", "format", sipsFormat, rawPath, "--out", outPath},
		})
		candidates = append(candidates, converter{
			path: "magick",
			name: "magick",
			args: []string{rawPath, outPath},
		})
		candidates = append(candidates, converter{
			path: "convert",
			name: "convert",
			args: []string{rawPath, outPath},
		})
	default:
		candidates = append(candidates, converter{
			path: "magick",
			name: "magick",
			args: []string{rawPath, outPath},
		})
		candidates = append(candidates, converter{
			path: "convert",
			name: "convert",
			args: []string{rawPath, outPath},
		})
	}

	var errs []string
	for _, candidate := range candidates {
		if candidate.path == "" {
			candidate.path = candidate.name
		}
		if _, lookErr := exec.LookPath(candidate.path); lookErr != nil {
			errs = append(errs, fmt.Sprintf("%s not found", candidate.name))
			continue
		}
		cmd := exec.Command(candidate.path, candidate.args...)
		combined, runErr := cmd.CombinedOutput()
		if runErr == nil {
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v: %s", candidate.name, runErr, strings.TrimSpace(string(combined))))
	}

	return fmt.Errorf("convert jpx to %s failed: %s", outputExt, strings.Join(errs, " | "))
}

// ResolveBundledMagickExecutable 查找系统中可用的 ImageMagick 二进制。
func ResolveBundledMagickExecutable() string {
	exePath, err := os.Executable()
	if err != nil || exePath == "" {
		return ""
	}
	searchDir := filepath.Dir(exePath)
	matches, err := filepath.Glob(filepath.Join(searchDir, "ImageMagick*.exe"))
	if err != nil {
		return ""
	}
	for _, candidate := range matches {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// ExtractSoftMask 读取并解码与图片关联的 SMask（软遮罩/透明度通道）。
func ExtractSoftMask(ctx *model.Context, sd *types.StreamDict, objNr, w, h int, timing bool, pageNr int) ([]byte, error) {
	o, _ := sd.Find("SMask")
	if o == nil {
		return nil, nil
	}

	sm, _, err := ctx.XRefTable.DereferenceStreamDict(o)
	if err != nil {
		return nil, err
	}
	if sm == nil {
		return nil, nil
	}
	if err := sm.Decode(); err != nil {
		return nil, err
	}

	bpc := sm.IntEntry("BitsPerComponent")
	if bpc == nil || *bpc != 8 {
		return nil, nil
	}

	sw := sm.IntEntry("Width")
	sh := sm.IntEntry("Height")
	if sw == nil || sh == nil || *sw != w || *sh != h {
		return nil, nil
	}

	return sm.Content, nil
}

// ReadDirEntries 读取目录并自然排序。
func ReadDirEntries(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sortEntries(entries)
	return entries, nil
}

func sortEntries(entries []os.DirEntry) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if NaturalLess(entries[j].Name(), entries[i].Name()) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}

// MinRegionAreaThreshold 根据图片尺寸动态计算前景区域的最小面积阈值。
func MinRegionAreaThreshold(width, height int) int {
	area := width * height * 5 / 1000
	if area < 1000 {
		return 1000
	}
	return area
}
