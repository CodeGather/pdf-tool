"""Tests for pdf_tool.operations."""

from __future__ import annotations

import pytest
from pathlib import Path

from pypdf import PdfReader, PdfWriter


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_pdf(path: Path, num_pages: int = 1) -> Path:
    """Write a minimal valid PDF with *num_pages* blank pages to *path*."""
    writer = PdfWriter()
    for _ in range(num_pages):
        writer.add_blank_page(width=612, height=792)
    with open(path, "wb") as f:
        writer.write(f)
    return path


# ---------------------------------------------------------------------------
# merge_pdfs
# ---------------------------------------------------------------------------

class TestMergePdfs:
    def test_merges_two_files(self, tmp_path):
        from pdf_tool.operations import merge_pdfs

        a = _make_pdf(tmp_path / "a.pdf", 2)
        b = _make_pdf(tmp_path / "b.pdf", 3)
        out = tmp_path / "merged.pdf"

        result = merge_pdfs([a, b], out)

        assert result == out.resolve()
        reader = PdfReader(str(result))
        assert len(reader.pages) == 5

    def test_merges_three_files(self, tmp_path):
        from pdf_tool.operations import merge_pdfs

        files = [_make_pdf(tmp_path / f"f{i}.pdf", i + 1) for i in range(3)]
        out = tmp_path / "merged.pdf"

        result = merge_pdfs(files, out)

        reader = PdfReader(str(result))
        assert len(reader.pages) == 1 + 2 + 3

    def test_raises_for_single_file(self, tmp_path):
        from pdf_tool.operations import merge_pdfs

        a = _make_pdf(tmp_path / "a.pdf")
        with pytest.raises(ValueError, match="At least two"):
            merge_pdfs([a], tmp_path / "out.pdf")

    def test_raises_for_missing_file(self, tmp_path):
        from pdf_tool.operations import merge_pdfs

        a = _make_pdf(tmp_path / "a.pdf")
        with pytest.raises(FileNotFoundError):
            merge_pdfs([a, tmp_path / "nope.pdf"], tmp_path / "out.pdf")

    def test_creates_output_parent_dirs(self, tmp_path):
        from pdf_tool.operations import merge_pdfs

        a = _make_pdf(tmp_path / "a.pdf")
        b = _make_pdf(tmp_path / "b.pdf")
        out = tmp_path / "subdir" / "nested" / "merged.pdf"

        merge_pdfs([a, b], out)

        assert out.exists()


# ---------------------------------------------------------------------------
# split_pdf
# ---------------------------------------------------------------------------

class TestSplitPdf:
    def test_splits_into_individual_pages(self, tmp_path):
        from pdf_tool.operations import split_pdf

        src = _make_pdf(tmp_path / "src.pdf", 3)
        out_dir = tmp_path / "pages"

        pages = split_pdf(src, out_dir)

        assert len(pages) == 3
        for i, p in enumerate(pages, start=1):
            assert p.name == f"page_{i}.pdf"
            reader = PdfReader(str(p))
            assert len(reader.pages) == 1

    def test_creates_output_directory(self, tmp_path):
        from pdf_tool.operations import split_pdf

        src = _make_pdf(tmp_path / "src.pdf", 1)
        out_dir = tmp_path / "new_dir"

        split_pdf(src, out_dir)

        assert out_dir.is_dir()

    def test_raises_for_missing_file(self, tmp_path):
        from pdf_tool.operations import split_pdf

        with pytest.raises(FileNotFoundError):
            split_pdf(tmp_path / "ghost.pdf", tmp_path / "pages")


# ---------------------------------------------------------------------------
# extract_pages
# ---------------------------------------------------------------------------

