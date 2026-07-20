---
name: lark-watch
description: >-
  用户视角实时监控飞书消息并生成回复草稿与洞察。当用户说"监控飞书消息"、
  "盯一下飞书"、"帮我看飞书"、"飞书自动回复"、"lark-watch"、"watch my
  Feishu/Lark messages"，或希望 Claude 代替自己关注飞书私聊/群聊并起草回复时使用。
  不负责：bot 视角事件订阅（lark-event）、主动发送与消息管理（lark-im）。
---

# lark-watch：飞书消息监控与回复草稿

Go 单二进制（`bin/lark-watch`）：一个进程同时承载**消息轮询**（chat-list 活跃降序 +
逐会话增量拉取，低频 messages-search 兜底对账，用户本人视角）与**卡片回调直发**
（lark-cli consume 子进程 + 自动重启监督）。二进制侧完成过滤/去重/分级，只有值得处理的消息才会成为 Monitor
事件；卡片按钮点击由二进制直接执行，零模型参与。状态存 SQLite
（`~/.local/state/lark-watch/lark-watch.db`），多进程并发安全。

**路径规则**：下文 `{SKILL_DIR}` 必须替换为本 skill 的绝对目录
（如 `~/.claude/skills/lark-watch`）。Monitor/Bash 的 cwd 是用户当前目录，
相对路径会 exit 127。二进制缺失或需重建：`cd {SKILL_DIR}/go && make install`；
涉及卡片/存储的改动装完需重启 Monitor（常驻进程仍是旧二进制）。

## 启动（零交互）

用户表达监控意图后立即启动，不要反问监控范围（默认全量三层分级）。
auth 自检已内置：二进制启动即校验（lark-cli 是否可用、user 身份、token 刷新期），
异常会立即输出 alert（见「alert / Monitor 退出」），按 msg 转告即可，无需预检：

1. 启动**唯一的** Monitor（轮询与卡片回调都在里面，禁止拆分）：

   ```
   Monitor({
     command: "{SKILL_DIR}/bin/lark-watch run",
     description: "飞书消息监控（lark-watch）",
     persistent: true
   })
   ```

   可选 flag：`--interval 45`（轮询秒数）、`--digest-window 600`、`--digest-max 20`。
   **不要传 `timeout_ms`**：超时到点会静默杀死监控进程（曾发生：1h 超时把整个
   监控带走且无人发现）。Monitor 参数就按模板三项，不增不减。
2. 挂兜底心跳（三个参数缺一不可，缺 `prompt` 会直接报错）：

   ```
   ScheduleWakeup({
     delaySeconds: 1800,
     prompt: "lark-watch 兜底心跳：跑 {SKILL_DIR}/bin/lark-watch status 健康检查，全部正常则 noop 并以相同 prompt 重挂 1800s；检查项与异常处理见 {SKILL_DIR}/SKILL.md「心跳唤醒」一节",
     reason: "lark-watch 兜底心跳"
   })
   ```

   Monitor 事件是主唤醒信号，心跳只做健康检查。`prompt` 用上述自含文本，
   不要写 `/lark-watch`（重进 skill 会误触发启动流程、重复起 Monitor）。

## 事件处理

stdout 每行一个 JSON 事件，`p` 字段区分类型。**判断权在模型**：二进制只做粗筛
（排除自己/机器人/噪音，p2p、@我 与音视频会议升 P0），值不值得回复、
是否打扰用户由你细判。

### P0（私聊 / 群里 @我 / 音视频会议 / watchlist / 关键词命中）

字段（按输出键序，正文靠前、ID 收尾）：`text`(正文，截 500 字) `from`(发送者)
`chat`(群名，p2p 为 null) `t`(时间) `ctype`(p2p/group) `type`(msg_type)
`mid`(message_id) `cid`(chat_id) `fid`(发送者 open_id) `ftype`(发送者类型)
`link`(applink，点击直达该消息)。

同一轮询周期内同会话的多条 P0 聚合为一个事件：`n`(条数)≥2 时含 `msgs[]`
（时间升序，每条 text/from/t/type/mid/fid），顶层字段取最后一条作代表。
细判与草稿针对**整组诉求**一次完成，`send-card --mid` 照常用顶层 mid
（回复落在最新一条下）。

`replied:true`：该消息之后你已在同会话发过言（大概率已亲自处理），系统通知
已自动抑制。默认安静跳过——不起草，转述至多一句带过；仅当正文明显仍需
单独回应时照常处理。

