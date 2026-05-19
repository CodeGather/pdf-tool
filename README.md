# pdf-tool

A command-line tool for common PDF operations built with Python.

## Features

- **merge** – Combine multiple PDFs into one file
- **split** – Split a PDF into individual single-page files
- **extract** – Extract specific pages into a new PDF
- **rotate** – Rotate pages by 90 °, 180 °, or 270 °
- **info** – Display metadata and basic information about a PDF

## Installation

```bash
pip install -e .
```

Requires Python ≥ 3.9.

## Usage

```
pdf-tool [OPTIONS] COMMAND [ARGS]...

Options:
  --version  Show the version and exit.
  --help     Show this message and exit.

Commands:
  merge    Merge multiple PDF files into a single PDF.
  split    Split a PDF into individual single-page PDF files.
  extract  Extract specific pages from a PDF into a new PDF.
  rotate   Rotate pages in a PDF by 90, 180, or 270 degrees.
  info     Display metadata and basic information about a PDF.
```

### merge

```bash
pdf-tool merge file1.pdf file2.pdf file3.pdf -o merged.pdf
```

### split

```bash
pdf-tool split document.pdf -o pages/
```

Produces `pages/page_1.pdf`, `pages/page_2.pdf`, …

### extract

```bash
# Extract pages 1, 3, and 5–7
pdf-tool extract document.pdf -p "1,3,5-7" -o extracted.pdf
```

Page ranges use comma-separated values and `start-end` notation.

### rotate

```bash
# Rotate all pages 90° clockwise
pdf-tool rotate document.pdf -a 90 -o rotated.pdf

# Rotate only pages 1 and 3 by 180°
pdf-tool rotate document.pdf -a 180 -p "1,3" -o rotated.pdf
```

### info

```bash
pdf-tool info document.pdf
```

Example output:

```
File:              /path/to/document.pdf
File size:         142,336 bytes
Pages:             12
Encrypted:         False
Title:             My Document
Author:            —
Subject:           —
Creator:           Writer
Producer:          LibreOffice 7.5
Creation date:     D:20240101120000Z
Modification date: —
```

## Development

```bash
pip install -e ".[dev]"
pytest
```