class TestExtractPages:
    def test_extracts_single_page(self, tmp_path):
        from pdf_tool.operations import extract_pages

        src = _make_pdf(tmp_path / "src.pdf", 5)
        out = tmp_path / "out.pdf"

        result = extract_pages(src, out, [3])

        reader = PdfReader(str(result))
        assert len(reader.pages) == 1

    def test_extracts_multiple_pages(self, tmp_path):
        from pdf_tool.operations import extract_pages

        src = _make_pdf(tmp_path / "src.pdf", 10)
        out = tmp_path / "out.pdf"

        result = extract_pages(src, out, [1, 3, 5, 7])

        reader = PdfReader(str(result))
        assert len(reader.pages) == 4

    def test_raises_for_out_of_range_page(self, tmp_path):
        from pdf_tool.operations import extract_pages

        src = _make_pdf(tmp_path / "src.pdf", 3)
        with pytest.raises(ValueError, match="out of range"):
            extract_pages(src, tmp_path / "out.pdf", [5])

    def test_raises_for_empty_page_list(self, tmp_path):
        from pdf_tool.operations import extract_pages

        src = _make_pdf(tmp_path / "src.pdf", 3)
        with pytest.raises(ValueError, match="At least one"):
            extract_pages(src, tmp_path / "out.pdf", [])

    def test_raises_for_missing_file(self, tmp_path):
        from pdf_tool.operations import extract_pages

        with pytest.raises(FileNotFoundError):
            extract_pages(tmp_path / "ghost.pdf", tmp_path / "out.pdf", [1])


# ---------------------------------------------------------------------------
# rotate_pages
# ---------------------------------------------------------------------------

class TestRotatePages:
    def test_rotate_all_pages(self, tmp_path):
        from pdf_tool.operations import rotate_pages

        src = _make_pdf(tmp_path / "src.pdf", 3)
        out = tmp_path / "out.pdf"

        result = rotate_pages(src, out, 90)

        reader = PdfReader(str(result))
        assert len(reader.pages) == 3

    def test_rotate_specific_pages(self, tmp_path):
        from pdf_tool.operations import rotate_pages

        src = _make_pdf(tmp_path / "src.pdf", 4)
        out = tmp_path / "out.pdf"

        result = rotate_pages(src, out, 180, pages=[1, 3])

        reader = PdfReader(str(result))
        assert len(reader.pages) == 4

    def test_raises_for_invalid_angle(self, tmp_path):
        from pdf_tool.operations import rotate_pages

        src = _make_pdf(tmp_path / "src.pdf", 1)
        with pytest.raises(ValueError, match="90, 180, or 270"):
            rotate_pages(src, tmp_path / "out.pdf", 45)

    def test_raises_for_out_of_range_page(self, tmp_path):
        from pdf_tool.operations import rotate_pages

        src = _make_pdf(tmp_path / "src.pdf", 2)
        with pytest.raises(ValueError, match="out of range"):
            rotate_pages(src, tmp_path / "out.pdf", 90, pages=[5])

    def test_raises_for_missing_file(self, tmp_path):
        from pdf_tool.operations import rotate_pages

        with pytest.raises(FileNotFoundError):
            rotate_pages(tmp_path / "ghost.pdf", tmp_path / "out.pdf", 90)


# ---------------------------------------------------------------------------
# get_info
# ---------------------------------------------------------------------------

class TestGetInfo:
    def test_returns_page_count(self, tmp_path):
        from pdf_tool.operations import get_info

        src = _make_pdf(tmp_path / "src.pdf", 4)
        info = get_info(src)

        assert info["page_count"] == 4

    def test_returns_file_size(self, tmp_path):
        from pdf_tool.operations import get_info

        src = _make_pdf(tmp_path / "src.pdf", 1)
        info = get_info(src)

        assert info["file_size"] == src.stat().st_size
        assert info["file_size"] > 0

    def test_encrypted_is_false_for_plain_pdf(self, tmp_path):
        from pdf_tool.operations import get_info

        src = _make_pdf(tmp_path / "src.pdf", 1)
        info = get_info(src)

        assert info["encrypted"] is False

    def test_raises_for_missing_file(self, tmp_path):
        from pdf_tool.operations import get_info

        with pytest.raises(FileNotFoundError):
            get_info(tmp_path / "ghost.pdf")

    def test_metadata_keys_present(self, tmp_path):
        from pdf_tool.operations import get_info

        src = _make_pdf(tmp_path / "src.pdf", 1)
        info = get_info(src)

        for key in ("title", "author", "subject", "creator", "producer",
                    "creation_date", "modification_date"):
            assert key in info