`type` 为 `video_chat`/`vc_meeting`（发起或分享视频/语音会议）时 `text` 常为空：
这类事件实时性最强（不聚合、不带 replied），跳过细判与草稿，立即转述
「谁在哪发起了会议」+ `link` 让用户点击加入。系统侧已弹出专用「忽略/加入」
弹窗（见 `notify-vc`），转述职责不变。

处理流程：

1. **细判**：是否需要回应？FYI/已读即可的消息只简要转述，不起草。
2. **上下文**（判定需要回应就拉，一次 10 条；纯闲聊/FYI/音视频会议跳过）：
   `lark-cli im +chat-messages-list --chat-id <cid> --page-size 10 --no-reactions --format json`
   线程消息用 `+threads-messages-list --thread <omt_>`。阅读时区分本人发言
   （sender id 为自己的 open_id），别把他人的话当成自己的承诺。
3. **分类**：咨询 / 闲聊 / 任务 / FYI。
4. **草稿**：起草前先注入个人画像（lark-persona 产物；文件存在才用，缺失静默跳过）：
   读 `~/.local/share/lark-persona/persona/contacts/<fid>.md`（对此人的称呼与语气分寸）
   和 `~/.local/share/lark-persona/persona/style.md` 中对应受众层（上级/平级/下属/群）
   ＋「反面清单」＋「草稿改善指引」（closure 时机的事实式认可、QA 报 bug 先接住再下
   结论）。然后按 /write skill 的规则起草（口语化贴合用户平时语气，纯正文无评注），
   出稿前跑 write skill 的标点门禁脚本校验。
   称谓精简：p2p 私聊双方指代明确，能省的「你/我」和对方称呼直接省——
   「收到，下午发过去」而不是「我收到了，我下午发给你」；只在指代易混
   （涉及第三人、多个事项归属时）或分寸需要（persona 记录的称呼习惯，
   如对上级开头带称呼）时保留。群聊不受此限，@ 与称呼照旧。
   反敷衍：能当场查清的问题，草稿不用「稍后处理/我看一下」类空话搪塞——答案可能
   在飞书云文档/wiki/妙记/表格里时，先用对应 lark-cli 只读命令查证，草稿直接给
   结论并带出处；确实查不清、或用户风格就是先应一声时才用缓冲话术（persona 优先）。
   高效沟通：对方只寒暄/问「在吗/忙吗」/说「有个问题」却不给内容时，草稿应一声并
   顺势引导直接说事（如「在的，直接说就行」）；对方描述了问题但缺关键信息、无法给
   结论时，草稿先回应已知部分，再点名要具体缺口（报错原文/截图、traceId、环境、
   复现步骤、单号——列确切要什么，不写「能不能多给点信息」这类空泛话，一次问全、
   最快闭环）。先后关系：能自查的先自查（反敷衍），只有对方才能补的才开口要；
   对上级/客户等分寸敏感对象只问缺口、不做沟通方式说教，语气按 persona。
   候选数量自适应（1–3 条）：事实型/查证型回复只出 1 条——查证后答案唯一，多条
   是噪音；语气敏感（对上级/客户、拒绝、催促、坏消息）或存在多种合理应对策略
   （先答应 vs 先问细节、接受 vs 婉拒）时出 2–3 条候选。候选间必须是不同的应对
   策略，不是同义改写；按推荐度排序，① 放最推荐的。
5. **展示**：原消息（含可点击 `link`）+ 分类 + 草稿 + 洞察。洞察写有信息量的内容：
   - 任务类：与用户当前会话/仓库工作的关联（同一项目？同一服务？），给出建议动作
     （如"这与你正在改的 X 有关，建议先回复预期时间"）；
   - 咨询类：如果答案在用户已有的代码/文档/近期工作里，直接把依据带出来；
   - 找人/协调类：指出对方真实意图与紧急程度判断依据。
   没有洞察就不硬写，只给分类+草稿。
6. **默认发确认卡片**：展示的同时立即用 `send-card` 把草稿（全部候选）发成确认
   卡片（见下「卡片确认」），用户在飞书/手机点任一候选的「发送」即确认；终端确认
   仍然可用，两端任一确认即发送。终端路径确认后执行：

   ```
   lark-cli im +messages-reply --message-id <mid> --as user \
     --idempotency-key <mid> --text $'<草稿>'
   ```

   富文本用 `--markdown`；需进线程用 `--reply-in-thread`。幂等键固定用源消息
   mid，天然防双发。终端发送前先看 `status` 的 pending 数——已在飞书端点过
   「发送」的不要重复发。

