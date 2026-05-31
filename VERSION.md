# PDF Tool Version

Current version: v2.6+ (with concurrent compress progress)

## 新增功能（v2.6+）

- **并发压缩进度**：`-merge-compress -p` 现在实时输出压缩进度（0→100），与合并进度共用同一 `-p` 参数
- **后台并发启动**：压缩 goroutine 启动与结果收集并行，避免 sem 信号量阻塞延迟进度显示
- **WaitGroup 竞态修复**：`wg.Add(len(files))` 提前到主 goroutine 设置，确保压缩完整执行

## 新增功能（v2.5+）

- **PDF 压缩**：基于 Ghostscript 的 `-compress` 单文件/目录模式
- **合并前压缩**：`-merge-compress`（默认开启），大幅减小合并输出体积
- **压缩预设**：`-compress-preset` — screen / ebook / printer / prepress / high
- **分辨率控制**：`-compress-resolution`（默认 1200 DPI），覆盖预设降采样率
- **gs 自发现**：`findGS()` 自动查找 PATH → 同目录 → bund/ → Homebrew
- **dist 打包**：`build-release.sh` 自动从 `bund/darwin-universal/` 拷贝 gs