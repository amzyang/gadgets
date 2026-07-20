---
name: local-repos
provides: 用户在写的代码/服务的 schema、接口、字段定义与近期改动
when: 话题涉及代码标识符（字段名/接口名/服务名），或对方在讨论用户的项目设计
cost: fast
---
rg -il '<关键术语>' <你的代码目录，如 ~/Vcs> -g '!node_modules' -g '!vendor' \
  -g '!dist' | head -5
命中后 Read 相关 schema/代码/文档拿依据，必要时 git log 看近期改动；
找不到即算无据，不扩大搜索。