### 卡片确认（默认路径：起草后立即发卡，用户在飞书/手机上点按钮确认）

卡片回调链路随 `run` 常驻，起草只需一条命令（模板渲染/转义/pending 全部内置；
必填仅 `--mid`/`--draft`，其余为卡片展示字段、可省略——P0 事件里都有，建议带上）：

```
printf '%s' '<草稿>' | {SKILL_DIR}/bin/lark-watch send-card \
  --mid <mid> --draft - --original '<原消息文本>' --from '<发送者>' \
  --scene '<私聊|群名>' --t '<消息时间>'
```

多候选（2–3 条）时 `--draft` 重复给出，用 bash 进程替换免临时文件：

```
{SKILL_DIR}/bin/lark-watch send-card --mid <mid> \
  --draft <(printf '%s' '<候选①>') --draft <(printf '%s' '<候选②>') \
  --original '<原消息文本>' --from '<发送者>' --scene '<私聊|群名>' --t '<消息时间>'
```

草稿含代码块/列表/链接等 markdown 构造时加 `--format markdown`——确认后以
post 富文本回复（对方看到渲染后的代码块），卡片预览也按 markdown 渲染；
纯对话文本不加（markdown 会误解析 `*`、`_` 等字面字符）；format 应用于全部候选。

用户点「发送」（多候选时「发送 ①/②/③」任一）= 以该候选发出（单击即发，无二次
弹窗，幂等键防连点；发出后其余候选随卡片一并失效）；「复制草稿」= bot 把全部
候选逐条回发纯文本（长按可复制，手改后自己发）；「忽略」= 丢弃全部候选。点击后
的一切由二进制直接执行，零模型参与。细节与排错见
`{SKILL_DIR}/references/card-confirm-flow.md`。

### digest（群聊摘要，每 10 分钟或攒满 20 条）

字段：`n` 总条数，`chats[]` 按热度排序（`chat` 群名、`n` 条数、`peek` 最新一条
预览、`link` 直达会话）。一两句转述即可；只有出现值得注意的内容（与用户工作
相关的讨论、疑似找人）才建议展开某个群，展开命令同上 `+chat-messages-list`。

### backlog（启动时发现停机积压）

`{"p":"backlog","offline_secs":N}`：游标落后超 15 分钟已自动夹紧到当下（不会把
历史洪泛成实时 P0）。转告用户离线时长，建议说「补课」拉积压；不自动执行。

### alert / Monitor 退出

- `kind:"auth"`：user 身份不可用（未登录 / token 失效 / lark-cli 未安装），
  msg 已含行动指引，原样转告用户，完成后重启 Monitor。
- `kind:"auth-expiring"`：token 刷新期 < 24h，按 msg 转告提醒重新
  `lark-cli auth login`；Monitor 继续运行，无需重启。
- `kind:"api"`：连续调用失败（仍在退避重试），转告即可。
- `kind:"restricted"`：某群开启防泄密模式（禁止复制/转发），OpenAPI 无法读取
  该群消息（拉取与 search 均被服务端屏蔽，与 token/scope 无关）。二进制已自动
  跳过该群并每 24h 重探一次（`LW_RESTRICTED_REPROBE` 可调），告警仅发一次。
  转告用户：该群不在监控覆盖内，如需覆盖只能请群管理员关闭防泄密模式；
  被跳过的群列在 `status` 输出的 `restricted_chats` 字段。
- `kind:"card-daemon"`：卡片回调监听连续快速失败（自动重启中，仅卡片按钮降级，
  轮询不受影响），转告即可。
- Monitor 意外退出：看 stderr（Monitor 输出文件），可自动重启一次；再次失败
  则停下来交给用户，不要反复重启。

### 心跳唤醒（ScheduleWakeup 触发）

