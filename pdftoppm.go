package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// pdftoppmExec 缓存 pdftoppm 可执行文件的路径。
var pdftoppmExec string

func init() {
	pdftoppmExec = findPdftoppm()
}

// findPdftoppm 按优先级搜索 pdftoppm：
//
//	1. 项目目录 pdftoppm/<平台>/pdftoppm[.exe]
//	2. 可执行文件同目录的 pdftoppm/<平台>/pdftoppm[.exe]
//	3. $PATH 中的 pdftoppm
//	4. 返回空字符串（调用者自行回退到 go-fitz）
func findPdftoppm() string {
	platformDir := platformPdftoppmDir()
	execName := "pdftoppm"
	if runtime.GOOS == "windows" {
		execName = "pdftoppm.exe"
	}

	// 搜索项目内捆绑 → 可执行文件同目录
	searchDirs := searchPdftoppmDirs()
	for _, dir := range searchDirs {
		candidate := filepath.Join(dir, platformDir, execName)
		if _, err := os.Stat(candidate); err == nil {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}

	// 最后查 PATH
	if path, err := exec.LookPath("pdftoppm"); err == nil {
		return path
	}

	return ""
}

// platformPdftoppmDir 返回当前平台的目录名：darwin-arm64, windows-amd64 等。
func platformPdftoppmDir() string {
	return fmt.Sprintf("%s-%s", runtime.GOOS, runtime.GOARCH)
}

// searchPdftoppmDirs 返回 pdftoppm 捆绑目录的父目录列表。
func searchPdftoppmDirs() []string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(wd, "pdftoppm"))
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "pdftoppm"))
	}
	return dirs
}

// hasPdftoppm 检查是否找到了可用的 pdftoppm。
func hasPdftoppm() bool {
	return pdftoppmExec != ""
}

// pdftoppmCommand 返回一个指向已找到的 pdftoppm 的命令。
func pdftoppmCommand(args ...string) *exec.Cmd {
	if pdftoppmExec == "" {
		return exec.Command("pdftoppm-NOT-FOUND")
	}
	return exec.Command(pdftoppmExec, args...)
}

// installPdftoppm 自动下载 pdftoppm 到项目目录。
func installPdftoppm(targetDir string) string {
	if targetDir == "" {
		if wd, err := os.Getwd(); err == nil {
			targetDir = filepath.Join(wd, "pdftoppm")
		} else {
			return ""
		}
	}

	platformDir := platformPdftoppmDir()
	installDir := filepath.Join(targetDir, platformDir)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建安装目录失败: %v\n", err)
		return ""
	}

	execName := "pdftoppm"
	if runtime.GOOS == "windows" {
		execName = "pdftoppm.exe"
	}
	binPath := filepath.Join(installDir, execName)

	switch runtime.GOOS {
	case "darwin":
		return installPdftoppmDarwin(installDir, binPath)
	case "windows":
		return installPdftoppmWindows(installDir, binPath)
	case "linux":
		return installPdftoppmLlinux(installDir, binPath)
	default:
		fmt.Fprintf(os.Stderr, "不支持自动下载的平台: %s。请手动安装 poppler。\n", runtime.GOOS)
		return ""
	}
}

