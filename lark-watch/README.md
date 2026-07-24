# lark-watch — 飞书消息监控 + 回复草稿（Claude Code skill）

以你本人视角实时盯飞书：私聊、@你、音视频会议秒级升级提醒，群聊攒摘要；
消息里的云文档链接、图片、文件附件会被自动预取喂给 Claude——判断和转述
基于真实内容，不会看着一个裸链接就说「无需处理」。值得回的消息由 Claude
起草回复，你在终端确认或在飞书卡片上点「发送」才发出——
**永远不会替你自动回复**。配合 [lark-persona](../lark-persona/) 的个人画像，
草稿会贴合你平时的说话语气。

## 前置条件

- macOS + [Claude Code](https://claude.com/claude-code)
- Node.js ≥ 18（`brew install node`）
- Go + golangci-lint（`brew install go golangci-lint`，编译监控二进制用）
- alerter（`brew install vjeantet/tap/alerter`，内置通知横幅依赖；未装时 P0 只响铃不弹横幅）
- 飞书官方 CLI：`npm i -g @larksuite/cli`

## 安装

> 嫌步骤多？把这个 README 直接丢给 Claude Code，让它照着帮你装。

1. **配置飞书应用**（一次性）：

   ```sh
   lark-cli config init --new
   ```

   会引导你在浏览器里创建一个自建应用（只属于你自己，权限完全隔离）。

2. **用户身份授权**（Device Flow，浏览器完成）：

   ```sh
   lark-cli auth login --domain im,contact,docs
   ```

   `docs` 域供资源预取读取云文档；缺它监控照常，只是文档链接退回手动获取。

   之后运行中若提示缺权限，按报错提示补授权即可。

3. **安装 skill**：

   ```sh
   npx skills add amzyang/gadgets
   ```

   选择 `lark-watch`（建议连 `lark-persona` 一起装）。
   手动方式：`git clone https://github.com/amzyang/gadgets` 后
   `ln -s <clone目录>/lark-watch ~/.claude/skills/lark-watch`。

4. **编译监控二进制**：

   ```sh
   cd ~/.claude/skills/lark-watch/go && make install
   ```

## 使用

```sh
claude /lark-watch
```

Claude 会启动后台监控并常驻：P0 消息（私聊/@你/会议/重点人）实时转述 + 起草回复，
群聊每 10 分钟出摘要。离开一阵后说「补课」可以拉未读积压。

## 配置（可选，`~/.config/lark-watch/`，改完即生效）

| 文件 | 作用 |
| --- | --- |
| `watchlist` | 重点人/重点群，命中升 P0（每行一个 `ou_`/`oc_` 或名称）|
| `keywords` | 正文关键词正则，命中升 P0 |
| `ignore` | 噪音正则，命中直接丢弃 |
| `notify` | P0 通知配置（可选；缺省走 alerter 通知中心横幅，`off` 关闭，写脚本自定义），详见 SKILL.md |
| `notify-vc` | 音视频会议专用通知命令（可选；缺省走 alerter 带「加入」按钮的横幅）|
| `quick-replies` | 通知横幅的常用语快捷回复，每行一条，点选即回复对应消息 |
| `reactions` | 通知横幅的表情回应，每行一个飞书 emoji_type，点选给消息贴表情 |

状态存 `~/.local/state/lark-watch/lark-watch.db`（SQLite，只落本机）；预取的
文档/图片/附件产物在 `~/.local/state/lark-watch/prefetch/`（7 天自动清扫，
`LW_PREFETCH=0` 可整体关闭预取）。

## 已知限制

开启防泄密模式（禁止复制/转发）的群，飞书 OpenAPI 拒绝读取其消息
（错误码 231203，消息拉取与搜索均被屏蔽），监控无法覆盖。lark-watch
检测到后会自动跳过该群（每 24h 重探一次）并告警一次；被跳过的群可在
`bin/lark-watch status` 输出的 `restricted_chats` 字段查看。如需覆盖，
只能请群管理员关闭防泄密模式。

## 卡住了？

飞书上找邹洋，或把报错原文丢给 Claude Code。
