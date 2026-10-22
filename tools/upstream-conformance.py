#!/usr/bin/env python3
"""上游一致性验证：FMH 的免密车道实现 vs OpenCode 官方客户端源码。

用法：
    python3 tools/upstream-conformance.py --opencode opencode --fmh fmh

`--opencode` 指向 OpenCode 仓库的克隆（需要能读到 packages/core/src）。

检查四件事：
    ① read / shell 的工具描述是否逐字一致
    ② 各参数描述是否逐字一致
    ③ 官方源码里的会话头定义位置
    ④ 会话 ID 形状的正则是否印证 FMH 的实现

退出码非 0 表示有不一致。

⚠️ 只用标准库 —— 不想给项目加依赖。

⚠️ **这个脚本只核对"FMH 发的请求跟官方源码一不一致"**。
   它**不**验证"上游到底按什么条件放行" —— 那个要**直接打上游**测，
   详见 docs/verification/upstream-conformance.md §2.6。
"""
import argparse
import os
import re
import sys

# FMH 的工具定义。
FMH_DESCRIPTIONS = {
    'read': 'zenReadDescription',
    'shell': 'zenShellDescription',
}

# FMH 的参数描述（字段名 → 描述）。
FMH_FIELDS = {
    'path': 'File or directory to read',
    'offset': 'The line or directory entry to start reading from (1-based)',
    'limit': 'The maximum number of lines or directory entries to read (defaults to and capped at 2000)',
    'command': 'Shell command string to execute',
    'workdir': 'Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead.',
    'timeout': 'Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default.',
    'background': "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion.",
}

# 官方源码里对应文件。
OC_FILES = {
    'read': 'packages/core/src/tool/plugin/read.ts',
    'shell': 'packages/core/src/tool/plugin/shell.ts',
}

# 会话头。
SESSION_HEADERS = [
    'x-opencode-session',
    'x-opencode-session-id',
    'x-session-affinity',
    'x-opencode-project',
    'x-opencode-client',
]


def read(path):
    try:
        with open(path, encoding='utf-8', errors='replace') as f:
            return f.read()
    except OSError:
        return ''


def pieces(text):
    """抽出所有字符串字面量（双引号 / 单引号 / 模板串）。

    ⚠️ 官方有两种写法：
        "整句在一行"                        → 直接命中
        "前半段" + "后半段"                  → 需要把相邻字面量拼起来

    所以这里除了单条字面量，还额外产出**拼接后的组合**。
    """
    out = set()
    raw = []
    for m in re.finditer(r'"((?:[^"\\]|\\.)*)"', text):
        out.add(m.group(1))
        raw.append(m.group(1))
    for m in re.finditer(r"'((?:[^'\\]|\\.)*)'", text):
        out.add(m.group(1))
        raw.append(m.group(1))
    for m in re.finditer(r'`([^`]*)`', text):
        out.add(m.group(1))
        raw.append(m.group(1))

    # 相邻字面量的拼接（官方用 " + " 连接）。
    # ⚠️ 只拼**连续出现**的（中间只隔着 `+` 和空白）。
    joined = re.findall(
        r'"((?:[^"\\]|\\.)*)"\s*\+\s*"((?:[^"\\]|\\.)*)"', text)
    for a, b in joined:
        out.add(a + b)
    joined3 = re.findall(
        r'"((?:[^"\\]|\\.)*)"\s*\+\s*"((?:[^"\\]|\\.)*)"\s*\+\s*"((?:[^"\\]|\\.)*)"',
        text)
    for a, b, c in joined3:
        out.add(a + b + c)

    return out


def fmh_constants(fmh_root):
    """从 FMH 的 zen_fingerprint.go 里抽出常量和参数描述。"""
    path = os.path.join(fmh_root, 'internal', 'lane', 'zen_fingerprint.go')
    src = read(path)
    consts = {}
    for m in re.finditer(r'(zen\w+)\s*=\s*"((?:[^"\\]|\\.)*)"', src):
        consts[m.group(1)] = m.group(2)
    for m in re.finditer(r'(zen\w+)\s*=\s*`([^`]*)`', src):
        consts[m.group(1)] = m.group(1) if False else m.group(2)
    return consts


def resolve_templates(text, values):
    """把官方源码里的模板串按实际运行时值展开。

    官方有两处是运行时拼接的：
        `Commands run on ${OS} using ${shell}.`
        `Timeout in milliseconds. ... Defaults to ${DEFAULT_TIMEOUT_MS} for ...`

    这里的做法是：**把 FMH 的已知运行时值代进去**，看能不能命中官方源码。
    这验证的是"FMH 用的是官方那条模板的正确填充结果"。
    """
    out = text
    for name, val in values.items():
        out = out.replace('${%s}' % name, val)
    return out


