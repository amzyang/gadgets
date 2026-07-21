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

   可选 flag：`--interval 5`（轮询秒数）、`--digest-window 600`、`--digest-max 20`。
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

**超长事件分片（`p:"chunk"`）**：单行过长的事件会被拆成连续多行
`{"p":"chunk","seq":i,"of":k,"data":"<原 JSON 片段>"}`。收到 chunk 行时，把
`seq` 1 到 `k` 的全部 `data` 按序**原样字符串拼接**（不修剪、不解转义），得到
完整的单行事件 JSON，再按其 `p` 字段走对应处理流程。同一事件的分片必然相邻、
不与其他事件交错；若一批只见部分分片（罕见），等下一批凑齐再处理，禁止对
残缺片段强行解析或直接转述 `data` 原文。

### P0（私聊 / 群里 @我 / 音视频会议 / watchlist / 关键词命中）

字段（按输出键序，正文靠前、ID 收尾）：`text`(正文，截 500 字) `from`(发送者)
`chat`(群名，p2p 为 null) `t`(时间) `ctype`(p2p/group) `type`(msg_type)
`mid`(message_id) `cid`(chat_id) `fid`(发送者 open_id) `ftype`(发送者类型)
`link`(applink，点击直达该消息；以 markdown 链接展示，见「展示规范」)。

同一轮询周期内同会话的多条 P0 聚合为一个事件：`n`(条数)≥2 时含 `msgs[]`
（时间升序，每条 text/from/t/type/mid/fid），顶层字段取最后一条作代表。
细判与草稿针对**整组诉求**一次完成，`send-card --mid` 照常用顶层 mid
（回复落在最新一条下）。

`replied:true`：该消息之后你已在同会话发过言（大概率已亲自处理），系统通知
已自动抑制。默认安静跳过——不起草，转述至多一句带过；仅当正文明显仍需
单独回应时照常处理。

事件积压：事件是串行处理的，一次唤醒可能带多行积压事件，轮到某条时会话可能
已翻篇（`replied` 是事件产生时刻的快照，会过期）。同 cid 的多条 P0 只处理
最新一条，更早的视为被取代、并入转述一句带过；事件 `t` 明显落后当前时间
（正在消化积压）时，先按第 2 步核对会话最新状态再决定起草与催促。

`type` 为 `video_chat`/`vc_meeting`（发起或分享视频/语音会议）时 `text` 常为空：
这类事件实时性最强（不聚合、不带 replied），跳过细判与草稿，立即转述
「谁在哪发起了会议」+ `link`（markdown 链接，标签「加入会议」）让用户点击加入。
系统侧已弹出专用「忽略/加入」
横幅（见 `notify-vc`），转述职责不变。

**转述先行**：被 P0 事件唤醒后，先输出原文、再动手处理——在任何工具调用之前，
第一段就是原文转述，让用户最快看到「谁说了什么」。格式为引用块，每条消息一段：
`> **发送者**（群名｜私聊）HH:MM` 换行接 `text` 逐字原样（不改写、不概括、
不省略；`n`≥2 时 `msgs[]` 按时间升序逐条引用），引用块后跟
`[👉 直达消息](link)`。`text` 恰为 500 字说明已被二进制截断，末尾注明
「（可能被截断，前 500 字）」。例外：`replied:true` 与积压中被取代的旧 P0
不先行引用（维持至多一句带过）；音视频会议 `text` 常为空，按其专属流程
立即转述，不套引用块。

处理流程：

