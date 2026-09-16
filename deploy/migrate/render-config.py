#!/usr/bin/env python3
"""把旧环境的 config.yaml 改写成容器网络里可用的形态。

**为什么不用正则、也不用 yaml.dump:**

- 正则版踩过坑(2026-09-16):`re.sub(r'^(\\s*host:\\s*).*', 'postgres', text, count=1)`
  改的是文件里**第一个** `host:`,而那是 `server.host` —— 结果把监听地址改成了
  "postgres",database.host 反而原样留着 localhost。一个正则同时改错了两处。
- `yaml.safe_load` + `yaml.dump` 能改对,但会丢掉全部注释、重排键序、改变引号风格。
  这份文件是运维会手工看和改的,把它洗成另一副样子是有代价的。

所以按**段 + 缩进**做行级精确替换:只动指定顶层段内的指定键,其余字节逐字保留。
"""
import re
import sys


def replace_in_section(lines, section, key, value):
    """在顶层 `section:` 块内替换 `key:` 的值。返回 (新行列表, 是否改到)。

    块的边界按缩进判定：从 `section:` 之后第一行开始，直到出现一个缩进为 0
    的非空行为止。这正是 YAML 的块语义，比"找下一个顶层键名"稳——后者会被
    注释行和空行骗到。
    """
    out, in_section, changed = [], False, False
    for line in lines:
        if re.match(rf'^{re.escape(section)}:\s*(#.*)?$', line):
            in_section, out = True, out + [line]
            continue
        if in_section:
            # 缩进归零且非空、非注释 → 段结束
            if line.strip() and not line.startswith((' ', '\t')) and not line.lstrip().startswith('#'):
                in_section = False
            else:
                m = re.match(rf'^(\s+{re.escape(key)}:\s*)(.*?)(\s*#.*)?$', line)
                if m:
                    out.append(f"{m.group(1)}{value}{m.group(3) or ''}\n")
                    changed = True
                    continue
        out.append(line)
    return out, changed


def main():
    src, dst, pg_password = sys.argv[1], sys.argv[2], sys.argv[3]
    lines = open(src, encoding='utf-8').readlines()

    # (段, 键, 新值, 为什么)
    edits = [
        ('database', 'host', 'postgres', '容器网络里的服务名'),
        ('database', 'password', pg_password, '新环境的库口令是新生成的，与旧环境无关'),
        ('redis', 'host', 'redis', '容器网络里的服务名'),
        # 新 redis 没有设口令(它只在 compose 网络内可达，未映射端口)。
        # 留着旧口令会让 sub2api 带着一个错的 AUTH 去连，直接连不上。
        ('redis', 'password', '""', '新 redis 未设口令'),
    ]

    report = []
    for section, key, value, why in edits:
        lines, ok = replace_in_section(lines, section, key, value)
        shown = '<已隐去>' if 'password' in key else value
        report.append(f"  {'改到' if ok else '未找到'}  {section}.{key} = {shown}   ({why})")

    open(dst, 'w', encoding='utf-8').writelines(lines)
    print('\n'.join(report))

    # 未找到就是失败：静默跳过会让 sub2api 拿着 localhost 去连数据库，
    # 表现为"起来了但一直连不上"，比直接报错难查得多。
    if any('未找到' in r for r in report):
        print('有键没改到 —— 拒绝当作成功', file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    main()
