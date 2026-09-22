#!/usr/bin/env python3
"""Generate the Cvent Data PWA icons deterministically.

Four PNGs in web/icons/: a solid accent (#2563eb) square with a white
"C" glyph centered. "Maskable" variants keep the same drawing inside an
80% safe zone (glyph scaled to 80% of the canvas, background full-bleed).

The glyph is the default bold font's "C" when one is found on the system
(deterministic: same font file -> same pixels); otherwise a circle-arc
"C" is drawn. No random or timestamp-dependent elements, so repeated
runs produce byte-identical files.
"""

import os
import sys

from PIL import Image, ImageDraw, ImageFont

ACCENT = (37, 99, 235)  # #2563eb
WHITE = (255, 255, 255)
SAFE_ZONE = 0.8  # maskable: glyph fits within the center 80%

OUT_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "web", "icons")

FONT_CANDIDATES = [
    "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
    "/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf",
    "/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
    "/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
    "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
]


def find_font():
    for path in FONT_CANDIDATES:
        if os.path.exists(path):
            return path
    return None


def draw_glyph(draw, cx, cy, r, font_path, size):
    """Draw the white 'C' centered at (cx, cy) with radius r."""
    if font_path:
        font = ImageFont.truetype(font_path, int(size * 0.72))
        text = "C"
        bbox = draw.textbbox((0, 0), text, font=font)
        w = bbox[2] - bbox[0]
        h = bbox[3] - bbox[1]
        draw.text((cx - w / 2 - bbox[0], cy - h / 2 - bbox[1]), text,
                  font=font, fill=WHITE)
    else:
        # Circle-arc fallback: gap on the right side -> reads as 'C'.
        width = max(2, int(r * 0.28))
        box = [cx - r, cy - r, cx + r, cy + r]
        draw.arc(box, start=45, end=315, fill=WHITE, width=width)


def make_icon(size, maskable, font_path):
    img = Image.new("RGB", (size, size), ACCENT)
    draw = ImageDraw.Draw(img)
    # Glyph radius: full size for plain, 80% safe zone for maskable.
    scale = SAFE_ZONE if maskable else 1.0
    r = int(size * 0.30 * scale)
    draw_glyph(draw, size / 2, size / 2, r, font_path, size * scale)
    return img


def main():
    out_dir = os.path.normpath(OUT_DIR)
    os.makedirs(out_dir, exist_ok=True)
    font_path = find_font()
    print(f"font: {font_path or '(none — arc fallback)'}")
    for size, maskable in [(192, False), (512, False), (192, True), (512, True)]:
        suffix = "-maskable" if maskable else ""
        path = os.path.join(out_dir, f"icon-{size}{suffix}.png")
        make_icon(size, maskable, font_path).save(path, "PNG", optimize=True)
        print(f"wrote {path} ({os.path.getsize(path)} bytes)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