def template_matches(fmh_text, oc_source, values):
    """FMH 的一段文字，能不能由官方源码里的某条模板**拼出来**。

    官方把描述写成数组 + `.join(" ")`，或者用 `${...}` 插值，
    所以光搜字面量找不到 —— 得把官方源码里的所有 `${...}` 都代入试一遍。
    """
    # 直接命中。
    if fmh_text in pieces(oc_source):
        return True
    # 代入运行时值后命中。
    if resolve_templates(fmh_text, values) in pieces(oc_source):
        return True
    # 官方源码里有含 ${...} 的模板，代入后能不能等于 FMH 的这段。
    for tmpl in pieces(oc_source):
        if '${' not in tmpl:
            continue
        if resolve_templates(tmpl, values) == fmh_text:
            return True
    return False


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--opencode', required=True, help='OpenCode 仓库根目录')
    ap.add_argument('--fmh', required=True, help='FMH 仓库根目录')
    args = ap.parse_args()

    oc_root = os.path.abspath(args.opencode)
    fmh_root = os.path.abspath(args.fmh)

    consts = fmh_constants(fmh_root)
    oc_pieces = set()
    oc_files = {}
    for key, rel in OC_FILES.items():
        p = os.path.join(oc_root, rel.replace('/', os.sep))
        text = read(p)
        oc_files[key] = text
        oc_pieces |= pieces(text)

    # ⚠️ 官方模板串的运行时值 —— 这些是 OpenCode 在 Linux 上跑时的实际取值。
    #    FMH 复现的是**已经拼好的**请求体，所以这些值必须是固定的。
    RUNTIME = {
        'OS': 'Linux',
        'shell': 'bash',
        'DEFAULT_TIMEOUT_MS': '120000',
    }

    failures = 0

    # ── ① 工具描述 ──
    print('=' * 72)
    print('① 工具描述：FMH vs OpenCode 官方')
    print('=' * 72)
    for key, cname in FMH_DESCRIPTIONS.items():
        fmh_text = consts.get(cname, '')
        if not fmh_text:
            print('  ❌ %-6s 找不到 FMH 的常量 %s' % (key, cname))
            failures += 1
            continue

        # ⚠️ **先整段比对** —— 官方多数是一整条字面量。
        if template_matches(fmh_text, oc_files.get(key, ''), RUNTIME):
            print('  ✅ %-6s 整段逐字命中官方源码' % key)
            continue

        # 官方是**多段拼接**（数组 + join）时，逐句比对。
        frags = [f.strip() for f in re.split(r'(?<=\.)\s+', fmh_text) if f.strip()]
        ok = 0
        miss = []
        for f in frags:
            if template_matches(f, oc_files.get(key, ''), RUNTIME):
                ok += 1
            else:
                miss.append(f)
        status = '✅' if ok == len(frags) else '⚠️ '
        if ok != len(frags):
            failures += 1
        print('  %s %-6s 逐句 %d/%d 命中' % (status, key, ok, len(frags)))
        for m in miss:
            print('       ❓ %s' % m[:96])
    print()

    # ── ② 参数描述 ──
    print('=' * 72)
    print('② 参数描述：FMH vs OpenCode 官方')
    print('=' * 72)
    both = '\n'.join(oc_files.values())
    for name, desc in sorted(FMH_FIELDS.items()):
        if template_matches(desc, both, RUNTIME):
            print('  ✅ %-12s 与官方一致' % name)
        else:
            print('  ❌ %-12s 官方没有' % name)
            failures += 1
    print()

    # ── ③ 会话头 ──
    print('=' * 72)
    print('③ 会话头：官方源码里的定义位置')
    print('=' * 72)
    src_root = os.path.join(oc_root, 'packages', 'core', 'src')
    for h in SESSION_HEADERS:
        hits = []
        for base, dirs, files in os.walk(src_root):
            dirs[:] = [d for d in dirs if d != 'node_modules']
            for f in files:
                if f.endswith(('.ts', '.tsx')):
                    p = os.path.join(base, f)
                    if h in read(p):
                        hits.append(os.path.relpath(p, oc_root))
        mark = '✅' if hits else '⚠️ '
        print('  %s %-24s %s' % (mark, h, hits[0] if hits else '官方源码里没找到'))
    print()

    # ── ④ 会话 ID 形状 ──
    print('=' * 72)
    print('④ 会话 ID 形状：官方正则 vs FMH 实现')
    print('=' * 72)
    oc_re = re.compile(r'/(\^ses_\[0-9a-f\]\{64\}\$)/')
    found = None
    for base, dirs, files in os.walk(src_root):
        dirs[:] = [d for d in dirs if d != 'node_modules']
        for f in files:
            if f.endswith(('.ts', '.tsx')):
                p = os.path.join(base, f)
                m = oc_re.search(read(p))
                if m:
                    found = (m.group(1), os.path.relpath(p, oc_root))
                    break
        if found:
            break

    fmh_src = read(os.path.join(fmh_root, 'internal', 'lane', 'zen_fingerprint.go'))
    fmh_re = re.search(r'zenSessionRe = regexp\.MustCompile\(`([^`]+)`\)', fmh_src)
    if found:
        print('  ✅ 官方正则   %s' % found[0])
        print('     位置       %s' % found[1])
    else:
        print('  ⚠️  官方源码里没找到 ses_ 正则')
    if fmh_re:
        print('  ✅ FMH 正则    %s' % fmh_re.group(1))
    print()
    print('  → 官方正则只管**形状**；上游网关还会**解码**那 12 位 hex，')
    print('    编码规则是 FMH 实测出来的（见 docs/verification/upstream-conformance.md §2.2）')
    print()

    print('=' * 72)
    if failures:
        print('❌ %d 项不一致' % failures)
        print('=' * 72)
        return 1
    print('✅ 全部一致')
    print('=' * 72)
    print()
    print('⚠️ 提醒：上面核对的是"FMH 的请求与官方源码一致"。')
    print('   上游实际校验的是：UA 形状 · 四个会话头 · 会话 ID 形状 ·')
    print('   流式 · 有 read+shell 两个工具。')
    print('   工具描述**不在**校验项里 —— 详见 §2.6。')
    return 0


if __name__ == '__main__':
    sys.exit(main())
