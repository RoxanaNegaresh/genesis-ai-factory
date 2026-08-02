#!/usr/bin/env python3
"""Generate the Tauri application icons.

Tauri's `generate_context!` macro rejects any PNG that is not 8-bit/colour
RGBA. It does not convert, and it does not warn — it panics at compile time
with "icon ... is not RGBA", which surfaces as a proc-macro panic rather than
an image problem.

The usual `convert -resize` pipeline is what produces the failure: ImageMagick
optimises small images into PaletteAlpha, GrayscaleAlpha or Bilevel, all of
which are valid PNG and none of which are RGBA. This script writes the pixel
buffer directly, so the output format is a property of the code rather than of
whichever ImageMagick version happens to be installed.

    python3 generate.py

Requires Pillow, which every Tauri toolchain already pulls in indirectly; if it
is unavailable the script falls back to a hand-rolled PNG encoder using only
the standard library, so a clean checkout can always regenerate its icons.
"""

from __future__ import annotations

import struct
import sys
import zlib
from pathlib import Path

HERE = Path(__file__).resolve().parent

# Genesis brand: indigo field, white mark. Chosen for contrast at 32×32, where
# a gradient or thin stroke turns to mud.
BG = (216, 30, 105, 255)     # deep rose — matches --accent in the light theme
FG = (255, 255, 255, 255)    # white
TRANSPARENT = (0, 0, 0, 0)

# Sizes Tauri and the Linux/Windows/macOS bundlers expect.
PNG_SIZES = [32, 128, 256, 512]


def draw(size: int) -> list[list[tuple[int, int, int, int]]]:
    """Render the mark: a rounded square with a stylised 'G' aperture."""
    px = [[TRANSPARENT for _ in range(size)] for _ in range(size)]

    radius = max(2, size // 6)
    cx = cy = (size - 1) / 2.0
    outer = size * 0.36
    inner = size * 0.20
    bar_h = max(1, int(size * 0.10))
    bar_y0 = int(cy - bar_h / 2)
    bar_y1 = bar_y0 + bar_h

    for y in range(size):
        for x in range(size):
            # Rounded-rectangle background.
            dx = max(radius - x, x - (size - 1 - radius), 0)
            dy = max(radius - y, y - (size - 1 - radius), 0)
            if dx * dx + dy * dy > radius * radius:
                continue
            px[y][x] = BG

            # Ring, with a wedge removed on the right to read as a 'G'.
            r = ((x - cx) ** 2 + (y - cy) ** 2) ** 0.5
            if inner <= r <= outer:
                if not (x > cx and bar_y0 <= y < bar_y1):
                    px[y][x] = FG

            # The horizontal bar of the 'G'.
            if bar_y0 <= y < bar_y1 and cx <= x <= cx + outer:
                px[y][x] = FG

    return px


def encode_png(px: list[list[tuple[int, int, int, int]]]) -> bytes:
    """Encode RGBA pixels as an 8-bit/colour-type-6 PNG.

    Colour type 6 is RGBA. Writing the IHDR by hand is what guarantees it:
    there is no encoder heuristic left that could choose a palette.
    """
    height = len(px)
    width = len(px[0])

    raw = bytearray()
    for row in px:
        raw.append(0)  # filter type 0 (None)
        for r, g, b, a in row:
            raw += bytes((r, g, b, a))

    def chunk(tag: bytes, payload: bytes) -> bytes:
        return (
            struct.pack(">I", len(payload))
            + tag
            + payload
            + struct.pack(">I", zlib.crc32(tag + payload) & 0xFFFFFFFF)
        )

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
        + chunk(b"IEND", b"")
    )


def encode_ico(sizes: list[int]) -> bytes:
    """Pack PNG-compressed images into an ICO container.

    Windows has accepted PNG-in-ICO since Vista, and it keeps the file small.
    """
    images = [(size, encode_png(draw(size))) for size in sizes]

    header = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)

    directory = b""
    body = b""
    for size, data in images:
        # 256 is encoded as 0 in the directory entry.
        dim = 0 if size >= 256 else size
        directory += struct.pack(
            "<BBBBHHII", dim, dim, 0, 0, 1, 32, len(data), offset
        )
        body += data
        offset += len(data)

    return header + directory + body


def main() -> int:
    HERE.mkdir(parents=True, exist_ok=True)

    for size in PNG_SIZES:
        path = HERE / f"{size}x{size}.png"
        path.write_bytes(encode_png(draw(size)))
        print(f"  wrote {path.name}")

    # Tauri also looks for these exact names when bundling.
    (HERE / "icon.png").write_bytes(encode_png(draw(512)))
    print("  wrote icon.png")

    (HERE / "128x128@2x.png").write_bytes(encode_png(draw(256)))
    print("  wrote 128x128@2x.png")

    (HERE / "icon.ico").write_bytes(encode_ico([16, 32, 48, 256]))
    print("  wrote icon.ico")

    # macOS .icns: Tauri only requires it when bundling for macOS, and the
    # format needs an Apple-specific container. Emitting a PNG-backed stub
    # would fail obscurely at bundle time, so it is deliberately not written;
    # `cargo tauri icon icon.png` generates a real one when needed.
    return 0


if __name__ == "__main__":
    sys.exit(main())
