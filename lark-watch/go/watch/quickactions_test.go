package watch

import (
	"strings"
	"testing"
)

// 配置缺失走内置默认：常用语在前、表情（👍 回应）殿后。
func TestLoadQuickActionsDefaults(t *testing.T) {
	acts := loadQuickActions(t.TempDir())
	if len(acts) != 3 {
		t.Fatalf("want 2 replies + 1 reaction, got %v", acts)
	}
	if acts[0].Cmd != CmdSendText || acts[0].Flag != FlagText || acts[0].Value != "收到" ||
		acts[1].Value != "好的，稍后回复" {
		t.Errorf("default replies: %v", acts)
	}
	if acts[2].Cmd != CmdReact || acts[2].Flag != FlagEmoji || acts[2].Value != "THUMBSUP" || acts[2].Label != "👍 回应" {
		t.Errorf("default reaction: %v", acts[2])
	}
}

// 配置文件：注释/空行跳过；ASCII 逗号转中文逗号（标签），Value 保留原文；
// 超长标签截断；与保留字（发送/复制/忽略）重名的剔除；@ 前缀剔除（撞 alerter
// 哨兵 @CLOSED/@TIMEOUT 会把关闭/超时误判成点选）；坏 emoji type 跳过，
// 混合大小写 key（如 Get）合法。
func TestLoadQuickActionsConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "quick-replies", "# 注释\n收到,马上看\n\n发送\n@CLOSED\n"+strings.Repeat("很", 25)+"\n")
	writeConfig(t, dir, "reactions", "ok-bad\nDONE\nGet\nFINGERHEART\nYes\nJIAYI\n")

	acts := loadQuickActions(dir)
	if len(acts) != 7 {
		t.Fatalf("got %v", acts)
	}
	if acts[0].Label != "收到，马上看" || acts[0].Value != "收到,马上看" {
		t.Errorf("comma handling: %+v", acts[0])
	}
	long := acts[1]
	if len([]rune(long.Label)) != maxQuickLabelRunes+1 || !strings.HasSuffix(long.Label, "…") {
		t.Errorf("long label not truncated: %+v", long)
	}
	if long.Value != strings.Repeat("很", 25) {
		t.Errorf("value must keep full text: %+v", long)
	}
	if acts[2].Cmd != CmdReact || acts[2].Value != "DONE" || acts[2].Label != "✅ 回应" {
		t.Errorf("reaction: %+v", acts[2])
	}
	if acts[3].Value != "Get" || acts[3].Label != "收到 回应" {
		t.Errorf("mixed-case reaction: %+v", acts[3])
	}
	if acts[4].Value != "FINGERHEART" || acts[4].Label != "🫰 回应" {
		t.Errorf("fingerheart reaction: %+v", acts[4])
	}
	if acts[5].Value != "Yes" || acts[5].Label != "YES 回应" {
		t.Errorf("yes reaction: %+v", acts[5])
	}
	if acts[6].Value != "JIAYI" || acts[6].Label != "+1 回应" {
		t.Errorf("jiayi reaction: %+v", acts[6])
	}
}

// 配额：表情至多 maxReactions 且优先保住，常用语补足其余；budget 硬上限。
func TestLoadQuickActionsBudget(t *testing.T) {
	dir := t.TempDir()
	replies := make([]string, 0, maxBannerActions)
	for i := 0; i < maxBannerActions; i++ {
		replies = append(replies, strings.Repeat("语", i+1))
	}
	reactions := make([]string, 0, maxReactions+1)
	for i := 0; i < maxReactions+1; i++ {
		reactions = append(reactions, "E"+strings.Repeat("M", i+1))
	}
	writeConfig(t, dir, "quick-replies", strings.Join(replies, "\n")+"\n")
	writeConfig(t, dir, "reactions", strings.Join(reactions, "\n")+"\n")

	acts := loadQuickActions(dir)
	if len(acts) != maxBannerActions-1 {
		t.Fatalf("want %d actions, got %d: %v", maxBannerActions-1, len(acts), acts)
	}
	var texts, reacts int
	for _, a := range acts {
		if a.Cmd == CmdReact {
			reacts++
		} else {
			texts++
		}
	}
	if reacts != maxReactions || texts != maxBannerActions-1-maxReactions {
		t.Errorf("want %d texts + %d reacts, got %d/%d: %v",
			maxBannerActions-1-maxReactions, maxReactions, texts, reacts, acts)
	}
}
