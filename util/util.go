package util

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var (
	mutoolPathCache string
	mutoolPathMu    sync.Once
	gsPathCache     string
	gsPathMu        sync.Once
)

// FindMutool 查找系统 mutool 路径。
func FindMutool() string {
	mutoolPathMu.Do(func() {
		// 1. 检查 PATH
		if p, err := exec.LookPath("mutool"); err == nil {
			mutoolPathCache = p
			return
		}
		// 2. 检查与可执行文件同目录
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			candidates := []string{
				filepath.Join(exeDir, "mutool"),
				filepath.Join(exeDir, "..", "Resources", "mutool"),
				filepath.Join(exeDir, "mutool.exe"),
			}
			for _, c := range candidates {
				if info, e := os.Stat(c); e == nil && !info.IsDir() {
					mutoolPathCache = c
					return
				}
			}
			// 3. 检查 binaries 子目录
			platform := "darwin"
			if runtime.GOOS == "windows" {
				platform = "windows"
			} else if runtime.GOOS == "linux" {
				platform = "linux"
			}
			p := filepath.Join(exeDir, "binaries", platform, "mutool")
			if info, e := os.Stat(p); e == nil && !info.IsDir() {
				mutoolPathCache = p
				return
			}
		}
	})
	return mutoolPathCache
}

// FindGS 查找系统 Ghostscript 路径。
func FindGS() string {
	gsPathMu.Do(func() {
		// 1. 检查 PATH
		if p, err := exec.LookPath("gs"); err == nil {
			gsPathCache = p
			return
		}
		// 2. 检查与可执行文件同目录
		if exe, err := os.Executable(); err == nil {
			exeDir := filepath.Dir(exe)
			candidates := []string{
				filepath.Join(exeDir, "gs"),
				filepath.Join(exeDir, "..", "Resources", "gs"),
				filepath.Join(exeDir, "gs.exe"),
				filepath.Join(exeDir, "gswin64c.exe"),
				filepath.Join(exeDir, "gswin32c.exe"),
			}
			for _, c := range candidates {
				if info, e := os.Stat(c); e == nil && !info.IsDir() {
					gsPathCache = c
					return
				}
			}
			// 3. 检查 binaries 子目录
			platform := "darwin"
			if runtime.GOOS == "windows" {
				platform = "windows"
			} else if runtime.GOOS == "linux" {
				platform = "linux"
			}
			p := filepath.Join(exeDir, "binaries", platform, "gs")
			if info, e := os.Stat(p); e == nil && !info.IsDir() {
				gsPathCache = p
				return
			}
		}
	})
	return gsPathCache
}

// ComputeWorkerCount 根据 CPU 百分比计算工作线程数。
func ComputeWorkerCount(parallelPercent int) int {
	numCPU := runtime.NumCPU()
	if parallelPercent <= 0 {
		return 1
	}
	n := (numCPU * parallelPercent + 50) / 100
	if n < 1 {
		n = 1
	}
	return n
}

// FormatSize 格式化字节数为人类可读字符串。
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// OutputExtension 根据格式返回文件扩展名。
func OutputExtension(format string) string {
	if format == "jpg" {
		return "jpg"
	}
	return format
}

// GetPageCount 获取 PDF 页数。
func GetPageCount(inputFile string) (int, error) {
	if FindMutool() != "" {
		if n, err := GetPageCountViaMutool(inputFile); err == nil && n > 0 {
			return n, nil
		}
	}
	if FindGS() != "" {
		cmd := exec.Command(FindGS(), "-dNOPAUSE", "-dBATCH", "-dQUIET", "-dNOSAFER",
			"-c", fmt.Sprintf("(%s) (r) file runpdfbegin pdfpagecount = quit", inputFile))
		output, err := cmd.Output()
		if err == nil {
			if n, parseErr := strconv.Atoi(strings.TrimSpace(string(output))); parseErr == nil && n > 0 {
				return n, nil
			}
		}
	}
	// 回退到 mutool pages
	cmd := exec.Command(FindMutool(), "pages", inputFile)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("get page count: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	pageCounts := make([]int, 0, len(lines))
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			if n, parseErr := strconv.Atoi(parts[2]); parseErr == nil {
				pageCounts = append(pageCounts, n)
			}
		}
	}
	if len(pageCounts) > 0 {
		return pageCounts[len(pageCounts)-1], nil
	}
	return 0, fmt.Errorf("cannot determine page count")
}

