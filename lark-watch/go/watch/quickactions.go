package watch

import (
	"path/filepath"
	"regexp"
	"strings"
)

// 快捷动作配置文件名（ConfigDir 下），归本模块所有。
const (
	quickRepliesFile = "quick-replies"
	reactionsFile    = "reactions"
)

const (
	maxBannerActions   = 6  // 横幅下拉动作总数上限（含固定首键：发送/复制）
	maxReactions       = 2  // 表情动作上限（保住常用语位置）
	maxQuickLabelRunes = 20 // 下拉标签截断长度（发送内容仍是全文）
)

// 内置默认：quick-replies / reactions 文件缺失时生效。
var (
	defaultQuickReplies = []string{"收到", "好的，稍后回复"}
	defaultReactions    = []string{"THUMBSUP"}
)

// reactionLabels 是 EMOJI_TYPE → 下拉显示标签；查不到用原文（飞书 emoji key
// 全集很大，只映射常用的）。
var reactionLabels = map[string]string{
	"THUMBSUP": "👍", "OK": "👌", "DONE": "✅", "APPLAUSE": "👏",
	"HEART": "❤️", "THANKS": "🙏", "JIAYI": "+1",
}

// emojiTypeRe 校验 reactions 配置行（飞书 emoji_type 形如 THUMBSUP/OK）。
var emojiTypeRe = regexp.MustCompile(`^[A-Z0-9_]+$`)

// quickAction 是通知横幅下拉里的一个快捷动作，携带回调规格：
// alerter 分发片段据此拼 `<Cmd> --mid <mid> --<Flag> <Value>`，
// 新增动作种类只改这里的构造，不动脚本生成器。
type quickAction struct {
	Label string // 下拉文案（已清洗：逗号转中文、超长截断）
	Cmd   string // 回调子命令（CmdSendText | CmdReact）
	Flag  string // 值参数的 flag 名（FlagText | FlagEmoji）
	Value string // 常用语全文（保留原始逗号）或 EMOJI_TYPE
}

// reservedActionLabels 是横幅固定键位文案，快捷动作不得重名
// （sh case 首个匹配分支胜出，重名会点错动作）。
var reservedActionLabels = map[string]bool{"发送": true, "复制": true, "忽略": true}

// loadQuickActions 读取快捷动作配置（每次弹横幅现读，改完即生效）：
// 常用语在前、表情在后，表情配额至多 maxReactions、常用语补足 budget 余量
// （budget = maxBannerActions 扣除固定首键占位）。
// 文件缺失用内置默认；标签清洗见 quickReplyAction/reactionAction；
// 超配额截断记 evlog（notify.actions.trim）。
func loadQuickActions(configDir string) []quickAction {
	budget := maxBannerActions - 1
	replies := readConfigLines(filepath.Join(configDir, quickRepliesFile))
	if replies == nil {
		replies = defaultQuickReplies
	}
	reactions := readConfigLines(filepath.Join(configDir, reactionsFile))
	if reactions == nil {
		reactions = defaultReactions
	}

	var reacts []quickAction
	for _, r := range reactions {
		if !emojiTypeRe.MatchString(r) {
			logf("skipping invalid emoji type in %s: %s", reactionsFile, r)
			continue
		}
		if len(reacts) == maxReactions {
			break
		}
		reacts = append(reacts, quickAction{Label: reactionLabel(r), Cmd: CmdReact, Flag: FlagEmoji, Value: r})
	}
	if n := len(reacts); n > budget {
		reacts = reacts[:budget]
	}

	out := make([]quickAction, 0, budget)
	seen := map[string]bool{}
	for k := range reservedActionLabels {
		seen[k] = true
	}
	for _, text := range replies {
		if len(out) == budget-len(reacts) {
			break
		}
		label := quickLabel(text)
		// @ 前缀撞 alerter 哨兵输出（@CONTENTCLICKED/@CLOSED/@TIMEOUT）：
		// 关闭/超时本该忽略，却会命中该动作臂误发消息。
		if strings.HasPrefix(label, "@") {
			logf("skipping quick reply with alerter-reserved prefix in %s: %s", quickRepliesFile, label)
			continue
		}
		if seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, quickAction{Label: label, Cmd: CmdSendText, Flag: FlagText, Value: text})
	}
	for _, r := range reacts {
		if seen[r.Label] {
			continue
		}
		seen[r.Label] = true
		out = append(out, r)
	}
	if dropped := len(replies) + len(reactions) - len(out); dropped > 0 {
		evlog.Debug("notify.actions.trim", "kept", len(out), "dropped", dropped)
	}
	return out
}

// quickLabel 清洗常用语的下拉标签：ASCII 逗号转中文（alerter --actions 按 ASCII
// 逗号切分 CSV）、超长截断加省略号；Value 仍发全文。
func quickLabel(text string) string {
	label := strings.ReplaceAll(text, ",", "，")
	if runes := []rune(label); len(runes) > maxQuickLabelRunes {
		label = string(runes[:maxQuickLabelRunes]) + "…"
	}
	return label
}

// reactionLabel 是表情动作的下拉标签：常用 emoji 给符号，未知 key 用原文。
func reactionLabel(emojiType string) string {
	if l, ok := reactionLabels[emojiType]; ok {
		return l + " 回应"
	}
	return emojiType
}
