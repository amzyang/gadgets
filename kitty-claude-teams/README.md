# kitty-claude-teams

让 **Claude Code 的 Agent Teams（团队/teammate 分屏模式）跑在 [kitty](https://sw.kovidgoyal.net/kitty/) 终端里**。

## 背景：为什么需要它

Claude Code 的实验特性 **Agent Teams** 用 `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1` 打开。
分屏模式下，每个 teammate 占一个独立 pane，但官方**只支持 `tmux` 和 iTerm2** ——
Ghostty、kitty、Windows Terminal、VS Code 内置终端都不支持。实现方式是 Claude 直接
shell-out 调用真正的 `tmux`（`split-window` / `send-keys` / `select-pane` / `kill-pane` /
`capture-pane` / `display-message`）来摆放 pane。

[cmux](https://github.com/manaflow-ai/cmux) 给 Ghostty 用的「hack」是：**伪造一个 tmux**——
在 PATH 前面塞一个名叫 `tmux` 的 shim、并导出 `$TMUX` / `$TMUX_PANE`，让 Claude 以为自己
在 tmux 里；shim 再把每条 tmux 命令翻译成自己窗口管理器的调用。

这个 gadget 把同样的把戏搬到 **kitty**：shim 把 tmux 命令翻译成 kitty 远程控制
（`kitten @`）。于是 teammate 会作为**原生 kitty 分屏**弹出，而不是 tmux pane。

## 命令翻译表

| Claude 发出的 tmux | 翻译成的 kitty 远程控制 |
| --- | --- |
| `split-window -h` / `-v` | `kitten @ launch --location=vsplit` / `hsplit` |
| `send-keys -t %N … Enter` | `kitten @ send-text --match id:<wid>`（按键名转真实控制字节） |
| `select-pane -t %N` | `kitten @ focus-window --match id:<wid>` |
| `kill-pane -t %N` | `kitten @ close-window --match id:<wid>` |
| `capture-pane -p -t %N` | `kitten @ get-text --match id:<wid>`（仅 `-p` 时才输出） |
| `display-message -p -F …` | 本地展开 `#{pane_id}` 等占位符 |
| `tmux -V` | 返回伪造版本号，骗过版本检查 |
| 其它（`set-option`/`has-session`…） | 静默成功（exit 0），与 cmux 的策略一致 |

合成的 tmux pane id（`%1`、`%2`…）与 kitty window id 的映射保存在
`$XDG_RUNTIME_DIR/kitty-claude-teams/<session>/map`，退出时自动清理。

## 准备工作

1. **kitty 打开远程控制**（`~/.config/kitty/kitty.conf`）：

   ```conf
   allow_remote_control yes
   listen_on unix:/tmp/mykitty
   ```

   或临时启动：`kitty -o allow_remote_control=yes -o listen_on=unix:/tmp/mykitty`。

2. 让分屏真正平铺，建议给当前 tab 用 `splits` 布局（`enabled_layouts splits,*`）；
   `--location=vsplit/hsplit` 也会强制分屏。

3. `claude` 已安装并在 PATH 上。

## 用法

在一个 kitty 窗口里：

```bash
/path/to/kitty-claude-teams [--layout split|tab|os-window] [--tabs] [额外的 claude 参数...]
```

它会：

1. 预检 kitty 远程控制是否可达（`kitten @ ls`）；
2. 建一个临时 `bin/` 目录，把 `tmux` 软链到 `tmux-shim.sh` 并前置到 PATH；
3. 导出伪造的 `TMUX` / `TMUX_PANE=%0`、`CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1`；
4. `exec claude --teammate-mode auto …`（未自带 `--teammate-mode` 时才加默认值）。

放进 PATH 方便调用：

```bash
ln -s "$(pwd)/kitty-claude-teams/kitty-claude-teams" ~/.local/bin/kitty-claude-teams
```

### 环境变量

| 变量 | 作用 |
| --- | --- |
| `KCT_LAYOUT` | teammate 落在哪里：`split`（默认，分屏）/ `tab`（新 tab）/ `os-window`（新 OS 窗口）。等价于 `--layout`/`--tabs` |
| `KCT_KITTY_TO` | 要控制的 kitty socket，如 `unix:/tmp/mykitty`。默认取 kitty 自动导出的 `$KITTY_LISTEN_ON` |
| `KCT_DEBUG=1` | 把每条 shim 翻译打到 stderr，便于调试 |

### teammate 放在 tab 还是分屏

默认 `split-window` 翻译成 kitty 同 tab 内的分屏，会改变当前 tab 的布局——这正是
upstream [anthropics/claude-code#23615](https://github.com/anthropics/claude-code/issues/23615)
抱怨的点（spawn 进新 window 而不是切当前 pane）。用 `--tabs` / `KCT_LAYOUT=tab` 让每个
teammate 开在**独立 tab**，互不挤占布局；`os-window` 则每个开一个新 OS 窗口。

```bash
kitty-claude-teams --tabs          # teammate 各占一个 tab
KCT_LAYOUT=os-window kitty-claude-teams
```

该设置会随 env 注入到 teammate 窗口，嵌套 spawn 的子 teammate 也沿用同一布局。
`new-window` 命令始终开新 tab，不受影响。

## 局限 / 说明

- 这是**子集 shim**，按 cmux 的翻译范围只处理 Agent Teams 实际用到的 tmux 动词，
  其余命令一律静默成功。若 Claude 的 teams 模式用到未覆盖的命令，开 `KCT_DEBUG=1`
  能看到 `unhandled:` 日志，再按需补。
- 翻译逻辑已用「假 kitten」做过离线单测（命令拼装、`#{pane_id}` 展开、`-p` 语义、
  pane↔window 映射）；但**尚未对真实的 kitty + Claude teams 会话端到端验证**，
  因为开发环境里没有 kitty。欢迎在真机上试并反馈 `unhandled:` 日志。
- `capture-pane` 严格遵守 tmux 语义：只有带 `-p` 才写 stdout，否则不输出——
  避免污染 Claude 的输出流（这正是 cmux code review 里踩过的坑）。

## 相关

- cmux 的 `claude-teams`：<https://cmux.com/docs/agent-integrations/claude-code-teams>
- Claude Code Agent Teams 文档：<https://code.claude.com/docs/en/agent-teams>
- kitty 远程控制：<https://sw.kovidgoyal.net/kitty/remote-control/>
