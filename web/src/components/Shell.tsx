import { useEffect, useState } from 'react'
import {
  Boxes,
  ChartNoAxesColumn,
  LogOut,
  Menu,
  Monitor,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Plug,
  Settings as SettingsIcon,
  Sun,
  Waypoints,
  X,
} from 'lucide-react'
import { Badge, Button, cn } from './ui'
import { BrandMark } from './Brand'
import { useStatus } from '../api/client'

/** 页面路由表。 */
export type RouteKey =
  | 'quickstart'
  | 'models'
  | 'usage'
  | 'sources'
  | 'settings'

interface NavItem {
  key: RouteKey
  label: string
  icon: typeof Plug
  group: string
}

/**
 * 导航项。
 *
 * ⚠️ **快速接入排第一** —— 因为 NAS 场景下，
 * 装完之后的第一件事就是"接进去"。
 * 这个顺序是 FMH 自己定的。
 */
const navItems: NavItem[] = [
  { key: 'quickstart', label: '快速接入', icon: Plug, group: '接入' },
  { key: 'models', label: '模型清单', icon: Boxes, group: '运行' },
  // ⚠️ 用量和上游源**原本都是 Activity 图标** —— 侧栏上一模一样，
  //   根本分不出哪个是哪个。现在用量的柱状图，上游源的汇聚点。
  { key: 'usage', label: '用量', icon: ChartNoAxesColumn, group: '运行' },
  { key: 'sources', label: '上游源', icon: Waypoints, group: '运行' },
  { key: 'settings', label: '设置', icon: SettingsIcon, group: '系统' },
]

/**
 * 主题。
 *
 * ⚠️ 有**三档**，不只是亮/暗：
 *   `system` —— 跟随操作系统（默认）
 *   `light`  —— 强制亮色
 *   `dark`   —— 强制暗色
 *
 * ⚠️ 之前只有两档，且"跟随系统"只在**首次访问**时生效一次
 * （读 localStorage，读不到就看 matchMedia）——
 *   之后用户改了系统主题，页面**不会跟着变**。
 *   现在存的是 `system`，并监听 matchMedia 的变化。
 */
type ThemePref = 'system' | 'light' | 'dark'

/** 实际生效的主题（`system` 解析后的结果）。 */
type ResolvedTheme = 'light' | 'dark'