// installPdftoppmDarwin 通过 Homebrew 安装或从其他机器复制版本来安装 pdftoppm。
func installPdftoppmDarwin(installDir, binPath string) string {
	// 先尝试 brew
	if brewPath, err := exec.LookPath("brew"); err == nil {
		fmt.Fprintf(os.Stderr, "检测到 Homebrew，尝试安装/复制 pdftoppm...\n")

		// brew list poppler 检查是否已安装
		if exec.Command(brewPath, "list", "poppler").Run() != nil {
			fmt.Fprintf(os.Stderr, "正在通过 Homebrew 安装 poppler（约 33MB，首次需网络）...\n")
			cmd := exec.Command(brewPath, "install", "poppler")
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "Homebrew 安装失败: %v\n", err)
				return ""
			}
		}

		// 从 brew Cellar 复制 pdftoppm
		brewPopplerDir := filepath.Join(filepath.Dir(filepath.Dir(brewPath)), "Cellar", "poppler")
		versions, _ := filepath.Glob(filepath.Join(brewPopplerDir, "*", "bin", "pdftoppm"))
		if len(versions) == 0 {
			fmt.Fprintf(os.Stderr, "未找到 pdftoppm (brew 安装可能不完整)\n")
			return ""
		}
		src := versions[len(versions)-1]
		if err := copyFile(src, binPath); err != nil {
			fmt.Fprintf(os.Stderr, "复制 pdftoppm 失败: %v\n", err)
			return ""
		}

		// 复制 libpoppler.dylib
		libDir := filepath.Dir(filepath.Dir(src))
		libFile := filepath.Join(libDir, "lib", "libpoppler.159.0.0.dylib")
		if _, err := os.Stat(libFile); err == nil {
			dstLib := filepath.Join(installDir, "libpoppler.159.0.0.dylib")
			copyFile(libFile, dstLib)

			// 复制 liblcms2
			lcmsFile := "/opt/homebrew/opt/little-cms2/lib/liblcms2.2.dylib"
			if _, err := os.Stat(lcmsFile); err == nil {
				dstLcms := filepath.Join(installDir, "liblcms2.2.dylib")
				copyFile(lcmsFile, dstLcms)
				fmt.Fprintf(os.Stderr, "  依赖: liblcms2.2.dylib\n")
			} else {
				// 尝试从 brew 找 liblcms2
				if lcmsLibs, _ := filepath.Glob(filepath.Join(filepath.Dir(libDir), "..", "little-cms2", "*", "lib", "liblcms2.2.dylib")); len(lcmsLibs) > 0 {
					dstLcms := filepath.Join(installDir, "liblcms2.2.dylib")
					copyFile(lcmsLibs[0], dstLcms)
					fmt.Fprintf(os.Stderr, "  依赖: liblcms2.2.dylib (from brew cellar)\n")
				}
			}

			// 修正 rpath
			fixDarwinRpath(installDir)
		}

		fmt.Fprintf(os.Stderr, "完成！pdftoppm 已安装到: %s\n", binPath)
		return binPath
	}

	fmt.Fprintf(os.Stderr, "未检测到 Homebrew。请从已有 poppler 的 Mac 复制:\n")
	fmt.Fprintf(os.Stderr, "  mkdir -p %s\n", installDir)
	fmt.Fprintf(os.Stderr, "  scp user@oldmachine:/opt/homebrew/bin/pdftoppm %s/\n", installDir)
	fmt.Fprintf(os.Stderr, "  scp -r user@oldmachine:/opt/homebrew/Cellar/poppler/*/lib/libpoppler*.dylib %s/\n", installDir)
	fmt.Fprintf(os.Stderr, "  cp /opt/homebrew/opt/little-cms2/lib/liblcms2.2.dylib %s/  (如果有)\n", installDir)
	return ""
}

// fixDarwinRpath 修正 macOS 上 pdftoppm 的库搜索路径，使其在当前目录找 libpoppler。
func fixDarwinRpath(installDir string) {
	binFile := filepath.Join(installDir, "pdftoppm")
	libFile := filepath.Join(installDir, "libpoppler.159.0.0.dylib")
	lcmsFile := filepath.Join(installDir, "liblcms2.2.dylib")

	// 如果 install_name_tool 不可用（Xcode 未安装），跳过
	if _, err := exec.LookPath("install_name_tool"); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: install_name_tool 不可用，跳过 rpath 修正\n")
		fmt.Fprintf(os.Stderr, "  使用: DYLD_LIBRARY_PATH=%s %s\n", installDir, binFile)
		return
	}

	// 修正 libpoppler.dylib 的 id
	if _, err := os.Stat(libFile); err == nil {
		exec.Command("install_name_tool", "-id", "@loader_path/libpoppler.159.0.0.dylib", libFile).Run()
	}

	// 修正 liblcms2.dylib 的 id
	if _, err := os.Stat(lcmsFile); err == nil {
		exec.Command("install_name_tool", "-id", "@loader_path/liblcms2.2.dylib", lcmsFile).Run()
	}

	// 修正 pdftoppm 对 libpoppler 的引用
	if _, err := os.Stat(binFile); err == nil {
		exec.Command("install_name_tool", "-change",
			"@rpath/libpoppler.159.dylib",
			"@loader_path/libpoppler.159.0.0.dylib",
			binFile,
		).Run()
	}

	// 修正 libpoppler 对 liblcms2 的引用
	if _, err := os.Stat(libFile); err == nil {
		if _, err2 := os.Stat(lcmsFile); err2 == nil {
		exec.Command("install_name_tool", "-change",
			"/opt/homebrew/opt/little-cms2/lib/liblcms2.2.dylib",
			"@loader_path/liblcms2.2.dylib",
			libFile,
		).Run()
		}
		// 也尝试其他可能的路径
		exec.Command("install_name_tool", "-change",
			"@rpath/liblcms2.2.dylib",
			"@loader_path/liblcms2.2.dylib",
			libFile,
		).Run()
	}

	fmt.Fprintf(os.Stderr, "  rpath 已修正（@loader_path）\n")
}

