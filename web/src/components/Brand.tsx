/**
 * 品牌标识 —— logo 图片。
 *
 * ⚠️ 原来这里是个纯 CSS 的方块 + 字母 "F"（蓝底白字）。
 *   现在换成真实的 logo 图片。
 *
 * ⚠️ 资源在 `web/public/` 下：
 *   - `logo.png` 256×256 圆角 —— UI 用
 *   - `favicon.svg` / `favicon.ico` —— 浏览器标签页
 *
 * ⚠️ logo 本身是**深色**的（近黑底 + 亮蓝图形），
 *   所以在浅色和深色主题下都能看清 —— 不需要额外的底色。
 */

/** logo 的尺寸档位。 */
const SIZES = {
  sm: 'h-7 w-7',
  md: 'h-8 w-8',
} as const

/** Brand 图标。 */
export function BrandMark({ size = 'md' }: { size?: keyof typeof SIZES }) {
  return (
    <img
      src="/logo.png"
      alt="Free Model House"
      className={`${SIZES[size]} shrink-0 rounded-[var(--r-sm)] object-cover`}
      width={32}
      height={32}
    />
  )
}