1. **细判**：是否需要回应？FYI/已读即可的消息只简要转述，不起草。
2. **上下文**（判定需要回应就拉，一次 10 条；纯闲聊/FYI/音视频会议跳过）：
   `lark-cli im +chat-messages-list --chat-id <cid> --page-size 10 --no-reactions --format json`
   线程消息用 `+threads-messages-list --thread <omt_>`。阅读时区分本人发言
   （sender id 为自己的 open_id），别把他人的话当成自己的承诺。
   若列表显示本人在该事件消息之后已有发言，视同 `replied:true`：安静跳过，
   不起草不发卡——用户已亲自处理，任何草稿和催促都是打扰。
   非文本内容先看后判：细判或草稿所依赖的信息在非文本载体里（图片、文件
   附件、文档/表格/妙记链接、名片、交互卡片、合并转发、语音视频——事件
   `type` 字段与正文链接模式可识别）时，必须实际获取查看后再细判/起草；
   `[File: xxx]` 占位符、裸链接、卡片 JSON 都不是内容本身，凭它们推断就是
   编造上下文。仅当细判/起草依赖该内容时才拉取（对方明说 FYI、`replied`
   跳过、纯寒暄不拉），但「无需回应」的结论必须能由纯文字部分独立得出——
   不得看着占位符猜内容再判 FYI。
   含图（`type` 为 `image`、`post` 带 image 块，或正文/相邻消息提到
   附图/截图/如图）：从上述消息列表的 content 里取 `image_key`，执行
   `cd $(mktemp -d) && lark-cli im +messages-resources-download --message-id <mid> --file-key <img_key> --type image --output img.png`
   （`--output` 只接受相对路径，故先进临时目录），再用 Read 查看图片。
   带飞书文档链接（docx/wiki）：`lark-cli docs +fetch --doc "<URL或token>"`，
   长文只读与诉求相关部分。文件附件（`type=file`）：同图片命令改
   `--type file`，先看 file_name 判断模型能否消费。其余类型（表格/Base/
   妙记/名片/合并转发）的获取命令与权限失败处置查
   `{SKILL_DIR}/references/content-fetch.md` 对照表；各类型都有对应的
   lark-* skill（lark-doc/lark-sheets/lark-base/lark-minutes/lark-im/
   lark-contact），内联命令拿不准或报错时进对应 skill 照其指引执行。
   拿不到或模型无法消费（语音/视频/表情包/无权限/防泄密）时，转述与草稿
   显式声明「未能查看 X」，发卡可加 `--note` 同步声明；草稿不得写
   「看过了」，需要内容时请对方发文字版。
3. **分类**：咨询 / 闲聊 / 任务 / FYI。对方在约会议/约时间（"找个时间对齐"
   "明天过下方案"）而会话尚未敲定日程的，除正常草稿外另走「预约意向卡」
   （见下节，与草稿卡并行、互不替代）。
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
   表态门禁：草稿可以代笔语气，不能代笔立场。对方在请求实质表态（技术方案
   取舍、设计决策确认、同意/拒绝承诺、评审意见、排期拍板）时，草稿里的同意、
   拒绝、承诺、方案评价必须有本次求证中可指出的依据（本会话前文、归档、文档、
   代码之一），说不出依据的表态不得出现在任何候选里。该约束挂在草稿输出上——
   即使细判没识别出决策类消息，出稿前也要过一遍「表态有据吗」；且优先级高于
   persona 风格卡的「轻确认收口」（风格决定怎么说，不决定持什么立场）。日常
   轻确认（收到/好的/时间安排）不触发。触发后先求证（全部只读，至多 3 次检索
   动作，找不到即止）：内置保底是拉长本会话历史——`--page-size` 提到 50 或
   线程全量，设计讨论的前文（字段用途、schema 由来）常在同一会话；更多源读
   `~/.config/lark-watch/context/*.md` 上下文源卡片（格式见「配置」；目录缺失
   或为空静默跳过），按各卡 when 匹配当前话题挑至多 3 张、fast 优先，照卡内
   指引执行。按求证结果出稿：有据支持——可同意，草稿带一句依据
   （「可以，未命中直接下一条的话 dwell 确实用不上」）；有据存疑——候选为
   保留意见＋追问，不出直接同意；无据——候选只能是复述确认式追问（把对方
   理由复述回去请其确认关键前提）或诚实缓冲（「我看下这块再回你」），用户
   想直接同意可点「复制草稿」手改。
   反敷衍：能当场查清的问题，草稿不用「稍后处理/我看一下」类空话搪塞——答案可能
   在某张 context 卡片声明的源里（飞书文档/聊天归档/本地代码等）时，选卡照指引
   查证，草稿直接给结论并带出处；确实查不清、或用户风格就是先应一声时才用缓冲
   话术（persona 优先）。
   反敷衍针对可查证的事实问题；需要用户本人拍板的技术表态走上面的表态门禁，
   诚实缓冲不算敷衍。
   高效沟通：对方只寒暄/问「在吗/忙吗」/说「有个问题」却不给内容时，草稿应一声并
   顺势引导直接说事（如「在的，直接说就行」）；对方描述了问题但缺关键信息、无法给
   结论时，草稿先回应已知部分，再点名要具体缺口（报错原文/截图、traceId、环境、
   复现步骤、单号——列确切要什么，不写「能不能多给点信息」这类空泛话，一次问全、
   最快闭环）。先后关系：能自查的先自查（反敷衍），只有对方才能补的才开口要；
   对上级/客户等分寸敏感对象只问缺口、不做沟通方式说教，语气按 persona。
   候选数量自适应（1–3 条）：事实型/查证型回复只出 1 条——查证后答案唯一，多条
   是噪音；语气敏感（对上级/客户、拒绝、催促、坏消息）、对方提出技术方案/设计
   决策，或存在多种合理应对策略（先答应 vs 先问细节、接受 vs 婉拒）时出 2–3 条
   候选。候选间必须是不同的应对
   策略，不是同义改写；按推荐度排序，① 放最推荐的。
