# 非文本内容获取对照表（content-fetch）

何时读我：SKILL.md「事件处理」第 2 步遇到图片/文档以外的非文本内容，或需要
权限失败处置、下载细节、声明措辞时。

## 总原则

- 占位符（`[File: xxx]`）、裸链接、卡片 JSON 都不是内容本身；获取与否按
  SKILL.md 第 2 步的必要性判据——细判/起草依赖才拉，但「无需回应」的结论
  必须能由纯文字部分独立得出，不得看着占位符猜内容再判 FYI。
- 与 `~/.config/lark-watch/context/` 上下文源卡片的边界：消息**自带**的资源
  （附件、正文里的链接）按本表硬性获取，不可配置；context 卡片是表态门禁/
  反敷衍的额外求证源（可选、用户可配）。第 2 步已读过的资源可直接充当表态
  门禁的依据，不重复拉取、不占门禁的 3 次检索预算。
- 识别靠事件 `type` 字段（聚合事件 `msgs[]` 每条都有自己的 type），不必猜
  渲染文本；`file_key`、卡片 JSON、分享对象 id 都从第 2 步消息列表
  （`--format json`）的 content 里取。
- 表中命令是最短路径快照；每类内容都有对应的 lark-* skill（完整参数、权限、
  排错与复杂读取——文档内嵌表格/画板、锚点局部读、大文件分片、高级权限表——
  都在 skill 里）。命令报错或参数拿不准时，进对应 skill 按其指引执行，
  不要对着本表猜 flag。

## 对照表

| 类型 | 识别特征 | 获取方式（skill + 命令） | 拿不到/无法消费时 |
|---|---|---|---|
| 图片 | `type=image`、post 含图块 | **lark-im**：SKILL.md 第 2 步内联命令（`--type image`） | 声明「图片未能查看」 |
| 文件附件 | `type=file`，content 含 file_key/file_name | **lark-im**：`+messages-resources-download --message-id <mid> --file-key <key> --type file`；先看 file_name 按下文分流 | 声明并注明文件名 |
| 云文档链接 | 正文含 `/docx/`、`/wiki/` URL 或 token | **lark-doc**：`docs +fetch --doc "<URL或token>"`（`--doc-format markdown`；URL 带 `#share-` 锚点自动局部读；长文只读与诉求相关部分） | 权限失败见下 |
| 电子表格链接 | `/sheets/` URL | **lark-sheets**：`sheets +workbook-info --url` 探结构 → `+csv-get --url --sheet-name --range` 读数据 | 同上 |
| 多维表格链接 | `/base/` URL | **lark-base**：`base +field-list --base-token --table-id` 取字段 → `+data-query`/`+record-list` 读数据 | 同上（高级权限表需 owner 授权） |
| 妙记链接 | `/minutes/` URL 或 token | **lark-minutes**：`minutes +detail --minute-tokens <t> --summary`（产物 flag 必须显式给，可加 `--todo`/`--transcript`；读现成产物是轻操作，允许进实时链路） | 2091005 无权限→声明，请 owner 开权限 |
| 个人名片 | `type=share_user`，content 含 ou_ | **lark-contact**：`contact +search-user --user-ids <ou_>` 补全姓名/部门 | 声明 |
| 群名片 | `type=share_chat`，content 含 oc_ | **lark-im**：`im chats get`（原生 API，先 `lark-cli schema im.chats.get` 看参数）——仅当自己在该群内才有详情 | 不在群内→声明「群详情不可见」 |
| 交互卡片 | `type=interactive` | **lark-im**：第 2 步列表的 content 就是完整卡片 JSON，直接抽其中文本阅读，零额外调用（事件 text 截 500 会截断 JSON，以列表为准） | — |
| 合并转发 | `type=merge_forward` | **lark-im**：容器 content 是合并渲染文本，通常够用；子消息里的图/文件用**容器 mid** 下载（子消息 id 会被 234003 拒绝） | 声明 |
| 语音 | `type=audio`（IM 语音，勿与 VC 事件混淆——VC 本就跳过细判草稿） | **lark-im** 只能下到 .opus、无转写；对语音发起妙记转写走 **lark-minutes**，属重操作、仅用户显式要求才做 | 声明「语音无法收听」，草稿请对方打字 |
| 视频 | `type=media` | **lark-im** 可下载，但模型无法观看 | 声明，只报文件名/时长 |
| 表情包 | `type=sticker` | 飞书不支持拉取（无对应获取手段） | 转述「表情包（内容不可见）」，一般判 FYI 无需回应 |

## 下载细节

- `cd $(mktemp -d)` 后再下载（`--output` 只接受相对路径）；图片 `--type image`，
  其余（文件/视频/语音）都是 `--type file`。
- 多附件一次全下：`+chat-messages-list --chat-id <cid> --download-resources`
  （落到 `./lark-im-resources/`）。
- 附件按 file_name 分流：pdf/图片/纯文本（md/txt/csv/log/json/代码）下载后
  Read 直接看；docx 可 `textutil -convert txt` 尝试一次（macOS 自带），失败
  即声明；zip/安装包/音视频不下载，直接声明。
- 数量上限：与诉求直接相关的至多 3 个资源，超出在转述里声明「其余附件/链接
  未逐一查看」——实时链路优先响应速度，不为大礼包附件拖死。

## 权限与失败

- 234002 / 14005（无权限或文件已删）：**勿重试**，声明未能查看；草稿可请
  对方开权限或直接贴文字版。
- 防泄密群：服务端屏蔽拉取，二进制已自动跳过（见 SKILL.md「alert / Monitor
  退出」`kind:"restricted"`），不会走到本表。

## 声明措辞

- 转述内联标注：「附件 报价单.zip（未查看）」「语音 32s（无法收听）」。
- 草稿禁写「我看了/看过/如你文档所说」这类未兑现的表述；需要内容时请对方
  发文字版或口头说明。
- 发卡带 `--note '附件未能查看'`（复用表态门禁的 note 通道，手机端可见）。
