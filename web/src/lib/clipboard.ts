/**
 * 复制到剪贴板 —— **兼容非 HTTPS**。
 *
 * ⚠️ **为什么不能用 `navigator.clipboard` 就完事**：
 *   那个 API 只在**安全上下文**（HTTPS 或 localhost）可用。
 *   而 FMH 是跑在 NAS 上的局域网服务，用户访问的是
 *   `http://192.168.x.x:8678` —— **HTTP，不是 HTTPS**。
 *   于是 `navigator.clipboard` 是 `undefined`，
 *   `writeText` 抛异常，而原来的代码 `catch` 里什么都不做 ——
 *   表现就是「点了复制按钮，一点反应都没有」。
 *
 * ⚠️ 降级方案：`document.execCommand('copy')`。
 *   它是废弃 API，但在 HTTP 环境下**唯一**能用的。
 *   做法：把文本塞进一个**离屏 textarea** → 全选 → 执行 copy。
 *
 * ⚠️ textarea 必须**在视口内**（不能 `display:none`），
 *   否则某些浏览器会拒绝复制 —— 所以用固定定位 + 透明 + z-index 压到最底。
 */

/** 把文本复制到剪贴板。返回是否成功。 */
export async function copyToClipboard(text: string): Promise<boolean> {
  if (!text) return false

  // ① 现代 API（localhost / HTTPS）。
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // 权限被拒 → 落到下面的降级方案。
    }
  }

  // ② 降级：execCommand。
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    // ⚠️ 必须在视口内（有些浏览器对不可见元素拒绝复制），
    //   所以用 `fixed` + `opacity:0` 而不是 `display:none`。
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '0'
    ta.style.left = '0'
    ta.style.width = '1px'
    ta.style.height = '1px'
    ta.style.padding = '0'
    ta.style.border = 'none'
    ta.style.outline = 'none'
    ta.style.boxShadow = 'none'
    ta.style.background = 'transparent'
    ta.style.opacity = '0'
    ta.style.zIndex = '-1'
    document.body.appendChild(ta)

    // 选中文本（iOS 需要 setSelectionRange 而不是 select()）。
    ta.focus()
    ta.select()
    ta.setSelectionRange(0, ta.value.length)

    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}