5. **展示**：分类 + 草稿 + 洞察（原消息已由「转述先行」段承担，不重复贴），
   `link` 写成 markdown 链接（见「展示规范」）。洞察写有信息量的内容：
   - 任务类：与用户当前会话/仓库工作的关联（同一项目？同一服务？），给出建议动作
     （如"这与你正在改的 X 有关，建议先回复预期时间"）；
   - 咨询类：如果答案在用户已有的代码/文档/近期工作里，直接把依据带出来
     （可借助 context 卡片定位）；
   - 找人/协调类：指出对方真实意图与紧急程度判断依据；
   - 表态门禁场景：写明求证结论——「已核对本会话前文/归档/代码 ✓，依据是…」
     或「未能验证对方建议合理性，表态留给你」。
   没有洞察就不硬写，只给分类+草稿。
   「建议尽快回复」类时间敏感催促，仅当事件未标 replied **且**本轮已核对过
   会话最新状态（第 2 步拉过消息列表）时才可写；没核对就只转述不催——积压时
   事件快照常已过期，催用户回一条他早已回过的消息只会消耗信任。
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

表态门禁场景必带 `--note`——卡片在候选下方以灰字展示判断依据状态，让用户在
手机上也能看到草稿的求证结论：有据时如 `--note '已核对本会话前文 ✓'`，
无据时如 `--note '未验证对方建议，表态请自行判断'`；非门禁场景可省略。

用户点「发送」（多候选时「发送 ①/②/③」任一）= 以该候选发出（单击即发，无二次
弹窗，幂等键防连点；发出后其余候选随卡片一并失效）；「复制草稿」= bot 把全部
候选逐条回发纯文本（长按可复制，手改后自己发）；「忽略」= 丢弃全部候选。点击后
的一切由二进制直接执行，零模型参与。细节与排错见
`{SKILL_DIR}/references/card-confirm-flow.md`。

### 预约意向卡（会议/日程意图 → 一键订会议室）

细判发现对方在约会议/约时间、而会话尚未敲定日程（无已约定的明确时间、无日程
卡片）时，除正常草稿卡外再发一张预约意向卡：用户在飞书点「预约」，回调链路
直接执行 `room book` 真实预订会议室（零模型参与），结果以 `booked`/
`book-failed` 事件回到 stdout（见下）。日程已敲定、对方只是转发会议信息、或
`replied` 场景不发。

1. **定时段**（1–3 个候选）：消息里已有明确时间直接用；模糊表达（"明天下午"）
   进 `/lark-calendar` 用 `+freebusy`/`+suggestion` 查空闲后落成具体时间块；
   完全没提时间（"改天聊聊"）不发卡，草稿里先问时间。
2. **前置自检**（本地/只读，不过关就不发卡、按 `/room` skill 引导修复）：
   `room whoami --json` exit 3 → 引导 `room login`；`room config get
   booking.room_list --json` 为空 → 引导配置会议室列表。
