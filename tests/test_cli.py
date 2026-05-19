"""Tests for pdf_tool.cli (Click commands)."""

from __future__ import annotations

from pathlib import Path

import pytest
from click.testing import CliRunner
from pypdf import PdfReader, PdfWriter

from pdf_tool.cli import cli


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _make_pdf(path: Path, num_pages: int = 1) -> Path:
    writer = PdfWriter()
    for _ in range(num_pages):
        writer.add_blank_page(width=612, height=792)
    with open(path, "wb") as f:
        writer.write(f)
    return path


# ---------------------------------------------------------------------------
# merge
# ---------------------------------------------------------------------------

class TestMergeCommand:
    def test_merge_success(self, tmp_path):
        runner = CliRunner()
        a = _make_pdf(tmp_path / "a.pdf", 2)
        b = _make_pdf(tmp_path / "b.pdf", 3)
        out = tmp_path / "merged.pdf"

        result = runner.invoke(cli, ["merge", str(a), str(b), "-o", str(out)])

        assert result.exit_code == 0
        assert "Merged 2 files" in result.output
        reader = PdfReader(str(out))
        assert len(reader.pages) == 5

    def test_merge_requires_two_inputs(self, tmp_path):
        runner = CliRunner()
        a = _make_pdf(tmp_path / "a.pdf")
        out = tmp_path / "out.pdf"

        result = runner.invoke(cli, ["merge", str(a), "-o", str(out)])

        assert result.exit_code != 0

    def test_merge_missing_output_option(self, tmp_path):
        runner = CliRunner()
        a = _make_pdf(tmp_path / "a.pdf")
        b = _make_pdf(tmp_path / "b.pdf")

        result = runner.invoke(cli, ["merge", str(a), str(b)])

        assert result.exit_code != 0


# ---------------------------------------------------------------------------
# split
# ---------------------------------------------------------------------------

class TestSplitCommand:
    def test_split_success(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 3)
        out_dir = tmp_path / "pages"

        result = runner.invoke(cli, ["split", str(src), "-o", str(out_dir)])

        assert result.exit_code == 0
        assert "3 page(s)" in result.output
        assert len(list(out_dir.glob("*.pdf"))) == 3

    def test_split_default_output_dir(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 2)

        with runner.isolated_filesystem(temp_dir=tmp_path):
            result = runner.invoke(cli, ["split", str(src)])

        assert result.exit_code == 0


# ---------------------------------------------------------------------------
# extract
# ---------------------------------------------------------------------------

class TestExtractCommand:
    def test_extract_success(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 5)
        out = tmp_path / "out.pdf"

        result = runner.invoke(
            cli, ["extract", str(src), "-p", "1,3,5", "-o", str(out)]
        )

        assert result.exit_code == 0
        assert "3 page(s)" in result.output
        reader = PdfReader(str(out))
        assert len(reader.pages) == 3

    def test_extract_range(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 10)
        out = tmp_path / "out.pdf"

        result = runner.invoke(
            cli, ["extract", str(src), "-p", "2-4", "-o", str(out)]
        )

        assert result.exit_code == 0
        reader = PdfReader(str(out))
        assert len(reader.pages) == 3

    def test_extract_out_of_range(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 2)
        out = tmp_path / "out.pdf"

        result = runner.invoke(
            cli, ["extract", str(src), "-p", "99", "-o", str(out)]
        )

        assert result.exit_code != 0


# ---------------------------------------------------------------------------
# rotate
# ---------------------------------------------------------------------------

class TestRotateCommand:
    def test_rotate_all_pages(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 2)
        out = tmp_path / "out.pdf"

        result = runner.invoke(
            cli, ["rotate", str(src), "-a", "90", "-o", str(out)]
        )

        assert result.exit_code == 0
        assert "all pages" in result.output
        reader = PdfReader(str(out))
        assert len(reader.pages) == 2

    def test_rotate_specific_pages(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 4)
        out = tmp_path / "out.pdf"

        result = runner.invoke(
            cli, ["rotate", str(src), "-a", "180", "-p", "1,3", "-o", str(out)]
        )

        assert result.exit_code == 0
        assert "2 page(s)" in result.output

    def test_rotate_invalid_angle(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf")
        out = tmp_path / "out.pdf"

        result = runner.invoke(
            cli, ["rotate", str(src), "-a", "45", "-o", str(out)]
        )

        assert result.exit_code != 0


# ---------------------------------------------------------------------------
# info
# ---------------------------------------------------------------------------

class TestInfoCommand:
    def test_info_output(self, tmp_path):
        runner = CliRunner()
        src = _make_pdf(tmp_path / "src.pdf", 3)

        result = runner.invoke(cli, ["info", str(src)])

        assert result.exit_code == 0
        assert "Pages:" in result.output
        assert "3" in result.output
        assert "File size:" in result.output

    def test_info_missing_file(self, tmp_path):
        runner = CliRunner()

        result = runner.invoke(cli, ["info", str(tmp_path / "ghost.pdf")])

        assert result.exit_code != 0


# ---------------------------------------------------------------------------
# version / help
# ---------------------------------------------------------------------------

class TestVersionHelp:
    def test_version(self):
        runner = CliRunner()
        result = runner.invoke(cli, ["--version"])
        assert result.exit_code == 0
        assert "0.1.0" in result.output

    def test_help(self):
        runner = CliRunner()
        result = runner.invoke(cli, ["--help"])
        assert result.exit_code == 0
        assert "pdf-tool" in result.output