/** 读系统偏好。 */
function systemTheme(): ResolvedTheme {
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

/** 读用户偏好。 */
function readPref(): ThemePref {
  const stored = localStorage.getItem('fmh-theme')
  if (stored === 'light' || stored === 'dark' || stored === 'system') return stored
  return 'system'
}

/** 把偏好解析成实际主题。 */
function resolve(pref: ThemePref): ResolvedTheme {
  return pref === 'system' ? systemTheme() : pref
}

/** 应用主题。 */
function applyTheme(pref: ThemePref) {
  const theme = resolve(pref)
  document.documentElement.dataset.theme = theme
  // ⚠️ 记的是**偏好**不是解析结果 ——
  //   否则用户选"跟随系统"时，下次访问会变成"固定"，
  //   系统主题再变就再也不跟了。
  localStorage.setItem('fmh-theme', pref)
}

/**
 * 读侧栏收起状态。
 *
 * ⚠️ 默认**展开** —— 侧栏是这个管理台的主要导航，
 *   一进来就只剩一列图标的话，新用户不知道能点哪儿。
 */
function readCollapsed(): boolean {
  return localStorage.getItem('fmh-sidebar-collapsed') === '1'
}

/** 应用外壳：侧栏 + 顶栏 + 移动端抽屉。 */
export function Shell({
  route,
  onNavigate,
  children,
  onLogout,
}: {
  route: RouteKey
  onNavigate: (r: RouteKey) => void
  children: React.ReactNode
  onLogout: () => void
}) {
  const [pref, setPref] = useState<ThemePref>(readPref)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [collapsed, setCollapsed] = useState(readCollapsed)
  const { data: status } = useStatus()

  // 实际生效的主题。
  const theme = resolve(pref)

  useEffect(() => {
    applyTheme(pref)
  }, [pref])

  // ⚠️ **跟随系统时，系统主题变了要跟着变** ——
  //   之前只在首次访问时读一次 matchMedia，之后就不管了。
  useEffect(() => {
    if (pref !== 'system') return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = () => applyTheme('system')
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [pref])

  // 侧栏收起状态要持久化。
  useEffect(() => {
    localStorage.setItem('fmh-sidebar-collapsed', collapsed ? '1' : '0')
  }, [collapsed])

  // 路由变化时关掉抽屉。
  useEffect(() => {
    setDrawerOpen(false)
  }, [route])

  // 分组导航。
  const groups = navItems.reduce<Record<string, NavItem[]>>((acc, item) => {
    ;(acc[item.group] ??= []).push(item)
    return acc
  }, {})

  const sidebar = (
    <div className="flex h-full flex-col">
      {/* 品牌 */}
      <div
        className={cn(
          'flex h-14 items-center gap-2.5',
          collapsed ? 'justify-center px-2' : 'px-4',
        )}
      >
        <BrandMark size="sm" />
        {!collapsed ? (
          <>
            <div className="min-w-0 flex-1">
              <div className="truncate text-[13.5px] font-semibold text-[var(--on-sidebar)]">
                Free Model House
              </div>
            </div>
            <Badge tone="accent" className="shrink-0">
              {status?.version ?? '…'}
            </Badge>
          </>
        ) : null}
      </div>

      {/* 导航 */}
      <nav className={cn('flex-1 overflow-y-auto py-2', collapsed ? 'px-1.5' : 'px-2.5')}>
        {Object.entries(groups).map(([group, items]) => (
          <div key={group} className="mb-4">
            {collapsed ? (
              // 收起时分组标题没有意义 —— 占位保持间距。
              <div className="h-4" />
            ) : (
              <div className="px-2 pb-1.5 text-[10.5px] font-semibold uppercase tracking-wider text-[var(--on-sidebar-dim)]">
                {group}
              </div>
            )}
            {items.map((item) => {
              const Icon = item.icon
              const active = route === item.key
              return (
                <button
                  key={item.key}
                  onClick={() => onNavigate(item.key)}
                  aria-current={active ? 'page' : undefined}
                  // ⚠️ 收起时靠 `title` 提示 —— 只有图标时得让人知道是什么。
                  title={collapsed ? item.label : undefined}
                  aria-label={collapsed ? item.label : undefined}
                  className={cn(
                    'fmh-touchable fmh-hoverable mb-0.5 flex w-full items-center gap-2.5 rounded-[var(--r-sm)] py-[7px] text-[13px] transition-colors duration-150',
                    collapsed ? 'justify-center px-0' : 'px-2',
                    'text-left',
                    // ⚠️ 用变量而不是 bg-white/10 ——
                    //   侧栏现在**跟随主题**（亮色是浅色底），
                    //   写死白色叠加层在亮色下会变成"灰底"。
                    active
                      ? 'bg-[var(--sidebar-active)] font-medium text-[var(--on-sidebar-active)]'
                      : 'text-[var(--on-sidebar-dim)] hover:bg-[var(--sidebar-hover)] hover:text-[var(--on-sidebar)]',
                  )}
                >
                  <Icon size={15} className="shrink-0" />
                  {!collapsed ? item.label : null}
                </button>
              )
            })}
          </div>
        ))}
      </nav>

      {/* ⚠️ **退出登录在分割线「上方」** ——
          分割线是"导航区 / 底部区"的分界。
          退出登录是**导航动作**，不是设置 ——
          放在线下面会让人以为它跟主题切换、收起按钮是一类。 */}
      {!collapsed ? (
        <button
          onClick={onLogout}
          className="fmh-touchable fmh-hoverable mb-1 flex w-full items-center gap-2.5 rounded-[var(--r-sm)] px-2.5 py-[7px] text-[13px] text-[var(--on-sidebar-dim)] transition-colors duration-150 hover:bg-[var(--sidebar-hover)] hover:text-[var(--on-sidebar)]"
          aria-label="退出登录"
        >
          <LogOut size={15} className="shrink-0" />
          <span>退出登录</span>
        </button>
      ) : null}

      {/* 底部 */}
      <div className="border-t border-[var(--sidebar-line)] px-2.5 py-2.5">
        <div className="flex items-center gap-1">
          {/* ⚠️ 三档循环：跟随系统 → 亮色 → 暗色 → 跟随系统。
              之前只有亮/暗两档，没有"跟随系统"这个选项 ——
              首次访问时读一次系统偏好，之后系统变了就不跟了。 */}
          <button
            onClick={() =>
              setPref(
                pref === 'system' ? 'light' : pref === 'light' ? 'dark' : 'system',
              )
            }
            className={cn(
              'fmh-touchable fmh-hoverable flex h-8 items-center gap-2 rounded-[var(--r-sm)] text-[12px] text-[var(--on-sidebar-dim)] transition-colors duration-150 hover:bg-[var(--sidebar-hover)] hover:text-[var(--on-sidebar)]',
              collapsed ? 'w-full justify-center px-0' : 'w-full px-2',
            )}
            aria-label="切换主题"
            title={
              pref === 'system'
                ? '主题：跟随系统（点击改为亮色）'
                : pref === 'light'
                  ? '主题：亮色（点击改为暗色）'
                  : '主题：暗色（点击改为跟随系统）'
            }
          >
            {pref === 'system' ? (
              <Monitor size={14} />
            ) : theme === 'dark' ? (
              <Sun size={14} />
            ) : (
              <Moon size={14} />
            )}
            {!collapsed ? (
              <span className="truncate">
                {pref === 'system' ? '跟随系统' : theme === 'dark' ? '暗色' : '亮色'}
              </span>
            ) : null}
          </button>
        </div>

        {/* 收起/展开按钮 —— 收起时它是**唯一**的展开入口。 */}
        <button
          onClick={() => setCollapsed((v) => !v)}
          className={cn(
            'fmh-touchable fmh-hoverable mt-1 flex h-8 items-center gap-2 rounded-[var(--r-sm)] text-[12px] text-[var(--on-sidebar-dim)] transition-colors duration-150 hover:bg-[var(--sidebar-hover)] hover:text-[var(--on-sidebar)]',
            collapsed ? 'w-full justify-center px-0' : 'px-2',
          )}
          aria-label={collapsed ? '展开侧边栏' : '收起侧边栏'}
          title={collapsed ? '展开侧边栏' : '收起侧边栏'}
        >
          {collapsed ? (
            <PanelLeftOpen size={14} />
          ) : (
            <>
              <PanelLeftClose size={14} />
              <span>收起</span>
            </>
          )}
        </button>
      </div>
    </div>
  )

  return (
    <div className="flex h-full">
      {/* 桌面侧栏 */}
      <aside
        className="fmh-sidebar hidden shrink-0 lg:block"
        style={{
          width: collapsed ? 'var(--sidebar-w-collapsed)' : 'var(--sidebar-w)',
          background: 'var(--sidebar)',
          // ⚠️ **侧栏右边要有明确的边界** ——
          //   之前侧栏底色跟主内容区几乎一样（亮色主题差 1.03:1），
          //   肉眼看不出哪里是侧栏、哪里是内容。
          //   加一层投影/边线，侧栏就"浮"在内容之上。
          boxShadow: 'var(--sidebar-shadow)',
        }}
      >
        {sidebar}
      </aside>

      {/* 移动端抽屉 */}
      {drawerOpen ? (
        <div className="fixed inset-0 z-50 lg:hidden">
          <div
            className="absolute inset-0 bg-black/45"
            onClick={() => setDrawerOpen(false)}
            aria-hidden
          />
          <aside
            className="fmh-sidebar absolute inset-y-0 left-0 w-[240px] shadow-[var(--shadow-3)]"
            style={{ background: 'var(--sidebar)' }}
          >
            <button
              onClick={() => setDrawerOpen(false)}
              className="fmh-touchable-icon fmh-hoverable absolute right-2 top-3.5 flex h-7 w-7 items-center justify-center rounded-[var(--r-sm)] text-[var(--on-sidebar-dim)] hover:bg-[var(--sidebar-hover)]"
              aria-label="关闭菜单"
            >
              <X size={16} />
            </button>
            {sidebar}
          </aside>
        </div>
      ) : null}

      {/* 主区 */}
      <div className="flex min-w-0 flex-1 flex-col">
        {/* 移动端顶栏 */}
        <header className="fmh-topbar flex h-14 items-center gap-3 border-b border-[var(--line)] bg-[var(--surface)] px-4 lg:hidden">
          <Button size="sm" variant="ghost" onClick={() => setDrawerOpen(true)} aria-label="打开菜单">
            <Menu size={16} />
          </Button>
          <div className="text-[14px] font-semibold text-[var(--ink)]">
            {navItems.find((i) => i.key === route)?.label}
          </div>
        </header>

        <main className="min-h-0 flex-1 overflow-y-auto">
          <div className="fmh-content mx-auto max-w-[1180px] px-4 py-5 sm:px-6 sm:py-6">
            {children}
          </div>
        </main>
      </div>
    </div>
  )
}

/** 页面标题。 */
export function PageTitle({
  title,
  description,
  actions,
}: {
  title: string
  description?: string
  actions?: React.ReactNode
}) {
  return (
    <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        <h1 className="text-[19px] font-semibold leading-7 tracking-[-0.01em] text-[var(--ink)]">
          {title}
        </h1>
        {description ? (
          <p className="mt-0.5 text-[13px] leading-[19px] text-[var(--ink-3)]">
            {description}
          </p>
        ) : null}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </div>
  )
}