3. **参会人**：p2p 用 lark-contact `+search-user` 反查对方 `enterprise_email`
   （room 拒绝 `ou_` 开头的 open_id）；群聊仅在明显是全组会议时用 `-p <cid>`
   （`oc_` 直接支持），否则只加发起人；room login 后本人自动加入，无需 `-p` 本人。
4. **发卡**：

   ```
   {SKILL_DIR}/bin/lark-watch send-book-card --mid <mid> \
     --slot '07-22 14:00-15:00' --slot '07-22 16:00-17:00' \
     --title '<会议标题>' -p <enterprise_email|oc_xxx> \
     --original '<原消息文本>' --from '<发送者>' --scene '<私聊|群名>' --t '<消息时间>'
   ```

   `--slot` 格式 `MM-DD HH:MM-HH:MM`，可重复至多 3 条（点哪个订哪个）；
   `--title` 必填。预订参数在发卡时固化、点击后模型不再参与——时段与参会人
   发卡前就要算对；同 mid 重发即覆盖旧卡参数。卡片按钮「我要预约」（多时段
   「预约 ①/②/③」）＋「忽略」。细节与排错见
   `{SKILL_DIR}/references/card-confirm-flow.md`「预约意向卡」一节。

### digest（群聊摘要，每 10 分钟或攒满 20 条）

字段：`n` 总条数，`chats[]` 按热度排序（`chat` 群名、`n` 条数、`peek` 最新一条
预览）。会话链接自拼：`lark://applink.feishu.cn/client/chat/open?openChatId=<cid>`
（以 markdown 链接展示，一行一会话）。一两句转述即可；
只有出现值得注意的内容（与用户工作
相关的讨论、疑似找人）才建议展开某个群，展开命令同上 `+chat-messages-list`。

**peek 是非文本占位符时先看后说**：`[图片]`/`[文件:名]`/`[卡片:标题]`/
`[合并转发]`（旧二进制原文形如 `![Image](…`、`[Image: …`、`<file…`、`<card…`）
= 没有文字线索，禁止据此写「日常内容/闲聊/无需处理」这类定性——转述前必须
先展开实际查看：`+chat-messages-list --chat-id <cid>` 定位该消息拿
mid/msg_type/key，图片按 P0 图片命令下载后 Read，文档链接 `docs +fetch`，
文件先看 file_name 判断能否消费，其余类型查
`{SKILL_DIR}/references/content-fetch.md`。看完再定性：确属闲聊/表情包一句
带过即可，与用户工作相关照常提醒；拿不到或无法消费（防泄密/无权限/语音
视频）时照实转述「X 发了图片/文件（未能查看）」，不定性。

### backlog（心跳缺口积压：停机重启 / 休眠唤醒 / API 长故障恢复）

`{"p":"backlog","offline_secs":N}`：心跳缺口超 15 分钟时游标已自动夹紧到当下
（不会把历史洪泛成实时 P0），恢复拉取后一次性通报，`offline_secs` 覆盖全程
缺口。转告用户离线时长，建议说「补课」拉积压；不自动执行。

### booked / book-failed（预约意向卡结果）

- `{"p":"booked",...}`：用户点了「预约」且 room book 成功，含 `title/room/date/
  start/end`、`mid`（源消息，回复锚点）与 `event_id`（日历日程，room cancel 用）。
  转述预订结果，并起草一句告知对方（时间＋会议室，如「订好了，明天 14 点
  A栋3F-301」）经 `send-card --mid <mid>` 确认后发出；对方已被加为参会人时
  还会收到日程邀请，这句只是对话闭环，别写成正式通知。
- `{"p":"book-failed",...}`：预订失败，卡片已改「预约失败」、无需安抚操作。
  按 `reason` 处置：`no_room`/`conflict` → 换时段（可再查 `+freebusy`）重发
  意向卡；`auth`/`config` → 按 `hint` 引导用户修 room CLI（`/room` skill）；
  其余转述 `msg` 即可。失败不自动重试，重发新卡即重试。

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
3×interval（默认 15）、`consumer_state == "alive"`、守护进程还活着
（`pgrep -f 'bin/lark-watch run'`；TaskList 列的是 to-do 任务、查不到 Monitor，
不要用它验活）。auth 状态已并入 status 输出（`auth_ok` /
`auth_refresh_expires_in_secs` / `auth_warning`）：`auth_warning` 非空时
原样转告用户，不要另跑 `lark-cli auth status`。