第一步永远是**重挂心跳**：以相同 prompt 再 ScheduleWakeup 1800s（三参数同首挂，
见「启动」第 2 步）。先挂再检查——noop 分支忘记重挂会让心跳链就此断掉（曾发生）。
然后跑 `{SKILL_DIR}/bin/lark-watch status` 健康检查：`heartbeat_age_secs` <
3×interval（默认 135）、`consumer_state == "alive"`、守护进程还活着
（`pgrep -f 'bin/lark-watch run'`；TaskList 列的是 to-do 任务、查不到 Monitor，
不要用它验活）。auth 状态已并入 status 输出（`auth_ok` /
`auth_refresh_expires_in_secs` / `auth_warning`）：`auth_warning` 非空时
原样转告用户，不要另跑 `lark-cli auth status`。

## 展示规范

- 转述消息时带上 `link`（`lark://` applink，点击直接唤起飞书客户端定位到
  消息/会话，不经浏览器跳转）；打开会话即客户端已读——飞书没有"标记已读"
  的 API，跳转就是等效操作。
- 群名/人名直接用事件里的 `chat`/`from`，不要再查 contact。

## 硬规则

- **不代发**：任何回复必须经用户确认（终端确认或卡片点击）。展示草稿 ≠ 授权发送。
- **禁止主动断开**：Monitor 只有用户明确要求才停
  （TaskStop + `ScheduleWakeup stop:true`）。
- **实时链路不重放历史**：首启 baseline 从当下开始，停机重启自动夹紧游标。
  历史积压只经「补课」显式命令拉取，不要在实时链路里主动搜旧消息。
- **误报治理**：某类消息反复被推送但用户不关心时，主动建议加噪音规则：
  `{SKILL_DIR}/bin/lark-watch ignore-add '<regex>'`（对 "cid 群名 人名 正文"
  拼接串匹配，可压掉 P0；先经正则校验，坏模式会被拒绝）。下一 tick 即生效。

## 补课（拉积压/未读历史，按会话分组 + 处理游标）

触发语：「补课」「看看错过了什么」「未读消息」「我不在的时候有什么」。

飞书没有未读 API——「未读」= **自该会话上次 mark 以来的消息**；首次（无游标）默认
回看 24 小时。mark 是「已处理」的唯一事实源，与实时监控的去重互不影响：
实时瞥过 ≠ 已处理。

1. 执行 `{SKILL_DIR}/bin/lark-watch catchup`（可加 `--since 3d` 临时扩窗，
   硬上限 7 天；`--peek N` 控制每会话预览条数，默认 5）。
2. 输出单行 JSON：`chats[]` 已按「含 P0 的会话优先、条数降序」排好。转述时：
   P0 会话逐条展开（走既有 P0 处理流程：细判→草稿→确认），普通群聊报
   「群名（n 条）+ peek 预览 + link」即可；`truncated:true` 时明确告知
   「仅覆盖最近约 2000 条」。
3. 用户处理完一个会话（回复了/明确说不用管）即
   `{SKILL_DIR}/bin/lark-watch mark <cid>`；说「都标掉」「全部已处理」→
   `mark --all`（作用于最近一次 catchup 的会话集合）。
4. 需要看某会话完整上下文时用 `im +chat-messages-list --chat-id <cid>`。

## 配置（~/.config/lark-watch/，每 tick 重读，改完即生效）

- `watchlist`：每行一个，`ou_` 开头=重点人、`oc_` 开头=重点群、其他行按群名/人名
  精确匹配，命中即升 P0。用户说"重点关注张三/某群"时：人名先经 lark-contact
  `+search-user` 解析成 ou_（重名时向用户确认），群名经 lark-im `+chat-search`
  解析成 oc_，再追加到该文件；解析失败才退回写名称行。`#` 开头为注释。
- `keywords`：每行一个正则（Go RE2），正文命中升 P0。默认为空；用户想要时建议从
  `加急|紧急|尽快|帮忙看|帮我看` 起步，提醒避免单字模式（如"急"会命中"不急"）。
- `ignore`：每行一个正则，命中直接丢弃。优先用 `ignore-add` 追加（带校验），
  手工编辑也可以。
