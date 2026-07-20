# lark-watch — 飞书消息监控 + 回复草稿（Claude Code skill）

以你本人视角实时盯飞书：私聊、@你、音视频会议秒级升级提醒，群聊攒摘要；
值得回的消息由 Claude 起草回复，你在终端确认或在飞书卡片上点「发送」才发出——
**永远不会替你自动回复**。配合 [lark-persona](../lark-persona/) 的个人画像，
草稿会贴合你平时的说话语气。

## 前置条件

- macOS + [Claude Code](https://claude.com/claude-code)
- Node.js ≥ 18（`brew install node`）
- Go（`brew install go`，编译监控二进制用）
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
   lark-cli auth login --domain im,contact
   ```

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
| `notify` | P0 系统弹窗命令（macOS），详见 SKILL.md |
| `notify-vc` | 音视频会议专用弹窗命令（可选；缺省内置「忽略/加入」弹窗）|

状态存 `~/.local/state/lark-watch/lark-watch.db`（SQLite，只落本机）。

## 已知限制

开启防泄密模式（禁止复制/转发）的群，飞书 OpenAPI 拒绝读取其消息
（错误码 231203，消息拉取与搜索均被屏蔽），监控无法覆盖。lark-watch
检测到后会自动跳过该群（每 24h 重探一次）并告警一次；被跳过的群可在
`bin/lark-watch status` 输出的 `restricted_chats` 字段查看。如需覆盖，
只能请群管理员关闭防泄密模式。

## 卡住了？

飞书上找邹洋，或把报错原文丢给 Claude Code。
