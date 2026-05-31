# PDF Tool Version

Current version: v2.5+ (with Ghostscript compression & merge-compress)

## 新增功能（v2.5+）

- **PDF 压缩**：基于 Ghostscript 的 `-compress` 单文件/目录模式
- **合并前压缩**：`-merge-compress`（默认开启），大幅减小合并输出体积
- **压缩预设**：`-compress-preset` — screen / ebook / printer / prepress / high
- **分辨率控制**：`-compress-resolution`（默认 1200 DPI），覆盖预设降采样率
- **gs 自发现**：`findGS()` 自动查找 PATH → 同目录 → bund/ → Homebrew
- **dist 打包**：`build-release.sh` 自动从 `bund/darwin-universal/` 拷贝 gs