- `notify`：P0 系统级通知命令（macOS 弹窗/横幅）。文件内容为一条 shell 命令，
  P0 到达时经 `sh -c` 异步执行；每 tick 的 P0 批次聚合为一次调用。消息经环境
  变量注入：`LW_TITLE` 标题（多条带条数）、`LW_MESSAGE`/`LW_SUMMARY` 每条一行
  的聚合摘要（`发送者（群名|私聊）: 正文`）、`LW_LINK` 首条 applink（点击跳转
  直达消息窗口）、`LW_COUNT` 条数、`LW_FROM`/`LW_CHAT`/`LW_TEXT`/`LW_TYPE`/
  `LW_CTYPE` 取首条。缺失/空文件 = 不通知。用户说"来消息弹窗提醒我"时直接写入
  该文件，默认给「忽略/复制/跳转」弹窗版（点「复制」把消息摘要置入剪贴板，
  点「跳转」open applink 唤起飞书定位到消息，60 秒无操作自动关闭）：

  ```sh
  osascript -e 'on run argv
  set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "复制", "跳转"} default button "跳转" giving up after 60
  set b to button returned of r
  if b is "复制" then set the clipboard to (item 1 of argv)
  if b is "跳转" and (item 3 of argv) is not "" then do shell script "open " & quoted form of (item 3 of argv)
  end run' "$LW_MESSAGE" "$LW_TITLE" "$LW_LINK"
  ```

  响铃已内置于二进制（通知前自动响：终端 bell 优先，无 tty 回退 osascript
  beep，SSH 会话静默），脚本里不必再加 bell。

  通知与草稿联动：需要起草回复的 P0（非音视频会议）不即时弹出，而是等草稿
  卡片发出（`send-card`）后再展示——通知到达时飞书里已有可点的确认卡片，
  模型无需额外操作。窗口内未发卡（判定 FYI 无需回复、起草超时）则在
  `LW_NOTIFY_GRACE`（环境变量，默认 180 秒）后照常弹出兜底；音视频会议仍
  即时通知（走专用「忽略/加入」弹窗或 `notify-vc` 脚本，不经 notify 脚本），
  `LW_NOTIFY_GRACE=0` 恢复全部即时。停机重启时过旧的待弹通知
  自动丢弃（不弹陈旧消息）。

  飞书客户端处于前台且用户活跃（输入空闲 < 2 分钟）时自动跳过响铃与通知
  （人已在看飞书，无需再弹；锁屏/走开或探测失败时照常通知），无需配置。

  要横幅不打断操作可换 `display notification (item 1 of argv) with title (item 2
  of argv)`（走通知中心，但横幅点击带不了跳转）。注意用 argv 传参，勿把 `$LW_*`
  拼进 AppleScript 源码（正文含引号会破坏脚本甚至被注入）。

  手动/脚本触发一条通知用子命令：
  `{SKILL_DIR}/bin/lark-watch notify --title <标题> --message <内容> --link <lark://…>`
  （优先走 notify 配置脚本；未配置时回退内置「忽略/复制/跳转」弹窗，无 `--link`
  则「复制/OK」按钮）。弹窗会阻塞到用户点击或 60 秒超时，模型调用时用
  `run_in_background`。
- `notify-vc`：音视频会议（`video_chat`/`vc_meeting`）专用通知命令，覆盖 VC
  批次的弹窗样式；`notify` 仍是通知总开关（notify 缺失时 notify-vc 不生效）。
  环境变量与 notify 相同，仅 `LW_TITLE` 为「📞 音视频会议」（多条带条数）。
  缺失时用内置「忽略/加入」弹窗：默认「加入」，点「加入」open 首条 applink
  直达会话中的会议消息，60 秒无操作自动关闭。响铃与前台抑制同样适用。
  自定义样式从内置等价模板起步：

  ```sh
  osascript -e 'on run argv
  set r to display dialog (item 1 of argv) with title (item 2 of argv) buttons {"忽略", "加入"} default button "加入" giving up after 60
  if button returned of r is "加入" then do shell script "open " & quoted form of (item 3 of argv)
  end run' "$LW_MESSAGE" "$LW_TITLE" "$LW_LINK"
  ```

## 状态与排错

- 状态库：`~/.local/state/lark-watch/lark-watch.db`（SQLite，`sqlite3` 可直接查；
  表：meta/seen/handled/processed/fetched/pending/digest_buf/catchup_last/restricted）。
  同目录 `*.imported` 是 bash 时代的留档，可忽略。
- 健康检查：`{SKILL_DIR}/bin/lark-watch status`。`restricted_chats` 非空表示
  这些群开启了防泄密模式、监控无法覆盖（见「alert / Monitor 退出」的
  `kind:"restricted"`）。
- 重置监控：TaskStop Monitor 后删 lark-watch.db 再重启（会重新 baseline）。
- 重建二进制：`cd {SKILL_DIR}/go && make install`（vet + test + build）。
- 单元测试：`cd {SKILL_DIR}/go && go test ./...`。