## 展示规范

- P0 原文以引用块逐字展示（格式见「转述先行」）：`> **发送者**（群名｜私聊）
  HH:MM` 换行接正文原样，多条按时间升序。这是原文的唯一出处，分类/草稿/洞察
  不再重复贴原文。
- 转述消息时带上 `link`（`lark://` applink，点击直接唤起飞书客户端定位到
  消息/会话，不经浏览器跳转）；打开会话即客户端已读——飞书没有"标记已读"
  的 API，跳转就是等效操作。
- `link` 以 markdown 链接直接写进转述正文：`[👉 打开「群名/人名」会话](<link>)`。
  标签按场景写：会话级 applink「打开会话」、消息级「直达消息」、音视频会议
  「加入会议」。多条链接（补课/digest 按会话一行）各自一行。不要输出裸
  `lark://` 文本，也不要另起 Bash 调用输出转义序列。
- 群名/人名直接用事件里的 `chat`/`from`，不要再查 contact。

## 硬规则

- **不代发**：任何回复必须经用户确认（终端确认、卡片点击，或通知横幅上
  点「发送/常用语/表情」——横幅点选即用户显式确认）。展示草稿 ≠ 授权发送。
  预约会议室同理：`room book` 是真实预订，只经意向卡「预约」点击触发，
  模型不得自行执行——除非用户在终端明确让订。
- **禁止主动断开**：Monitor 只有用户明确要求才停
  （TaskStop + `ScheduleWakeup stop:true`）。
- **实时链路不重放历史**：首启 baseline 从当下开始，停机重启自动夹紧游标。
  历史积压只经「补课」显式命令拉取，不要在实时链路里主动搜旧消息。
- **非文本内容先看后判**：细判/起草所依赖的信息在图片、附件、链接文档、
  卡片等非文本载体里时，必须实际获取查看后才能分类/起草（图片/文档/附件
  命令见「事件处理」的「上下文」步骤，其余见
  `{SKILL_DIR}/references/content-fetch.md`）。占位符与裸链接不是内容，
  禁止凭周边文字推断（曾发生：把「咨询详情」截图误判为 Grafana 看板）；
  可获取的必须获取，不可获取的显式声明「未能查看」。约束跟随一切内容
  定性动作——细判/起草之外，digest/补课转述里写「日常内容/闲聊/无需处理」
  也算「判」，同样先看后说（曾发生：digest 把未查看的群聊图片转述成
  「日常内容不用展开」）；看不了就照实转述＋声明未查看，不定性。
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
   「群名（n 条）+ peek 预览 + link（markdown 链接，一行一会话）」即可；
   `truncated:true` 时明确告知
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
- `context/`：上下文源卡片目录（模型侧配置，二进制不读；改完下一次起草生效）。
  每张卡声明一个起草时可用的只读信息源，frontmatter 必填 `provides`（能给
  什么）、`when`（何时找我）、`cost`（fast/slow，slow 排后执行），body 为只读
  命令指引（模型按实际话题改写查询词）。消费场景：表态门禁求证、反敷衍查证、
  洞察关联。目录缺失/为空回退内置保底（拉长本会话历史）。模板与格式说明见
  `{SKILL_DIR}/references/context-providers/`。用户说「加一个求证源/让草稿能
  查 X」时，直接替用户写一张卡进该目录。
