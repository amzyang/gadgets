# 蒸馏流程（LLM map-reduce）

前置：归档已采集、`graph.sh run` 已产出 `evidence/contacts.json`、用户已校正 `seeds.yaml`。
DATA=~/.local/share/lark-persona。产物写入 `$DATA/persona/`。

## Step 0 —— 增量判定（非首轮时）

跑 `graph.sh stale`（读 `persona/manifest.json` 游标 vs 归档新增），只对输出中
`stale: true` 的风格层/关系卡、以及 `card-missing`（新进 top15）执行下面的步骤；
全 fresh 则无事可做。首轮（无 manifest）跳过本步，做全量。

方法论借鉴 immortal-skill：证据分级 + 矛盾并存；本流程只蒸「互动风格」与「关系」两维，
不做记忆/性格维（非目标）。

## 证据分级与矛盾

- `verbatim`：本人原话直接引用（最高置信，卡片中标 `>` 引用块）。
- `impression`：从多条消息归纳的印象（标注样本量，如「基于 2026-01~07 共 340 条」）。
- 矛盾证据不强行统一：同一特征出现相反证据时并列写出（如「对上级用『收到』，
  但深夜加急时会直接语音」），不删一留一。

## 语料提取（jq 配方）

```bash
SELF=$(lark-cli auth status | jq -r '.identities.user.openId')
DATA=~/.local/share/lark-persona

# 我发出的全部消息（style 语料，量级远小于全量）
cat "$DATA"/archive/msgs/*/*.ndjson | jq -c --arg s "$SELF" 'select(.fid == $s and .type == "text")'

# 我与某联系人的 p2p 对话（关系卡语料；cid 从 archive/chats.ndjson 按 tid 查）
cat "$DATA"/archive/msgs/<cid>/*.ndjson | jq -c 'select(.type == "text")'

# 按受众切分我的消息：先从 seeds.yaml + contacts.json 得到 上级/下属/平级 的 open_id 集，
# 再按所在 p2p 会话归组；群消息全部归入「群发言」层。
```

## Step 1 — style.md（我的说话风格，按受众分层）

Map：把「我发出的消息」按受众层切块（每块 ≤300 条），每块由一个子代理提炼：
句长与节奏、开场/收尾习惯、语气词与口头禅（verbatim 举例 ≥5 条）、emoji/表情包频率、
称呼方式、技术词汇 vs 口语的配比、拒绝/催促/求助时的表达方式。

Reduce：聚合各块结论，按下方模板成文；跨块矛盾按「矛盾并存」规则保留。

```markdown
# style.md 模板
## 总则（跨受众一致的特征）
## 对上级
## 对平级
## 对下属
## 群发言
## 反面清单（我从不这样说）
<!-- 每节含: 特征要点 + verbatim 例句(引用块) + 证据标注 -->
```

## Step 2 — contacts/<open_id>.md（关系卡，取 contacts.json total 前 15 人）

每人一个子代理，输入 = 该 p2p 会话最近 200~400 条 + contacts.json 中该人统计行 +
seeds.yaml 定位。产出：

```markdown
# <姓名>（<open_id>）
- 关系：上级/平级/下属/跨部门（来源: seeds|推断）
- 我的称呼方式：<verbatim>
- 高频话题域：
- 语气分寸：正式度、可开玩笑程度、响应期望（参考回复中位数）
- 典型往来示例（verbatim 引用 2~3 段）
- 矛盾/例外：
```

## Step 3 — org.md（汇报链视图）

骨架优先级：seeds.yaml（人工覆盖）> `archive/org.ndjson`（通讯录 ground truth，
contacts.json 已由 org-merge 合并为 `relation` 字段）> 统计推断 guess。
推断补入的条目全部标注「推断，置信 低/中」；来源间冲突记录冲突不静默覆盖。

## Step 4 — 更新 manifest（蒸馏收尾必做，否则增量游标失真）

把本轮实际使用的语料边界写回 `persona/manifest.json`：

```json
{"version": 1, "self": "<ou_自己>", "distilled_at": "<今天>",
 "corpus_until": "<语料截止时刻，格式 YYYY-MM-DD HH:MM，保守取归档拉取当日 00:00>",
 "style": {"up": {"n": <条数>, "peer_ids": ["<上级 ou>", "..."]},
           "peer": {"n": <条数>}, "group": {"n": <条数>}},
 "contacts": {"<ou_x>": {"name": "<姓名>", "n": <该卡语料条数>}}}
```

增量重蒸只更新变动的字段（某层重蒸 → 更新该层 n 与全局 corpus_until；补卡 →
contacts 加条目）。

## Step 5 — 验收（盲测）

从近期真实收到的消息中挑 5 条（覆盖上级/平级/群各至少 1 条），用 style.md + 关系卡
起草回复，请用户判断「像不像我、称呼语气对不对」；不达标则把用户反馈追加进对应文件的
反面清单/矛盾节后重蒸该节。
