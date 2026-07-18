package watch

import (
	"encoding/json"
	"fmt"
	"os"
)

// CardEvent 是 card.action.trigger 回调事件（扁平结构）。
type CardEvent struct {
	EventID     string `json:"event_id"`
	ActionTag   string `json:"action_tag"`
	Token       string `json:"token"`
	CardContent string `json:"card_content"`
	ActionValue string `json:"action_value"`
}

type cardAction struct {
	Action string `json:"action"`
	Mid    string `json:"mid"`
	Idx    int    `json:"idx"` // 发送按钮的候选索引；旧卡片无此键，零值即候选 0
}

// cardLogf 输出 [card] 前缀的 stderr 诊断日志（卡片链路专用）。
func cardLogf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[card] "+format+"\n", args...)
}

// HandleCardEvent 处理单个卡片回调（CLI 直接执行，零模型参与）。
// send 按 idx 直发对应候选（幂等键防连点）/ ignore / copy（bot 逐条回发全部候选
// 纯文本，不改卡）/ pending 缺失或 idx 越界改卡「已失效」/ 发送失败保留 pending。
func HandleCardEvent(s *Store, cli LarkCLI, self string, raw []byte, now int64) {
	var ev CardEvent
	if err := json.Unmarshal(raw, &ev); err != nil || ev.EventID == "" || ev.ActionTag != "button" {
		return
	}
	if dup, err := s.HandledSeen(ev.EventID, now, HandledMax()); err != nil {
		cardLogf("handled check failed: %v", err)
		return
	} else if dup {
		cardLogf("duplicate event %s, skipped", ev.EventID)
		return
	}

	var act cardAction
	if json.Unmarshal([]byte(ev.ActionValue), &act) != nil || act.Action == "" || act.Mid == "" {
		cardLogf("event %s missing action/mid", ev.EventID)
		return
	}

	drafts, format, cardSrc, hasPending := s.PendingGet(act.Mid)
	updateCard := func(st doneState, keepIdx int) {
		src := cardSrc
		if src == "" {
			src = ev.CardContent
		}
		if src == "" || ev.Token == "" {
			cardLogf("no card source/token, skip update")
			return
		}
		newCard, err := RenderDoneCard(src, st, keepIdx)
		if err != nil {
			cardLogf("card source parse failed: %v", err)
			return
		}
		if err := cli.UpdateCard(ev.Token, newCard); err != nil {
			cardLogf("card update failed (token 可能已用尽，属预期): %v", err)
		}
	}

	switch act.Action {
	case "send":
		if !hasPending {
			cardLogf("send: pending missing for %s", act.Mid)
			updateCard(doneStale, -1)
			return
		}
		if act.Idx < 0 || act.Idx >= len(drafts) {
			// 同 mid 重发覆盖 pending 后，旧卡按钮可能指向已不存在的候选
			cardLogf("send: idx %d out of range for %s (%d drafts)", act.Idx, act.Mid, len(drafts))
			updateCard(doneStale, -1)
			return
		}
		if err := cli.ReplyAsUser(act.Mid, drafts[act.Idx], format); err != nil {
			updateCard(doneFailed, -1)
			cardLogf("reply failed for %s (pending kept): %v", act.Mid, err)
			return
		}
		updateCard(doneSent, act.Idx)
		s.PendingDelete(act.Mid)
		cardLogf("sent reply for %s (candidate %d)", act.Mid, act.Idx)
	case "ignore":
		updateCard(doneIgnored, -1)
		s.PendingDelete(act.Mid)
		cardLogf("ignored %s", act.Mid)
	case "copy":
		if !hasPending {
			cardLogf("copy: pending missing for %s", act.Mid)
			updateCard(doneStale, -1)
			return
		}
		for _, draft := range drafts {
			if err := cli.SendTextAsBot(self, draft); err != nil {
				cardLogf("draft text send failed for %s: %v", act.Mid, err)
				return
			}
		}
		cardLogf("draft text sent for %s (%d candidate(s))", act.Mid, len(drafts))
	default:
		cardLogf("unknown action %q for %s", act.Action, act.Mid)
	}
}
