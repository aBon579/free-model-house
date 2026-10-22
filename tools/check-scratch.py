#!/usr/bin/env python3
"""检查 scratch 运行时阶段的陷阱。

⚠️ 为什么需要这个：
   Docker 一次只报**一个**错 —— 修完一个才暴露下一个。
   FMH 在 NAS 上连续踩了四个：401 镜像源 / go mod download exit 1 /
   zoneinfo not found / USER root 找不到 passwd。
   这个脚本把这类问题**一次性查全**。

用法：
   python3 tools/check-scratch.py            # 检查当前目录的 Dockerfile
   python3 tools/check-scratch.py <路径>     # 检查指定目录
"""
import os
import re
import sys


def find_project_root(start):
    """向上找有 Dockerfile + docker-compose.yml 的目录。

    ⚠️ 优先检查 start 本身，然后才往上 ——
       免得从 tools/ 目录跑时找到的是**上级**目录。
    """
    d = os.path.abspath(start)
    if os.path.isfile(d):
        d = os.path.dirname(d)
    if (os.path.exists(os.path.join(d, "Dockerfile")) and
            os.path.exists(os.path.join(d, "docker-compose.yml"))):
        return d
    parent = os.path.dirname(d)
    while True:
        if (os.path.exists(os.path.join(parent, "Dockerfile")) and
                os.path.exists(os.path.join(parent, "docker-compose.yml"))):
            return parent
        up = os.path.dirname(parent)
        if up == parent:
            return d
        parent = up


def parse_stages(text):
    """返回 [(阶段名, [有效指令])]。"""
    stages = []
    cur_name, cur = None, []
    for ln in text.splitlines():
        s = ln.strip()
        if not s or s.startswith("#"):
            continue
        if s.upper().startswith("FROM"):
            if cur_name is not None:
                stages.append((cur_name, cur))
            parts = s.split()
            cur_name = parts[1] if len(parts) > 1 else "?"
            cur = []
            continue
        if cur_name is not None:
            cur.append(s)
    if cur_name is not None:
        stages.append((cur_name, cur))
    return stages


def main():
    root = find_project_root(sys.argv[1] if len(sys.argv) > 1 else os.getcwd())
    df_path = os.path.join(root, "Dockerfile")
    comp_path = os.path.join(root, "docker-compose.yml")

    if not os.path.exists(df_path):
        print(f"❌ 找不到 Dockerfile：{df_path}")
        return 1

    df = open(df_path, encoding="utf-8").read()
    comp = open(comp_path, encoding="utf-8").read() if os.path.exists(comp_path) else ""

    problems = []
    notes = []

    # ── 找 scratch 阶段 ──────────────────────────────────────────
    scratch = None
    for name, cmds in parse_stages(df):
        if name.split(":")[0] == "scratch":
            scratch = cmds
    if scratch is None:
        print("ℹ️  没有 scratch 阶段 —— 跳过")
        return 0

    print("=== scratch 阶段的有效指令 ===")
    for s in scratch:
        print(f"  {s}")
    print()

    # ── ① USER 必须是数字 ────────────────────────────────────────
    for s in scratch:
        m = re.match(r"USER\s+(\S+)", s, re.I)
        if m:
            u = m.group(1)
            if ":" in u:
                name_part, gid = u.split(":", 1)
                if not name_part.isdigit():
                    problems.append(f"USER {u} 的用户名部分不是数字 —— "
                                    f"scratch 里没有 /etc/passwd，启动会报 "
                                    f"'unable to find user {name_part}'。改成 USER 0。")
                else:
                    notes.append(f"USER {u}（数字）✅")
            elif u.isdigit():
                notes.append(f"USER {u}（数字）✅")
            else:
                problems.append(f"USER {u} 用的是【名字】—— scratch 里没有 /etc/passwd，"
                                f"启动会报 'unable to find user {u}'。改成 USER 0。")

    # ── ② os/user 依赖 ───────────────────────────────────────────
    uses_os_user = False
    for dirpath, dirs, files in os.walk(root):
        dirs[:] = [d for d in dirs if d not in ("vendor", "node_modules", ".git", "dist")]
        for f in files:
            if not f.endswith(".go"):
                continue
            p = os.path.join(dirpath, f)
            try:
                body = open(p, encoding="utf-8").read()
            except Exception:
                continue
            if re.search(r'"os/user"', body):
                rel = os.path.relpath(p, root)
                if not any(x in rel for x in ("vendor", "node_modules")):
                    print(f"❌ {rel} 用了 os/user —— scratch 里需要拷 /etc/passwd")
                    uses_os_user = True
    if not uses_os_user:
        print("✅ 没有用 os/user —— 不需要 /etc/passwd")

    # ── ③ 时区数据 + TZ ──────────────────────────────────────────
    if re.search(r"COPY\s+--from=\S+\s+/zoneinfo/\s+/usr/share/zoneinfo/", df):
        print("✅ 时区数据已拷进 /usr/share/zoneinfo/")
    elif re.search(r"COPY\s+--from=\S+\s+/usr/share/zoneinfo", df):
        print("❌ 从 alpine 拷 /usr/share/zoneinfo —— alpine 不含 tzdata，会报 not found")
        problems.append("时区数据来源错了 —— alpine 不含 /usr/share/zoneinfo")
    else:
        print("❌ 缺时区数据 —— TZ 会静默退化成 UTC（不报错，但按天统计会错位）")
        problems.append("缺时区数据 —— TZ 会静默退化成 UTC")

    if re.search(r"TZ:\s*", comp):
        print("✅ compose 设了 TZ")
    else:
        print("⚠️  compose 没设 TZ —— 程序会按 UTC 切分用量")

    # ── ④ healthcheck 的命令必须在镜像里 ─────────────────────────
    m = re.search(r"healthcheck:(.*?)(?=\n    \w|\Z)", comp, re.S)
    if m:
        blk = m.group(1)
        t = re.search(r"test:\s*\[(.*?)\]", blk)
        if t:
            for c in [x.strip().strip('"') for x in t.group(1).split(",")]:
                if c in ("CMD", "CMD-SHELL") or not c.startswith("/"):
                    continue
                srcs = re.findall(r"COPY\s+--from=\S+\s+(\S+)\s+(\S+)", df)
                if any(d == c or c.startswith(d.rstrip("/") + "/") for _, d in srcs):
                    print(f"✅ healthcheck 用的 {c} 在镜像里")
                else:
                    print(f"❌ healthcheck 用 {c}，但它没被 COPY 进镜像")
                    problems.append(f"healthcheck 用 {c}，但它不在镜像里")

    # ── ⑤ ENTRYPOINT 必须存在 ────────────────────────────────────
    m = re.search(r'ENTRYPOINT\s+\["([^"]+)"\]', df)
    if m:
        ep = m.group(1)
        srcs = re.findall(r"COPY\s+--from=\S+\s+(\S+)\s+(\S+)", df)
        if any(d == ep or ep.startswith(d.rstrip("/") + "/") for _, d in srcs):
            print(f"✅ ENTRYPOINT {ep} 在镜像里")
        else:
            print(f"❌ ENTRYPOINT {ep} 没被 COPY 进镜像")
            problems.append(f"ENTRYPOINT {ep} 不在镜像里")

    print()
    if problems:
        print("❌ 发现问题：")
        for p in problems:
            print(f"  - {p}")
        return 1
    print("✅ scratch 阶段没发现问题")
    for n in notes:
        print(f"  {n}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
