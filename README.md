# PDF Tool

Version: v1.0.5

Archived snapshot of the v1.0.5 release of this tool.

This directory is a standalone Go module.

## Run

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
./pdf-tool -i input.pdf -o output
```

Optional flags:

```bash
./pdf-tool -i input.pdf -o output -f png -dpi 300
./pdf-tool -i input.pdf -o output -l -m-json
./pdf-tool -i input.pdf -o output -l -m
```

Merge mode:

```bash
# Merge all PDFs in a directory, in sorted order.
./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf

# Merge an explicit file list in the order you provide.
./pdf-tool -merge -merge-inputs a.pdf,b.pdf,c.pdf -o merged.pdf

# Force chunked merging for many small PDFs.
./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -merge-chunk-size 20

# Merge while printing progress and timing.
./pdf-tool -merge -merge-dir /path/to/pdfs -o merged.pdf -l -t

# Merge while keeping console output enabled and using a custom glob.
./pdf-tool -merge -merge-dir /path/to/pdfs -merge-glob "*.PDF" -o merged.pdf -l
```

Debug / trace example:

```bash
./pdf-tool -i input.pdf -o output -l -t
```

If you want to inspect a specific PDF more closely, combine `-l` and `-t` so the program prints branch decisions and per-stage elapsed time.

If you also add `-m-json`, the elapsed time is written into each image record's `time` field instead of being printed as separate timing lines.

For `JPXDecode + DeviceCMYK` images, this tool uses a native macOS conversion path to turn the decoded pixel buffer into PNG output. That path keeps color correctness first, while still avoiding whole-page rendering. If you only need the original JPX bytes, keeping the raw stream is faster; if you need PNG, the native conversion path is the supported option.

When `-merge` is enabled, `-o` is treated as the merged PDF output path instead of an output directory.

Common examples:

```bash
# Export images as JPG with a higher rendering DPI.
./pdf-tool -i sample.pdf -o out -f jpg -dpi 450

# Print debug logs while keeping the default PNG output.
./pdf-tool -i sample.pdf -o out -l

# Print both debug logs and timing information during troubleshooting.
./pdf-tool -i sample.pdf -o out -l -t

# Print image width/height while processing.
./pdf-tool -i sample.pdf -o out -l -m

# Print image metadata as JSON lines.
./pdf-tool -i sample.pdf -o out -l -m-json

# Merge a folder of PDFs into a single file.
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf

# Merge 300+ small PDFs in smaller batches.
./pdf-tool -merge -merge-dir ./pdfs -o merged.pdf -merge-chunk-size 25

# Merge a known ordered list of files.
./pdf-tool -merge -merge-inputs 1.pdf,2.pdf,3.pdf -o merged.pdf
```

Use `-l` to enable console output. Then choose either `-m-json` for a single JSON array of all image metadata, or `-m` for plain image width/height lines.

For merge mode, prefer `-merge-dir` for folder-based batches and `-merge-inputs` for an explicit ordered list. The default chunk size is tuned for many small files, and you can lower it if you want smaller intermediate batches.

The program renders each PDF page first, then crops the four largest visible regions from the rendered page image.

For direct image extraction, JPXDecode images are handled separately from JPG/PNG streams so they can be converted correctly without falling back to page rendering.

## Build

```bash
cd /path/to/pdf-tool
go build -o pdf-tool .
```

Build release binaries manually:

```bash
# macOS
GOTELEMETRY=off GOCACHE="$TMPDIR/go-cache" go build -o dist/darwin/pdf-tool .

# Windows 64-bit
GOTELEMETRY=off GOCACHE="$TMPDIR/go-cache" CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=/opt/homebrew/bin/x86_64-w64-mingw32-gcc go build -o dist/win/pdf-tool.exe .
```

Outputs:

- `dist/darwin/pdf-tool`
- `dist/win/pdf-tool.exe`

One-shot build script:

```bash
./scripts/build-release.sh
```

## Archive

This repository snapshot is archived as version: v1.0.5.
