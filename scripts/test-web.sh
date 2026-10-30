#!/bin/sh
# 前端纯逻辑测试入口。
#
# 这些测试只覆盖不依赖 DOM 的模块（wire 运行时校验、有界窗口、格式化与筛选）。
# 用 Node 内置测试运行器 + 原生 TypeScript 类型剥离，不引入额外测试框架。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root/web"

node --version
node --test test/logic.test.ts