// installPdftoppmWindows 从 poppler-windows GitHub release 下载 pdftoppm。
func installPdftoppmWindows(installDir, binPath string) string {
	releaseURL := "https://api.github.com/repos/oschwartz10612/poppler-windows/releases/latest"

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(releaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "获取 release 信息失败: %v\n", err)
		return ""
	}
	defer resp.Body.Close()

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		fmt.Fprintf(os.Stderr, "解析 release 信息失败: %v\n", err)
		return ""
	}

	var zipURL string
	for _, asset := range release.Assets {
		if strings.HasSuffix(asset.Name, ".zip") && strings.Contains(asset.Name, "Release-") {
			zipURL = asset.BrowserDownloadURL
			break
		}
	}
	if zipURL == "" {
		fmt.Fprintf(os.Stderr, "未找到 poppler-windows ZIP\n")
		return ""
	}

	fmt.Fprintf(os.Stderr, "下载 poppler-windows %s...\n", release.TagName)
	req, _ := http.NewRequest("GET", zipURL, nil)
	resp, err = client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "下载失败: %v\n", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "下载失败: HTTP %d\n", resp.StatusCode)
		return ""
	}

	zipPath := filepath.Join(os.TempDir(), "poppler-windows.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建临时文件失败: %v\n", err)
		return ""
	}
	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(zipPath)
		fmt.Fprintf(os.Stderr, "写入 ZIP 失败: %v\n", err)
		return ""
	}
	defer os.Remove(zipPath)

	fmt.Fprintf(os.Stderr, "解压中...\n")
	tmpDir, _ := os.MkdirTemp("", "poppler-extract-*")
	defer os.RemoveAll(tmpDir)

	extractCmd := exec.Command("powershell", "-Command",
		"Expand-Archive", "-Path", zipPath, "-DestinationPath", tmpDir, "-Force",
	)
	if out, err := extractCmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "解压失败: %v: %s\n", err, string(out))
		return ""
	}

	// 查找 pdftoppm.exe
	var pdftoppmSrc string
	filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() && info.Name() == "pdftoppm.exe" {
			pdftoppmSrc = path
		}
		return nil
	})

	if pdftoppmSrc == "" {
		fmt.Fprintf(os.Stderr, "解压后未找到 pdftoppm.exe\n")
		return ""
	}

	// 复制 pdftoppm.exe 和所有 DLL
	popplerBin := filepath.Dir(pdftoppmSrc)
	copyFile(pdftoppmSrc, binPath)

	dlls, _ := filepath.Glob(filepath.Join(popplerBin, "*.dll"))
	for _, dll := range dlls {
		copyFile(dll, filepath.Join(installDir, filepath.Base(dll)))
	}

	fmt.Fprintf(os.Stderr, "完成！pdftoppm 已安装到: %s (%d 个 DLL)\n", binPath, len(dlls))
	return binPath
}

// installPdftoppmLlinux 为 Linux 自动安装 pdftoppm。
func installPdftoppmLlinux(installDir, binPath string) string {
	for _, pm := range []struct {
		cmd  string
		args []string
	}{{"apt", []string{"install", "-y", "poppler-utils"}},
		{"dnf", []string{"install", "-y", "poppler-utils"}},
		{"yum", []string{"install", "-y", "poppler-utils"}},
	} {
		if pmPath, err := exec.LookPath(pm.cmd); err == nil {
			fmt.Fprintf(os.Stderr, "通过 %s 安装 poppler-utils...\n", pm.cmd)
			cmd := exec.Command(pmPath, pm.args...)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if cmd.Run() == nil {
				if src, err := exec.LookPath("pdftoppm"); err == nil {
					copyFile(src, binPath)
					return binPath
				}
			}
		}
	}
	fmt.Fprintf(os.Stderr, "请手动安装: apt install poppler-utils\n")
	return ""
}

// copyFile 复制文件。
func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	d, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer d.Close()

	if _, err := io.Copy(d, s); err != nil {
		return err
	}
	return os.Chmod(dst, 0755)
}