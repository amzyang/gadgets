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

发卡同时释放的 alerter 通知横幅也带「发送」动作：回调
`lark-watch send-draft --mid <mid>` 直接发候选①（幂等键同为 mid，横幅/卡片双端
点击不会双发）。发出后按发卡时回填的卡片 message_id（`pending.card_mid`）PATCH
改卡「✅ 已发送」（只留所发候选）并删 pending；横幅常用语快捷回复（send-text）
成功后同样改卡为「已快捷回复」（发出的是常用语而非草稿，候选正文全保留）。
改卡是 best-effort：card_mid 缺失（存量 pending/发卡响应缺字段）或 PATCH 失败
仅记日志，发送本身不受影响。

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
- `--note <text>`（表态门禁场景带上）：在全部候选块之后、共享按钮之前追加灰字
  「依据：…」状态行；空值整体省略。该行无 element_id、非按钮，完成态改卡时
  与引用块一样保留。
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
- 改卡 token 30 分钟/最多 2 次、须整卡替换；token 缺失或用尽时按事件自带的
  卡片 message_id `PATCH /open-apis/im/v1/messages/{id}` 兜底（无 token 限制），
  兜底也失败才记 stderr 放弃（发送本身不受影响）。
- 分支语义：send（按 idx 发对应候选；幂等键=mid 防连点双发，选定一条后其余候选
  同键去重、不会二发）/ ignore / copy（逐条回发全部候选，不改卡）/
  pending 缺失改卡「已失效」/ idx 越界（同 mid 重发覆盖 pending 后旧卡点了消失的
  候选）改卡「已失效」、pending 保留 / reply 失败保留 pending 改卡「发送失败」。

## 预约意向卡（send-book-card：点「预约」直接 room book）

与草稿卡同一条回调链路，独立 pending（SQLite `book_pending` 表，键同为源消息
mid——草稿卡与意向卡可对同一条消息并存、互不覆盖）。发卡命令与触发时机见
SKILL.md「预约意向卡」一节。

```
P0 会议意图消息 → 模型定时段/参会人 → lark-watch send-book-card（book_pending 入库 + bot 发卡）
  ├─ 点「预约 ①/②/③」→ 二进制：BookPendingClaim（原子取出并删除；room book 无幂等键，
  │  双订防护只能靠 claim——连点/重复事件第二次落空）→ exec room book -d -t --title -p --json
  │    ├─ 成功：改卡「✅ 已预约 <会议室>·<时段>」（只留所选时段块）+ stdout {"p":"booked",...}
  │    └─ 失败：改卡「❌ 预约失败：<message（hint）>」+ stdout {"p":"book-failed",...}；
  │       pending 不 re-put（按钮已随改卡移除），重试 = 模型按事件重发新卡
  └─ 点「忽略」→ 二进制：删 book_pending → 改卡「已忽略」
```

- 预订同步执行在 consumer 循环上（人手点击稀疏；不进 goroutine，关停时不会
  丢下进行到一半的预订）；单次预订超时 60s。已知取舍：room 挂死时其他卡片
  点击与 SIGTERM 关停最坏被拖 60s（二次 SIGTERM 无效），宁可等预订收尾也
  不留「订没订上」的不定态；等不及 SIGKILL 的话预订可能已生效，`room list
  --json` 核对。
- room CLI 从 PATH 找（`LW_ROOM_BIN` 可覆盖），固定 argv exec、绝不过 shell——
  参数来自本地 SQLite，不受消息内容注入。
- 成功信封解析失败（exit 0 但输出异常）按失败处理并提示「预订可能已生效，
  room list 核对」——绝不误报成功；真已订上时重订会被 conflict 挡住，不致双订。
- 改卡完成态与草稿卡共用 RenderDoneCard：token 版优先，缺失/用尽走事件
  message_id 的 PATCH 兜底。

## 排错

| 现象 | 排查 |
|---|---|
| 点按钮无反应 | `lark-watch status` 看 `consumer_state`；`lark-cli event status`；后台回调配置 |
| consumer 反复重启 | stderr 里 consume 退出原因；`card.action.trigger` 仅允许一个 consumer，查残留进程 |
| 改卡失败日志 | token 用尽会自动走 message_id PATCH 兜底；兜底也失败查 bot 对 `im/v1/messages` PATCH 的权限；发送本身不受影响 |
| 卡片显示「草稿已失效」 | pending 已被处理或清理，回终端确认状态（通知弹窗「发送」发出的现在会改卡「已发送」，不再走失效） |
| 点「预约」后改「预约失败」 | `grep card.book ~/.local/state/lark-watch/events.log \| jq .` 看 reason；auth/config 类按 `/room` skill 修（room login / booking.room_list） |
| 「预约」显示「已失效」 | book_pending 已被消费（双击第二下 / 同 mid 重发覆盖旧卡），以 `booked` 事件或 `room list --json` 为准 |
| 点「预约」后卡片无任何反应 | daemon 可能在预订中被 SIGKILL/崩溃带走（正常 SIGTERM 会等预订收尾）：`room list --json` 核对是否已订上，已订则手动告知对方，未订让模型重发意向卡 |
| 点按钮完全没动静（改卡也没有） | 点击者不是本人（卡片禁转发，但历史卡片无此限制）：events.log 里 grep `operator .* is not self` |
| 单元测试 | `cd {SKILL_DIR}/go && go test ./...`（card_test.go 覆盖回调分支+去重+多候选模板渲染+预约分支） |