// GetPageCountViaMutool 用 mutool info 获取页数。
func GetPageCountViaMutool(inputFile string) (int, error) {
	cmd := exec.Command(FindMutool(), "info", inputFile)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("mutool info: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "Pages:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return strconv.Atoi(parts[len(parts)-1])
			}
		}
	}
	return 0, fmt.Errorf("page count not found in mutool output")
}

// ReadPPM 读取 PPM 格式文件。
func ReadPPM(path string) (*image.RGBA, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	header := string(data[:2])
	if header == "P6" || header == "P3" {
		// 简单 PPM 解析
		lines := strings.SplitN(string(data), "\n", 4)
		if len(lines) < 4 {
			return nil, fmt.Errorf("invalid PPM: %s", path)
		}
		var w, h, maxVal int
		if _, err := fmt.Sscanf(lines[1], "%d %d", &w, &h); err != nil {
			return nil, fmt.Errorf("parse PPM dimensions: %w", err)
		}
		if _, err := fmt.Sscanf(lines[2], "%d", &maxVal); err != nil {
			return nil, fmt.Errorf("parse PPM maxval: %w", err)
		}
		_ = maxVal
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		pixelData := []byte(lines[3])
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				idx := (y*w + x) * 3
				if idx+2 < len(pixelData) {
					img.Set(x, y, color.RGBA{
						R: pixelData[idx],
						G: pixelData[idx+1],
						B: pixelData[idx+2],
						A: 255,
					})
				}
			}
		}
		return img, nil
	}
	return nil, fmt.Errorf("unsupported image format: %s", path)
}

// ApplyColorCorrectionToFile 对图片文件应用色彩校正。
func ApplyColorCorrectionToFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open for color correction: %w", err)
	}
	defer srcFile.Close()
	srcImg, _, err := image.Decode(srcFile)
	if err != nil {
		return fmt.Errorf("decode for color correction: %w", err)
	}
	rgba := ToRGBA(srcImg)
	ApplyColorCorrection(rgba)

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create color corrected file: %w", err)
	}
	defer dstFile.Close()
	if strings.HasSuffix(dst, ".jpg") || strings.HasSuffix(dst, ".jpeg") {
		return jpeg.Encode(dstFile, rgba, nil)
	}
	return png.Encode(dstFile, rgba)
}

// CropImage 裁剪图片。
func CropImage(img image.Image, rect image.Rectangle) *image.RGBA {
	rgba := ToRGBA(img)
	crop := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	draw.Draw(crop, crop.Bounds(), rgba, rect.Min, draw.Src)
	return crop
}

// ToRGBA 将任意 image.Image 转为 *image.RGBA。
func ToRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	return rgba
}

// IsBackground 判断像素是否为背景色（接近白色）。
func IsBackground(pixel color.RGBA) bool {
	return pixel.R >= 248 && pixel.G >= 248 && pixel.B >= 248
}

// ColorCorrectionMatrix 是 Adobe 标准的 sRGB→ProPhoto 色彩校正矩阵。
var ColorCorrectionMatrix = [3][4]float64{
	{0.7977, 0.1352, 0.0313, 0},
	{0.2880, 0.7119, 0.0001, 0},
	{0.0000, 0.0000, 0.8249, 0},
}

// ApplyColorCorrection 应用色彩校正矩阵到图片。
func ApplyColorCorrection(img *image.RGBA) {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			rf := float64(r) / 65535.0
			gf := float64(g) / 65535.0
			bf := float64(b) / 65535.0
			clamp := func(v float64) uint8 {
				if v < 0 {
					return 0
				}
				if v > 1 {
					return 255
				}
				return uint8(v * 255)
			}
			nr := clamp(ColorCorrectionMatrix[0][0]*rf + ColorCorrectionMatrix[0][1]*gf + ColorCorrectionMatrix[0][2]*bf + ColorCorrectionMatrix[0][3])
			ng := clamp(ColorCorrectionMatrix[1][0]*rf + ColorCorrectionMatrix[1][1]*gf + ColorCorrectionMatrix[1][2]*bf + ColorCorrectionMatrix[1][3])
			nb := clamp(ColorCorrectionMatrix[2][0]*rf + ColorCorrectionMatrix[2][1]*gf + ColorCorrectionMatrix[2][2]*bf + ColorCorrectionMatrix[2][3])
			img.Set(x, y, color.RGBA{R: nr, G: ng, B: nb, A: uint8(a >> 8)})
		}
	}
}