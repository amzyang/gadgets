---
name: lark-persona
description: >-
  从飞书聊天记录蒸馏个人画像：本地归档采集、关系图谱（高频伙伴、上下级推断草表）、
  个人说话风格蒸馏（按受众分层）与联系人关系卡。当用户说「蒸馏个人风格」「个人画像」
  「关系图谱」「更新聊天归档」「lark-persona」「让草稿/回复更像我」时使用。
  不负责实时监控（lark-watch）、消息收发（lark-im）。
---

# lark-persona — 飞书聊天记录 → 个人画像蒸馏

脚本路径中的 `{SKILL_DIR}` 指本 skill 的目录（本文件所在目录）。执行前先解析为绝对路径，
不要以 cwd 相对路径调用 `scripts/...`。

## 三层产物（~/.local/share/lark-persona/，全部只存本机）

| 层 | 路径 | 生成方式 | 用途 |
|---|---|---|---|
| 归档 | `archive/chats.ndjson`、`archive/msgs/<cid>/YYYY-MM.ndjson` | `collect.sh`（lark-cli 只读） | 语料底座，可反复重蒸 |
| 图谱 | `evidence/contacts.json`、`report.md`、`seeds.yaml`（人工校正） | `graph.sh`（纯 jq，零 LLM） | 高频伙伴 + 上下级草表 |
| 画像 | `persona/style.md`、`persona/contacts/<open_id>.md`、`persona/org.md` | LLM 蒸馏（见 references/distill.md） | 喂给 lark-watch 草稿 / 任意会话加载 |

## 命令

```bash
# 采集/增量更新归档（过去月按文件跳过，当月重拉；断点续传安全）
bash {SKILL_DIR}/scripts/collect.sh collect --months 6
bash {SKILL_DIR}/scripts/collect.sh org             # 通讯录组织数据（上级/部门 ground truth）
bash {SKILL_DIR}/scripts/collect.sh coverage        # 归档覆盖概况
bash {SKILL_DIR}/scripts/collect.sh chats           # 重建会话元数据

# 统计关系图谱 → evidence/contacts.json + report.md
bash {SKILL_DIR}/scripts/graph.sh run --top 20
```

首次全量采集耗时较长（数百会话 × 7 个月窗），建议 Monitor 承载后台跑
（Bash run_in_background 的子进程会被信号杀，不要用）。

## 增量更新（已跑过之后，用户说「更新归档/画像」时）

- **归档**：直接重跑 `collect.sh collect`，全自动增量——会话清单每次刷新（新同事/
  新群自动进目标集）、完整的过去月跳过、**月中采过的过去月按文件 mtime 自动补尾**
  （落盘时间早于月末 = 尾部不完整，重拉）、当月总是重拉。重跑成本约几分钟。
- **图谱**：`graph.sh run` 无状态幂等，归档更新后重跑即可（秒级）；组织人事变动时
  先重跑 `collect.sh org` 刷新 ground truth。
- **画像**：蒸馏游标在 `persona/manifest.json`（self、corpus_until、各风格层样本量、
  关系卡清单——每轮蒸馏收尾必须更新它）。判断哪些产物过期跑
  `graph.sh stale`（纯本地，约 8s）：对比 manifest 游标与归档新增量，输出 NDJSON——
  风格层新增 ≥ 上次样本 50%（`--style-percent`）、关系卡新增 ≥150 条
  （`--card-threshold`）、新进 top15 未建卡，三类过期目标。只重蒸 stale 条目，
  重蒸时**保留「草稿改善指引」节与反面清单第 10 条**（用户采纳的行为项，非语料观察）。
- **最高价值的增量语料**：lark-watch 草稿被用户修改后发出的 diff（草稿 vs 实发）——
  积累一批后喂回重蒸对应受众层，比重新读全量归档更有效。

## 工作流

1. **归档**：跑 `collect.sh collect`。用户只要求更新归档时到此为止。
2. **图谱**：先跑 `collect.sh org`（可重跑刷新），再跑 `graph.sh run`——报告中
   上下级优先用通讯录 ground truth（`archive/org.ndjson` 自动合并），可见范围外的人
   回退统计推断。把 `report.md` 给用户看，需纠正的人写
   `~/.local/share/lark-persona/seeds.yaml`（最高优先覆盖层，模板在 report 尾部）。
3. **蒸馏**：按 `references/distill.md` 的流程 map-reduce 蒸馏 persona 三件套。
   受众分层依据 = seeds.yaml（优先）+ contacts.json 的 guess（补充）。
4. **消费**：lark-watch 起草时按对端 open_id 读 `persona/contacts/<open_id>.md` +
   `persona/style.md` 对应受众层；其他会话中用户要求「以我的口吻」时加载 style.md。

## 隐私边界（硬约束）

- 归档与画像只落本机，不进 git、不外发、不贴入对外消息。
- 蒸馏会把消息内容送入 Claude API 处理；首次全量蒸馏前向用户确认知情。
- 只蒸馏用户本人画像；不为同事建独立画像 skill（关系卡仅描述「我与TA怎么互动」）。

## 关键事实（实测）

- `+chat-messages-list` 按月时间窗可回溯 ≥6 个月（p2p 与群均验证）；入群前消息不可见属正常。
- 消息 `create_time` 为分钟精度，同分钟排序靠 `message_position`。
- 组织字段走原生 API `contact/v3/users/:id`（`leader_user_id`/`department_ids`），
  需 scope `contact:user.department:readonly`。受租户通讯录可见范围限制
  （错误码 41050），通常仅本部门邻域可查；范围外回退统计推断，
  seeds.yaml 是最高优先人工覆盖层。
- `+get-user`/`+search-user` 等 CLI 封装不透出组织字段，查组织数据必须走 `lark-cli api`。
- 会话枚举含 `topic` 模式（话题群），当前采集只覆盖 p2p 与 group。
