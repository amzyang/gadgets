# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 仓库形态

个人 gadgets 单仓库，每个顶层目录是一个完全独立的小工具，无共享构建体系。

## 构建与测试

- `lark-watch/go`（唯一有构建体系的子项目）：
  - `make test` = `go vet ./...` + `go test -race ./...`（race detector 是默认要求）
  - `make lint` = `golangci-lint run`（errcheck 已禁用：本项目错误忽略是刻意的 best-effort 语义）
  - `make install` = lint + test + build，产物 `lark-watch/bin/lark-watch`（已 gitignore）
  - 重新构建后必须重启常驻的 Monitor 进程，否则仍在跑旧二进制
  - SQLite 依赖 `modernc.org/sqlite`（纯 Go、无 CGO）——不要改用 mattn/go-sqlite3 或引入 CGO
- `lark-persona`：`scripts/*.sh` 改动后运行 `bash lark-persona/tests/run.sh`（纯管道 diff 测试，不触网）

## 约束

- 运行时状态与用户数据在仓库外的 XDG 目录（`~/.local/state/lark-watch/`、`~/.config/lark-watch/`、`~/.local/share/lark-persona/`），属用户隐私，绝不提交进 git
- lark-* skill 运行时依赖全局安装的 `lark-cli`（`@larksuite/cli`，用户身份认证）
- 各工具均为 macOS-only（osascript 通知、kitty remote control），无需考虑跨平台
