#!/usr/bin/env python3
"""从原始 logo 生成前端要用的所有图片资源。

用法：
   python3 tools/make-logo.py                 # 用 logo.png（根目录）或 assets/logo-source.png
   python3 tools/make-logo.py <源图路径>       # 用指定的图

产出（都在 web/public/）：
   logo.png               256×256 圆角  —— UI 用
   favicon.svg            内嵌 PNG 的 SVG —— 现代浏览器
   favicon.ico            16/32/48/64    —— 老浏览器
   apple-touch-icon.png   180×180        —— iOS 主屏

⚠️ 为什么要有这个脚本：
   源图 1254×1254、1.2 MB —— 直接当 favicon 太重。
   而且源图**没有透明通道**、**满图无留白**，
   所以 UI 里要加圆角才不显得生硬。
"""
import base64
import io
import os
import sys

try:
    from PIL import Image, ImageDraw
except ImportError:
    print("❌ 需要 Pillow：pip install Pillow", file=sys.stderr)
    sys.exit(1)

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PUB = os.path.join(ROOT, "web", "public")

# 源图的候选路径（按优先级）。
# ⚠️ 根目录那份是用户直接放进来的原件 —— 优先用它。
# ⚠️ `assets/logo-source.png` 是备用（源图不在根目录时用）。
SRC_CANDIDATES = [
    os.path.join(ROOT, "logo.png"),
    os.path.join(ROOT, "assets", "logo-source.png"),
]


def rounded(src, size, radius_ratio=0.18):
    """缩到 size 并加圆角（返回 RGBA）。"""
    r = max(1, int(size * radius_ratio))
    small = src.resize((size, size), Image.LANCZOS)
    mask = Image.new("L", (size, size), 0)
    ImageDraw.Draw(mask).rounded_rectangle(
        [0, 0, size - 1, size - 1], radius=r, fill=255
    )
    out = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    out.paste(small, (0, 0), mask)
    return out


def main():
    if len(sys.argv) > 1:
        src_path = sys.argv[1]
        if not os.path.exists(src_path):
            print(f"❌ 找不到源图：{src_path}", file=sys.stderr)
            return 1
    else:
        src_path = next((p for p in SRC_CANDIDATES if os.path.exists(p)), None)
        if not src_path:
            print("❌ 找不到源图 —— 试：", file=sys.stderr)
            for p in SRC_CANDIDATES:
                print(f"     {p}", file=sys.stderr)
            return 1

    os.makedirs(PUB, exist_ok=True)
    im = Image.open(src_path).convert("RGB")
    print(f"源图 {src_path}  {im.size}")

    # ① UI 用的 logo（256，圆角）
    logo = rounded(im, 256)
    p = os.path.join(PUB, "logo.png")
    logo.save(p, "PNG", optimize=True)
    print(f"  logo.png              {os.path.getsize(p)//1024} KB")

    # ② favicon.svg（内嵌 PNG —— 任意尺寸都清晰）
    buf = io.BytesIO()
    logo.save(buf, "PNG", optimize=True)
    b64 = base64.b64encode(buf.getvalue()).decode()
    p = os.path.join(PUB, "favicon.svg")
    with open(p, "w", encoding="utf-8") as f:
        f.write(
            '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256">\n'
            f'  <image width="256" height="256" href="data:image/png;base64,{b64}"/>\n'
            "</svg>\n"
        )
    print(f"  favicon.svg           {os.path.getsize(p)//1024} KB")

    # ③ favicon.ico
    p = os.path.join(PUB, "favicon.ico")
    im.save(p, "ICO", sizes=[(16, 16), (32, 32), (48, 48), (64, 64)])
    print(f"  favicon.ico           {os.path.getsize(p)//1024} KB")

    # ④ apple-touch-icon（180，iOS 不支持透明）
    p = os.path.join(PUB, "apple-touch-icon.png")
    rounded(im, 180, 0.0).convert("RGB").save(p, "PNG", optimize=True)
    print(f"  apple-touch-icon.png  {os.path.getsize(p)//1024} KB")

    return 0


if __name__ == "__main__":
    sys.exit(main())
