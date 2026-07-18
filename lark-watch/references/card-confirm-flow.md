# 飞书卡片确认闭环（二进制直发架构）

默认确认通道（起草后立即发卡，终端确认并行可用）：草稿经 `send-card` 发成卡片，
用户在飞书点「发送」即以本人身份发出回复。**点击后的一切由 `lark-watch run` 内的卡片链路直接执行，
不经过模型——零推理、零 token**。点击按钮 = 用户确认（单击即发，无二次弹窗，
幂等键防连点）；依然禁止任何未经点击/终端确认的发送。

## 前置条件（均已在本机验证通过）

1. 开发者后台「事件与回调 → 回调配置」已开启（未开启时监听正常启动但收不到事件，无预检）。
2. bot 身份可用（`lark-cli auth status`），scope `im:message:readonly`。
3. `lark-watch run` 在跑（卡片回调链路随之常驻，无需单独进程）。

## 数据流

```
P0 消息 → 模型起草（1–3 条候选）→ lark-watch send-card（pending 入库 + 渲染模板 + bot 发卡给用户本人）
  ├─ 点「发送」（多候选时「发送 ①/②/③」，回调带 idx）→ 二进制：读 pending →
  │  以所选候选 +messages-reply --as user → 删 pending → 改卡「✅ 已发送」（只留所选候选块）
  ├─ 点「忽略」→ 二进制：删 pending → 改卡「已忽略」
  └─ 点「复制草稿」→ 二进制：bot 把全部候选逐条以纯文本私发给用户（长按/右键即可复制），
     pending 保留、不改卡——文本消息到达本身就是反馈
模型只负责起草 + 调 send-card；点击后完全旁路。
```

## 起草命令（模板渲染/转义/pending 全部内置于二进制）

```bash
printf '%s' '<草稿>' | {SKILL_DIR}/bin/lark-watch send-card \
  --mid <原消息 message_id> --draft - \
  --original '<原消息文本>' --from '<发送者名>' \
  --scene '<私聊|群名>' --t '<消息时间>'
```

- 必填仅 `--mid`/`--draft`；`--original/--from/--scene/--t` 为卡片展示字段，
  可省略（空值对应片段整体省略），P0 事件里都有、建议带上。
- `--draft` 接文件路径或 `-`（stdin），可重复给出 2–3 条候选（`-` 至多一次；
  多候选用进程替换 `--draft <(printf '%s' '<候选>')` 免临时文件）。多候选时每条
  候选块标注 ①②③ 并各带自己的发送按钮（callback value 含 `idx`），format 应用
  于全部候选。
- `--format text|markdown`（默认 text）随 pending 落盘：markdown 时草稿在卡片里
  按 markdown 渲染（保留围栏，开围栏前自动补空行——卡片方言要求），确认后以
  `--markdown` 走 post 富文本回复；text 时确认后以 `--text` 纯文本回复。
- 转义由二进制处理：`<at ...>名字</at>` → `@名字`，markdown 特殊字符转 HTML 实体
  （原始消息引用）；text 格式草稿内代码围栏降级为 `'''`、整体以代码块展示。
- pending（草稿 + 卡片原稿）写入 SQLite `pending` 表——改卡用本地原稿而非回调的
  `card_content`（服务端 user_dsl 序列化会丢 markdown 换行，实测踩过）。
- 卡片按钮：每条候选一个发送按钮（primary_filled，无 confirm 弹窗；单候选文案
  「发送」，多候选「发送 ①/②/③」）+ 底部共享「复制草稿」「忽略」。
- 模板实现在 `go/watch/cardtpl.go`；改样式先过 lark-im 卡片工作流
  （`~/.claude/skills/lark-im/references/card/lark-im-card-create.md`）的 Gate，
  改完 `cd {SKILL_DIR}/go && make install`。

## 回调链路（run 内置，自动监督）

- `run` 内部拉起 `lark-cli event consume card.action.trigger --as bot` 子进程读
  stdout；stdin 由父进程持有保活；SIGTERM 经 cmd.Cancel 传递（勿 kill -9，
  会泄漏服务端订阅）。
- 子进程异常退出自动退避重启（5s→15s→60s）；连续 3 次快速失败发一条
  `{"p":"alert","kind":"card-daemon"}`（仅卡片按钮降级，轮询不受影响）。
- event_id 去重滚动 1000 条（`handled` 表，防重启后重放）。
- 改卡 token 30 分钟/最多 2 次、须整卡替换；用尽时改卡失败仅记 stderr
  （发送本身不受影响）。
- 分支语义：send（按 idx 发对应候选；幂等键=mid 防连点双发，选定一条后其余候选
  同键去重、不会二发）/ ignore / copy（逐条回发全部候选，不改卡）/
  pending 缺失改卡「已失效」/ idx 越界（同 mid 重发覆盖 pending 后旧卡点了消失的
  候选）改卡「已失效」、pending 保留 / reply 失败保留 pending 改卡「发送失败」。

## 排错

| 现象 | 排查 |
|---|---|
| 点按钮无反应 | `lark-watch status` 看 `consumer_state`；`lark-cli event status`；后台回调配置 |
| consumer 反复重启 | stderr 里 consume 退出原因；`card.action.trigger` 仅允许一个 consumer，查残留进程 |
| 改卡失败日志 | token 30min/2 次用尽，属预期；发送本身不受影响 |
| 卡片显示「草稿已失效」 | pending 已被处理或清理，回终端重新起草 |
| 单元测试 | `cd {SKILL_DIR}/go && go test ./...`（card_test.go 覆盖回调分支+去重+多候选模板渲染） |
