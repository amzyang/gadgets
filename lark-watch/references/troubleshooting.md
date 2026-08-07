# 状态与排错细节

SKILL.md「状态与排错」的完整展开：状态库结构与事件诊断日志字段说明。
`{SKILL_DIR}` 指本 skill 的绝对目录。

## 状态库（SQLite）

`~/.local/state/lark-watch/lark-watch.db`，`sqlite3` 可直接查。表：
meta/seen/handled/processed/fetched/pending/book_pending/notify_wait/
reminder/digest_buf/catchup_last/restricted/chat_state/avatar/res_cache。
同目录 `*.imported` 是 bash 时代的留档，可忽略。

## 事件诊断日志（events.log）

`~/.local/state/lark-watch/events.log`（NDJSON，默认开启，路径见 `status`
输出的 `event_log` 字段）。记录内容：

- 每条消息的判定：`msg.keep`/`msg.drop`（`reason`：p2p/at-me/keyword:…/
  ignore:…/self 等）
- tick 摘要、stdout 事件（`emit`，超长分片 `emit.chunked`）
- 通知链路：`notify.defer/flush/claim/replied/skip`，抑制与发送失败带 mids
  （`notify.suppress`/`notify.fail`）
- 延时提醒：`reminder.set/fire/drop`（安排/到期弹出/过期丢弃，带 mid 与 due）
- 发卡锚点：`card.sent`/`card.book_sent`（改卡完成态 `card.done` 为 debug 级）
- 预约执行（`card.book`）、卡片动作（`card.action`）、横幅动作回调
  （`popup.send/qreply/react`）
- 顶层子命令失败（`cmd.error`）与全部 stderr 诊断文本

排查示例：「这条消息为什么推了/没推」按 mid grep——
`grep om_xxx events.log | jq .`；审查失败面按 `jq 'select(.level=="ERROR")'`
过滤（notify.fail/cmd.error）。

超 10MB 轮转为 `events.log.1`（各留一代）。环境变量：`LW_EVENT_LOG=0` 关闭、
`LW_EVENT_LOG_LEVEL=debug` 加记重复拉取、安静 tick 与本人消息丢弃
（reason=self 默认不落盘）、`LW_EVENT_LOG_MAX_MB` 调上限。
