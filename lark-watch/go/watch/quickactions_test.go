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
// 哨兵 @CLOSED/@TIMEOUT 会把关闭/超时误判成点选）；坏 emoji type 跳过。
func TestLoadQuickActionsConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "quick-replies", "# 注释\n收到,马上看\n\n发送\n@CLOSED\n"+strings.Repeat("很", 25)+"\n")
	writeConfig(t, dir, "reactions", "ok-bad\nDONE\n")

	acts := loadQuickActions(dir)
	if len(acts) != 3 {
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
}

// 配额：表情至多 maxReactions 且优先保住，常用语补足其余；budget 硬上限。
func TestLoadQuickActionsBudget(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "quick-replies", "一\n二\n三\n四\n五\n")
	writeConfig(t, dir, "reactions", "THUMBSUP\nOK\nDONE\nAPPLAUSE\nHEART\n")

	acts := loadQuickActions(dir)
	if len(acts) != 8 {
		t.Fatalf("want 8 actions, got %v", acts)
	}
	var texts, reacts int
	for _, a := range acts {
		if a.Cmd == CmdReact {
			reacts++
		} else {
			texts++
		}
	}
	if texts != 4 || reacts != 4 {
		t.Errorf("want 4 texts + 4 reacts, got %d/%d: %v", texts, reacts, acts)
	}
}
