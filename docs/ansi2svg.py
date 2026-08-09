#!/usr/bin/env python3
"""Render captured terminal output (with ANSI colors) as a static SVG card.

The README screenshot is generated from a real run rather than mocked up, so
it cannot drift from what the tool actually prints:

    script -q /dev/null tgsieve plan --all --timings > demo.ansi
    docs/ansi2svg.py demo.ansi docs/demo.svg --title "tgsieve plan --all"

Only the SGR codes tgsieve emits are supported (reset, bold, dim, and the
eight basic foreground colors).
"""

import argparse
import html
import re
import sys

CSI = re.compile(r"\x1b\[([0-9;]*)([A-Za-z])")

# GitHub-dark-ish palette, chosen to stay legible on both README themes.
COLORS = {
    30: "#6e7681", 31: "#ff7b72", 32: "#7ee787", 33: "#e3b341",
    34: "#79c0ff", 35: "#d2a8ff", 36: "#a5d6ff", 37: "#c9d1d9",
}
FG = "#c9d1d9"
DIM = "#8b949e"
BOLD = "#f0f6fc"
BG = "#0d1117"
PROMPT = "#7ee787"

CHAR_W = 7.83
LINE_H = 19.0
PAD_X = 18.0
PAD_TOP = 46.0
PAD_BOTTOM = 18.0


class Style:
    def __init__(self):
        self.reset()

    def reset(self):
        self.color = None
        self.bold = False
        self.dim = False

    def apply(self, codes):
        for c in codes:
            if c == 0:
                self.reset()
            elif c == 1:
                self.bold = True
            elif c == 2:
                self.dim = True
            elif c in COLORS:
                self.color = COLORS[c]

    def fill(self):
        if self.color:
            return self.color
        if self.dim:
            return DIM
        if self.bold:
            return BOLD
        return FG

    def weight(self):
        return "600" if self.bold else "400"


def parse(line):
    """Split one line into (text, fill, weight) runs."""
    style = Style()
    runs = []
    pos = 0
    for m in CSI.finditer(line):
        text = line[pos:m.start()]
        if text:
            runs.append((text, style.fill(), style.weight()))
        pos = m.end()
        if m.group(2) == "m":
            codes = [int(c) if c else 0 for c in m.group(1).split(";")]
            style.apply(codes)
    tail = line[pos:]
    if tail:
        runs.append((tail, style.fill(), style.weight()))
    return runs


def fold_backspaces(line):
    """Apply backspaces the way a terminal would: they erase what precedes them.

    macOS `script` opens the capture with a literal "^D" followed by two
    backspaces that rub it out again.
    """
    out = []
    for ch in line:
        if ch == "\x08":
            if out:
                out.pop()
        else:
            out.append(ch)
    return "".join(out)


def clean(raw):
    """Drop carriage-return redraws (the progress spinner) and stray controls."""
    lines = []
    for line in raw.split("\n"):
        line = line.split("\r")[-1]  # only the final state of a redrawn line
        line = fold_backspaces(line).replace("\x04", "")
        lines.append(line)
    while lines and not visible(lines[0]):
        lines.pop(0)
    while lines and not visible(lines[-1]):
        lines.pop()
    return lines


def visible(line):
    return CSI.sub("", line).strip() != ""


def render(lines, title):
    body = []
    if title:
        body.append([("$ ", PROMPT, "600"), (title, FG, "400")])
    body.extend(parse(line) for line in lines)

    widest = max((sum(len(t) for t, _, _ in runs) for runs in body), default=40)
    width = round(widest * CHAR_W + PAD_X * 2)
    height = round(len(body) * LINE_H + PAD_TOP + PAD_BOTTOM)

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}" role="img" aria-label="tgsieve example output">',
        f'  <rect width="{width}" height="{height}" rx="10" fill="{BG}"/>',
        f'  <rect width="{width}" height="30" rx="10" fill="#161b22"/>',
        '  <rect y="20" width="100%" height="10" fill="#161b22"/>',
        '  <circle cx="20" cy="15" r="5" fill="#ff5f57"/>',
        '  <circle cx="38" cy="15" r="5" fill="#febc2e"/>',
        '  <circle cx="56" cy="15" r="5" fill="#28c840"/>',
        '  <g font-family="ui-monospace,SFMono-Regular,SF Mono,Menlo,Consolas,'
        'DejaVu Sans Mono,monospace" font-size="13">',
    ]
    for i, runs in enumerate(body):
        y = PAD_TOP + i * LINE_H
        spans = []
        col = 0
        for text, fill, weight in runs:
            x = PAD_X + col * CHAR_W
            spans.append(
                f'<tspan x="{x:.1f}" y="{y:.1f}" fill="{fill}" '
                f'font-weight="{weight}">{html.escape(text)}</tspan>'
            )
            col += len(text)
        if spans:
            out.append("    <text xml:space=\"preserve\">" + "".join(spans) + "</text>")
    out.append("  </g>")
    out.append("</svg>")
    return "\n".join(out) + "\n"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("input", help="captured output, or - for stdin")
    ap.add_argument("output")
    ap.add_argument("--title", default="", help="command line to show as the prompt")
    args = ap.parse_args()

    raw = sys.stdin.read() if args.input == "-" else open(args.input, encoding="utf-8", errors="replace").read()
    svg = render(clean(raw), args.title)
    with open(args.output, "w", encoding="utf-8") as f:
        f.write(svg)
    print(f"wrote {args.output}")


if __name__ == "__main__":
    main()
