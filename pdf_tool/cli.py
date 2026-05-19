"""Command-line interface for pdf-tool."""

from __future__ import annotations

import sys
from pathlib import Path

import click

from pdf_tool import __version__
from pdf_tool.operations import (
    extract_pages,
    get_info,
    merge_pdfs,
    rotate_pages,
    split_pdf,
)


def _parse_pages(pages_str: str) -> list[int]:
    """Parse a comma/range page specification into a sorted list of 1-based page numbers.

    Examples::

        "1,3,5"   -> [1, 3, 5]
        "2-5"     -> [2, 3, 4, 5]
        "1,3-5,7" -> [1, 3, 4, 5, 7]

    Raises:
        click.BadParameter: On invalid syntax.
    """
    result: set[int] = set()
    for part in pages_str.split(","):
        part = part.strip()
        if not part:
            continue
        if "-" in part:
            bounds = part.split("-", 1)
            try:
                start, end = int(bounds[0]), int(bounds[1])
            except ValueError:
                raise click.BadParameter(f"Invalid page range: '{part}'")
            if start > end:
                raise click.BadParameter(
                    f"Invalid range '{part}': start must be <= end."
                )
            result.update(range(start, end + 1))
        else:
            try:
                result.add(int(part))
            except ValueError:
                raise click.BadParameter(f"Invalid page number: '{part}'")
    if not result:
        raise click.BadParameter("No valid page numbers provided.")
    return sorted(result)


@click.group()
@click.version_option(__version__, prog_name="pdf-tool")
def cli() -> None:
    """pdf-tool: Perform common operations on PDF files."""


@cli.command("merge")
@click.argument("inputs", nargs=-1, required=True, type=click.Path(exists=True))
@click.option(
    "-o", "--output", required=True, type=click.Path(), help="Output PDF path."
)
def merge_cmd(inputs: tuple[str, ...], output: str) -> None:
    """Merge multiple PDF files into a single PDF.

    INPUTS are two or more PDF files to merge, in the order provided.
    """
    if len(inputs) < 2:
        raise click.UsageError("At least two input PDF files are required.")
    try:
        out = merge_pdfs(inputs, output)
        click.echo(f"Merged {len(inputs)} files → {out}")
    except Exception as exc:
        click.echo(f"Error: {exc}", err=True)
        sys.exit(1)


@cli.command("split")
@click.argument("input", type=click.Path(exists=True))
@click.option(
    "-o",
    "--output-dir",
    default=".",
    show_default=True,
    type=click.Path(),
    help="Directory for output page PDFs.",
)
def split_cmd(input: str, output_dir: str) -> None:
    """Split a PDF into individual single-page PDF files."""
    try:
        pages = split_pdf(input, output_dir)
        click.echo(f"Split into {len(pages)} page(s) in '{output_dir}'")
    except Exception as exc:
        click.echo(f"Error: {exc}", err=True)
        sys.exit(1)


@cli.command("extract")
@click.argument("input", type=click.Path(exists=True))
@click.option(
    "-p",
    "--pages",
    required=True,
    metavar="PAGES",
    help="Pages to extract, e.g. '1,3-5,7'.",
)
@click.option(
    "-o", "--output", required=True, type=click.Path(), help="Output PDF path."
)
def extract_cmd(input: str, pages: str, output: str) -> None:
    """Extract specific pages from a PDF into a new PDF."""
    try:
        page_list = _parse_pages(pages)
        out = extract_pages(input, output, page_list)
        click.echo(f"Extracted {len(page_list)} page(s) → {out}")
    except click.BadParameter:
        raise
    except Exception as exc:
        click.echo(f"Error: {exc}", err=True)
        sys.exit(1)


@cli.command("rotate")
@click.argument("input", type=click.Path(exists=True))
@click.option(
    "-a",
    "--angle",
    required=True,
    type=click.Choice(["90", "180", "270"]),
    help="Clockwise rotation angle in degrees.",
)
@click.option(
    "-p",
    "--pages",
    default=None,
    metavar="PAGES",
    help="Pages to rotate, e.g. '1,3-5'.  Omit to rotate all pages.",
)
@click.option(
    "-o", "--output", required=True, type=click.Path(), help="Output PDF path."
)
def rotate_cmd(input: str, angle: str, pages: str | None, output: str) -> None:
    """Rotate pages in a PDF by 90, 180, or 270 degrees."""
    try:
        page_list = _parse_pages(pages) if pages else None
        out = rotate_pages(input, output, int(angle), page_list)
        target = f"{len(page_list)} page(s)" if page_list else "all pages"
        click.echo(f"Rotated {target} by {angle}° → {out}")
    except click.BadParameter:
        raise
    except Exception as exc:
        click.echo(f"Error: {exc}", err=True)
        sys.exit(1)


@cli.command("info")
@click.argument("input", type=click.Path(exists=True))
def info_cmd(input: str) -> None:
    """Display metadata and basic information about a PDF."""
    try:
        data = get_info(input)
    except Exception as exc:
        click.echo(f"Error: {exc}", err=True)
        sys.exit(1)

    click.echo(f"File:              {Path(input).resolve()}")
    click.echo(f"File size:         {data['file_size']:,} bytes")
    click.echo(f"Pages:             {data['page_count']}")
    click.echo(f"Encrypted:         {data['encrypted']}")
    click.echo(f"Title:             {data['title'] or '—'}")
    click.echo(f"Author:            {data['author'] or '—'}")
    click.echo(f"Subject:           {data['subject'] or '—'}")
    click.echo(f"Creator:           {data['creator'] or '—'}")
    click.echo(f"Producer:          {data['producer'] or '—'}")
    click.echo(f"Creation date:     {data['creation_date'] or '—'}")
    click.echo(f"Modification date: {data['modification_date'] or '—'}")
