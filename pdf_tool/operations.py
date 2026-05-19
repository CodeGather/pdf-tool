"""Core PDF operations for pdf-tool."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Iterable

from pypdf import PdfReader, PdfWriter


def merge_pdfs(input_paths: Iterable[str | Path], output_path: str | Path) -> Path:
    """Merge multiple PDF files into a single PDF.

    Args:
        input_paths: Paths to the PDF files to merge, in order.
        output_path: Path for the merged output PDF.

    Returns:
        The resolved path of the output file.

    Raises:
        FileNotFoundError: If any input file does not exist.
        ValueError: If fewer than two input files are provided.
    """
    paths = [Path(p) for p in input_paths]
    if len(paths) < 2:
        raise ValueError("At least two input PDFs are required for merging.")
    for p in paths:
        if not p.exists():
            raise FileNotFoundError(f"Input file not found: {p}")

    writer = PdfWriter()
    for p in paths:
        reader = PdfReader(str(p))
        for page in reader.pages:
            writer.add_page(page)

    out = Path(output_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with open(out, "wb") as f:
        writer.write(f)
    return out.resolve()


def split_pdf(input_path: str | Path, output_dir: str | Path) -> list[Path]:
    """Split a PDF into individual single-page PDFs.

    Each output file is named ``page_<N>.pdf`` where *N* is 1-based.

    Args:
        input_path: Path to the source PDF.
        output_dir: Directory where the page PDFs will be written.

    Returns:
        List of paths to the created page files.

    Raises:
        FileNotFoundError: If the input file does not exist.
    """
    src = Path(input_path)
    if not src.exists():
        raise FileNotFoundError(f"Input file not found: {src}")

    out_dir = Path(output_dir)
    out_dir.mkdir(parents=True, exist_ok=True)

    reader = PdfReader(str(src))
    created: list[Path] = []
    for i, page in enumerate(reader.pages, start=1):
        writer = PdfWriter()
        writer.add_page(page)
        out_file = out_dir / f"page_{i}.pdf"
        with open(out_file, "wb") as f:
            writer.write(f)
        created.append(out_file.resolve())
    return created


def extract_pages(
    input_path: str | Path,
    output_path: str | Path,
    pages: Iterable[int],
) -> Path:
    """Extract specific pages from a PDF into a new file.

    Args:
        input_path: Path to the source PDF.
        output_path: Path for the output PDF containing extracted pages.
        pages: 1-based page numbers to extract.

    Returns:
        The resolved path of the output file.

    Raises:
        FileNotFoundError: If the input file does not exist.
        ValueError: If a requested page number is out of range or no pages
            are specified.
    """
    src = Path(input_path)
    if not src.exists():
        raise FileNotFoundError(f"Input file not found: {src}")

    page_list = list(pages)
    if not page_list:
        raise ValueError("At least one page number must be specified.")

    reader = PdfReader(str(src))
    total = len(reader.pages)
    writer = PdfWriter()
    for num in page_list:
        if num < 1 or num > total:
            raise ValueError(
                f"Page {num} is out of range (document has {total} pages)."
            )
        writer.add_page(reader.pages[num - 1])

    out = Path(output_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with open(out, "wb") as f:
        writer.write(f)
    return out.resolve()


def rotate_pages(
    input_path: str | Path,
    output_path: str | Path,
    angle: int,
    pages: Iterable[int] | None = None,
) -> Path:
    """Rotate pages in a PDF by a multiple of 90 degrees.

    Args:
        input_path: Path to the source PDF.
        output_path: Path for the output PDF.
        angle: Clockwise rotation angle in degrees (must be 90, 180, or 270).
        pages: 1-based page numbers to rotate.  ``None`` (default) rotates
            all pages.

    Returns:
        The resolved path of the output file.

    Raises:
        FileNotFoundError: If the input file does not exist.
        ValueError: If *angle* is not 90, 180, or 270, or a page number is out
            of range.
    """
    if angle not in (90, 180, 270):
        raise ValueError("Rotation angle must be 90, 180, or 270 degrees.")

    src = Path(input_path)
    if not src.exists():
        raise FileNotFoundError(f"Input file not found: {src}")

    reader = PdfReader(str(src))
    total = len(reader.pages)

    if pages is None:
        target_pages = set(range(1, total + 1))
    else:
        target_pages = set(pages)
        for num in target_pages:
            if num < 1 or num > total:
                raise ValueError(
                    f"Page {num} is out of range (document has {total} pages)."
                )

    writer = PdfWriter()
    for i, page in enumerate(reader.pages, start=1):
        if i in target_pages:
            page.rotate(angle)
        writer.add_page(page)

    out = Path(output_path)
    out.parent.mkdir(parents=True, exist_ok=True)
    with open(out, "wb") as f:
        writer.write(f)
    return out.resolve()


def get_info(input_path: str | Path) -> dict:
    """Return metadata and basic information about a PDF.

    Args:
        input_path: Path to the PDF file.

    Returns:
        Dictionary with keys:

        - ``page_count`` – total number of pages (int)
        - ``title`` – document title or ``None``
        - ``author`` – document author or ``None``
        - ``subject`` – document subject or ``None``
        - ``creator`` – creating application or ``None``
        - ``producer`` – PDF producer or ``None``
        - ``creation_date`` – creation timestamp string or ``None``
        - ``modification_date`` – modification timestamp string or ``None``
        - ``encrypted`` – whether the PDF is encrypted (bool)
        - ``file_size`` – file size in bytes (int)

    Raises:
        FileNotFoundError: If the file does not exist.
    """
    src = Path(input_path)
    if not src.exists():
        raise FileNotFoundError(f"Input file not found: {src}")

    reader = PdfReader(str(src))
    meta = reader.metadata or {}

    return {
        "page_count": len(reader.pages),
        "title": meta.get("/Title"),
        "author": meta.get("/Author"),
        "subject": meta.get("/Subject"),
        "creator": meta.get("/Creator"),
        "producer": meta.get("/Producer"),
        "creation_date": meta.get("/CreationDate"),
        "modification_date": meta.get("/ModDate"),
        "encrypted": reader.is_encrypted,
        "file_size": os.path.getsize(src),
    }
