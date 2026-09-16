#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 ACCEPTANCE.yaml 的 toolchain.test_glob 展开成 `go test -run` 的正则。

为什么不把 205 个测试名直接写死在 calibration.command 里：
写死的名单是一份**冻结的分母**。此后新增的验收测试不会进入判决命令，套件在
无人察觉的情况下变小，而报表上「全绿」的样子一个字都不变 —— 那正是 S5 反复点名的
「分数不降靠缩分母实现」的同一种病，只不过缩的是测试集而不是变异体集。
所以名单每次现取：真源是 test_glob，这个脚本只做展开。

为什么判决命令要限定到 test_glob 而不是 `./internal/...` 全量：
ACCEPTANCE.yaml 里 test_glob 那一段已经写明，本验收清单的套件**只包含**本次移植
新增/触及的测试，不含 sub2api 既有的上千个测试。拿全量当判决命令有两个后果：
① 别的 lane 的失败会染红本清单的每一次判决（阴性标定当场失去鉴别力）；
② 埋雷被清单之外的测试杀掉也会记成「本清单发现了它」，充分性分数虚高。
限定之后 C9 的两条既存红仍在集合内（它的 check 就是 mirasim_identity_test.go），
所以这不是把红躲开，只是把判决范围对齐到清单自己声明的那一份。
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def test_globs(acc_path):
    """只抽 toolchain.test_glob 这一段；不引 PyYAML，判决命令不该依赖第三方包。"""
    out, depth, inside = [], None, False
    for line in open(acc_path, encoding="utf-8"):
        if re.match(r"^\s*test_glob:\s*$", line):
            inside = True
            depth = len(line) - len(line.lstrip())
            continue
        if not inside:
            continue
        if not line.strip() or line.lstrip().startswith("#"):
            continue
        ind = len(line) - len(line.lstrip())
        m = re.match(r"^\s*-\s*\"([^\"]+)\"\s*$", line)
        if m and ind > depth:
            out.append(m.group(1))
            continue
        if ind <= depth:
            break
    return out


def main():
    import glob
    names = set()
    for pat in test_globs(os.path.join(ROOT, "ACCEPTANCE.yaml")):
        for f in glob.glob(os.path.join(ROOT, pat), recursive=True):
            names |= set(re.findall(r"^func (Test\w+)\(", open(f, encoding="utf-8").read(), re.M))
    if not names:
        sys.stderr.write("test_glob 展开为空 —— 判决命令会退化成跑全量，拒绝输出\n")
        return 1
    sys.stdout.write("^(" + "|".join(sorted(names)) + ")$")
    return 0


if __name__ == "__main__":
    sys.exit(main())
