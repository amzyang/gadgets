# lark-persona — 飞书聊天记录 → 个人画像蒸馏（Claude Code skill）

把你自己的飞书聊天记录采集到本机，蒸馏出「你怎么说话」的个人画像：
按受众分层的说话风格（对上级/平级/群）、高频联系人关系卡、关系图谱。
产物喂给 [lark-watch](../lark-watch/)，让代拟的回复草稿像你本人写的。

**隐私**：归档与画像只存本机（`~/.local/share/lark-persona/`），不进 git、不外发；
蒸馏过程会把消息内容送入 Claude 处理，首次运行前会向你确认。
只蒸馏你本人的画像，不为同事建档。

## 前置条件

- macOS + [Claude Code](https://claude.com/claude-code)
- Node.js ≥ 18 + 飞书官方 CLI：`npm i -g @larksuite/cli`
- `jq`（`brew install jq`，关系图谱统计用）

## 安装

1. 配置飞书应用 + 用户授权（与 lark-watch 共用，做过一次即可）：

   ```sh
   lark-cli config init --new
   lark-cli auth login --domain im,contact
   ```

2. 安装 skill：

   ```sh
   npx skills add amzyang/gadgets
   ```

   选择 `lark-persona`。手动方式：clone 后
   `ln -s <clone目录>/lark-persona ~/.claude/skills/lark-persona`。

## 使用

```sh
claude /lark-persona
```

对 Claude 说「蒸馏我的个人风格」即可，它会依次：采集归档（首次约几百个会话，
耗时较长）→ 关系图谱 → 分层蒸馏画像。之后说「更新画像」做增量。

## 卡住了？

飞书上找邹洋，或把报错原文丢给 Claude Code。