- `notify`：P0 系统级通知（macOS 通知中心横幅）。**零配置默认开启**：文件
  缺失时直接用内嵌于二进制的横幅模板，60 秒无操作自动关闭（超时即忽略）。
  内置通知硬依赖 [alerter](https://github.com/vjeantet/alerter) ≥26
  （`brew install vjeantet/tap/alerter`；≥26 为双横线旗标语法，旧版不兼容；
  每次弹横幅现探测 PATH，装完即生效）：未装时只响铃并在 stderr 记安装指引
  日志（`notify` 子命令直接报错），无弹窗兜底。横幅不抢焦点，工作零打断：
  草稿联动通知（send-card 后弹出）正文展示对方消息摘要＋候选①全文（看清要
  发什么再按），动作下拉 =「发送」＋常用语＋表情回应（一键回复/回应，见
  `quick-replies`/`reactions` 配置）——「发送」直接以候选①回复对方（等价
  卡片「发送 ①」，幂等防双发，全程无需切回飞书）、**点横幅正文 = 复制并
  跳转**（候选①置入剪贴板并进飞书，想手改后发走这条）、关闭按钮「忽略」；
  即时/兜底通知（无草稿）动作下拉 =「复制」＋常用语＋表情、点正文 = 跳转
  （「复制」取消息摘要，快捷动作落在批次最后一条消息上）；VC 通知「加入」
  或点正文即入会；均 60 秒超时。横幅左侧图标显示飞书头像（私聊 = 对方头像、
  群聊 = 群头像，取批次首条；URL 经 SQLite 缓存 7 天，拉取失败静默回退默认
  图标，不阻断通知）。横幅以 alerter 默认 sender「终端」
  （com.apple.Terminal）名义投递；要有常驻按钮需在系统设置 → 通知里
  把「终端」的样式设为「提醒」（横幅样式几秒即逝）。用户说"来消息
  弹窗提醒我"时确认装有 alerter 即可（默认已开启）；说"别弹了/关掉通知"时
  写入关闭哨兵（空文件同义）：

  ```sh
  mkdir -p ~/.config/lark-watch && echo off > ~/.config/lark-watch/notify
  ```

  文件写其他内容 = 自定义通知脚本：P0 到达时经 `sh -c` 异步执行，每 tick 的
  P0 批次聚合为一次调用，消息经环境变量注入：`LW_TITLE` 标题（多条带条数）、
  `LW_MESSAGE`/`LW_SUMMARY` 每条一行的聚合摘要（`发送者（群名|私聊）: 正文`）、
  `LW_LINK` 首条 applink（点击跳转直达消息窗口）、`LW_COUNT` 条数、`LW_FROM`/
  `LW_CHAT`/`LW_TEXT`/`LW_TYPE`/`LW_CTYPE` 取首条、`LW_ICON` 头像 URL
  （私聊对方/群头像，取首条，可能为空）、`LW_DRAFT` 候选话术①与
  `LW_MID` pending 键（仅草稿联动通知有值；自定义脚本可用
  `{SKILL_DIR}/bin/lark-watch send-draft --mid "$LW_MID"` 实现自己的发送按钮）。
  osascript 自定义脚本用 argv 传参，勿把 `$LW_*` 拼进源码——正文含引号会
  破坏脚本甚至被注入。

  响铃已内置于二进制（通知前自动响：终端 bell 优先，无 tty 回退 osascript
  beep，SSH 会话静默），脚本里不必再加 bell。

  通知与草稿联动：需要起草回复的 P0（非音视频会议）不即时弹出，而是等草稿
  卡片发出（`send-card`）后再展示——通知到达时飞书里已有可点的确认卡片，
  模型无需额外操作。联动通知正文带候选①全文：默认横幅「发送」一键回复、
  点正文复制话术并进飞书，关闭/超时即忽略（发出后 pending 失效，卡片按钮
  再点显示「已失效」，属预期）。
  窗口内未发卡（判定 FYI 无需回复、起草超时）则在
  `LW_NOTIFY_GRACE`（环境变量，默认 180 秒）后照常弹出兜底；音视频会议仍
  即时通知（走专用「忽略/加入」横幅或 `notify-vc` 脚本，不经 notify 脚本），
  `LW_NOTIFY_GRACE=0` 恢复全部即时。停机重启时过旧的待弹通知
  自动丢弃（不弹陈旧消息）。

  飞书客户端处于前台且用户活跃（输入空闲 < 2 分钟）时自动跳过响铃与通知
  （人已在看飞书，无需再弹；锁屏/走开或探测失败时照常通知），无需配置。

  手动/脚本触发一条通知用子命令：
  `{SKILL_DIR}/bin/lark-watch notify --title <标题> --message <内容> --link <lark://…>`
  （优先走 notify 配置脚本；未配置时回退内置横幅——动作「复制」、点正文
  跳转 `--link`，未装 alerter 时直接报错）。横幅会阻塞到用户交互或 60 秒
  超时，模型调用时用 `run_in_background`。
- `notify-vc`：音视频会议（`video_chat`/`vc_meeting`）专用通知命令，覆盖 VC
  批次的横幅样式；`notify` 仍是通知总开关（notify 为 off 时 notify-vc 不生效）。
  环境变量与 notify 相同，仅 `LW_TITLE` 为「📞 音视频会议」（多条带条数）。
  缺失时用内置横幅（内嵌于二进制，默认无需配置；硬依赖 alerter，同 notify）：
  「加入」或点正文 open 首条 applink 直达会话中的会议消息，60 秒无操作
  自动关闭。响铃与前台抑制同样适用。自定义样式才需要该文件，写自己的脚本
  （argv 传参防注入，同 notify）。
- `quick-replies`：通知横幅的常用语快捷回复，每行一条（`#` 开头的整行为
  注释，行内 `#` 属内容）。缺失时内置默认「收到」「好的，稍后回复」。横幅下拉点选即以该文本回复对应消息（独立幂等键防连点双发，也不
  吞掉随后的正式回复；草稿场景发出后候选随之失效）。每次弹横幅现读。
  下拉标签中 ASCII 逗号显示为中文逗号（alerter 动作列表按逗号切分）、超长
  截断，与「发送/复制/忽略」重名的条目被剔除，`@` 开头的条目也被剔除
  （撞 alerter 哨兵输出 `@CLOSED`/`@TIMEOUT`，会把关闭/超时误判成点选）。
- `reactions`：通知横幅的表情回应，每行一个飞书 emoji_type（大写下划线，
  如 THUMBSUP/OK/DONE/APPLAUSE/HEART/THANKS），默认 THUMBSUP（👍），至多
  取 4 个。点选给对应消息加表情回应，不影响草稿候选（点赞 ≠ 已回复）。
  横幅动作总数上限 9（首键＋常用语＋表情），超出截断。

## 状态与排错

- 状态库：`~/.local/state/lark-watch/lark-watch.db`（SQLite，`sqlite3` 可直接查；
  表：meta/seen/handled/processed/fetched/pending/book_pending/notify_wait/
  digest_buf/catchup_last/restricted/chat_state/avatar）。同目录 `*.imported`
  是 bash 时代的留档，可忽略。
- 事件诊断日志：`~/.local/state/lark-watch/events.log`（NDJSON，默认开启，路径
  见 `status` 输出的 `event_log` 字段）。每条消息的判定（`msg.keep`/`msg.drop`
  的 `reason`：p2p/at-me/keyword:…/ignore:…/self 等）、tick 摘要、stdout 事件
  （`emit`，超长分片 `emit.chunked`）、通知链路（`notify.defer/flush/claim/replied/skip`，抑制与发送
  失败带 mids：`notify.suppress`/`notify.fail`）、发卡锚点（`card.sent`/
  `card.book_sent`，改卡完成态 `card.done` 为 debug 级）、预约执行
  （`card.book`）、卡片动作
  （`card.action`）、横幅动作回调（`popup.send/qreply/react`）、顶层子命令
  失败（`cmd.error`）与全部 stderr 诊断文本都在里面。排查「这条消息为什么推了/
  没推」按 mid grep：`grep om_xxx events.log | jq .`；审查失败面按
  `jq 'select(.level=="ERROR")'` 过滤（notify.fail/cmd.error）。超 10MB 轮转为
  `events.log.1`（各留一代）。`LW_EVENT_LOG=0` 关闭、`LW_EVENT_LOG_LEVEL=debug`
  加记重复拉取、安静 tick 与本人消息丢弃（reason=self 默认不落盘）、
  `LW_EVENT_LOG_MAX_MB` 调上限。
- 健康检查：`{SKILL_DIR}/bin/lark-watch status`。`restricted_chats` 非空表示
  这些群开启了防泄密模式、监控无法覆盖（见「alert / Monitor 退出」的
  `kind:"restricted"`）。
- 重置监控：TaskStop Monitor 后删 lark-watch.db 再重启（会重新 baseline）。
- 重建二进制：`cd {SKILL_DIR}/go && make install`（lint + vet + test + build，
  依赖 golangci-lint）。
- 单元测试：`cd {SKILL_DIR}/go && go test ./...`。
