package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// CardEvent 是 card.action.trigger 回调事件（扁平结构）。
type CardEvent struct {
	EventID     string `json:"event_id"`
	ActionTag   string `json:"action_tag"`
	Token       string `json:"token"`
	MessageID   string `json:"message_id"`  // 卡片自身 om_xxx，token 缺失/用尽时 PATCH 兜底
	OperatorID  string `json:"operator_id"` // 点击者 open_id（服务端填充）
	CardContent string `json:"card_content"`
	ActionValue string `json:"action_value"`
}

type cardAction struct {
	Action string `json:"action"`
	Mid    string `json:"mid"`
	Idx    int    `json:"idx"` // 发送/预约按钮的候选索引；旧卡片无此键，零值即候选 0
}

// cardLogf 输出 [card] 前缀的 stderr 诊断日志（卡片链路专用），
// 并 tee 一份进事件日志（evlog）。
func cardLogf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "[card] %s\n", msg)
	evlog.Info(msg, "component", "card")
}

// CardHandler 处理卡片回调（CLI 直接执行，零模型参与）。Booker 供预约意向卡
// 的「预约」按钮执行 room book；Out 供 book 分支向 stdout 事件流发
// booked/book-failed（Monitor 主唤醒信号），nil 时静默丢弃。
type CardHandler struct {
	Store  *Store
	CLI    LarkCLI
	Booker RoomBooker
	Self   string
	Out    func(line []byte)
}

// Handle 处理单个卡片回调。草稿卡：send 按 idx 直发对应候选（幂等键防连点）/
// ignore / copy（bot 逐条回发全部候选纯文本，不改卡）/ pending 缺失或 idx 越界
// 改卡「已失效」/ 发送失败保留 pending。预约意向卡：book 执行 room book
// （claim 防双订）/ book-ignore 丢弃。
func (h *CardHandler) Handle(raw []byte, now int64) {
	var ev CardEvent
	if err := json.Unmarshal(raw, &ev); err != nil || ev.EventID == "" || ev.ActionTag != "button" {
		return
	}
	// 卡片按钮触发真实副作用（以用户身份发消息 / room book），只认本人点击：
	// 卡片被转发后他人点按钮直接丢弃。operator_id 缺失放行（信息不足不误杀）。
	if ev.OperatorID != "" && h.Self != "" && ev.OperatorID != h.Self {
		cardLogf("event %s operator %s is not self, dropped", ev.EventID, ev.OperatorID)
		return
	}
	if dup, err := h.Store.HandledSeen(ev.EventID, now, HandledMax()); err != nil {
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
	evlog.Info("card.action", "action", act.Action, "mid", act.Mid, "idx", act.Idx, "event_id", ev.EventID)

	switch act.Action {
	case "book", "book-ignore":
		h.handleBook(ev, act, now)
	default:
		h.handleDraft(ev, act)
	}
}

// updateDoneCard 把回调对应的卡片改为完成态：本地原稿 src 优先，缺失回退事件
// card_content；token 版失败/缺失按事件自带的卡片 message_id PATCH 兜底。
func (h *CardHandler) updateDoneCard(ev CardEvent, src string, st doneState, keepIdx int) {
	if src == "" {
		src = ev.CardContent
	}
	if src == "" {
		cardLogf("no card source, skip update")
		return
	}
	newCard, err := RenderDoneCard(src, st, keepIdx)
	if err != nil {
		cardLogf("card source parse failed: %v", err)
		return
	}
	if ev.Token != "" {
		if h.CLI.UpdateCard(ev.Token, newCard) == nil {
			return
		}
		cardLogf("card update failed (token 可能已用尽), trying patch fallback")
	}
	// token 缺失或已用尽（30 分钟/2 次）：按事件自带的卡片 message_id PATCH。
	// 不查库——doneStale 场景 pending 已缺失，事件字段是唯一可靠来源。
	if ev.MessageID == "" {
		cardLogf("no token/message_id, skip update")
		return
	}
	if err := h.CLI.PatchCard(ev.MessageID, newCard); err != nil {
		cardLogf("card patch fallback failed: %v", err)
	}
}

// handleDraft 处理草稿确认卡的 send/ignore/copy 分支。
func (h *CardHandler) handleDraft(ev CardEvent, act cardAction) {
	drafts, format, cardSrc, hasPending := h.Store.PendingGet(act.Mid)
	updateCard := func(st doneState, keepIdx int) {
		h.updateDoneCard(ev, cardSrc, st, keepIdx)
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
		if err := h.CLI.ReplyAsUser(act.Mid, drafts[act.Idx], format, act.Mid); err != nil {
			updateCard(doneFailed, -1)
			cardLogf("reply failed for %s (pending kept): %v", act.Mid, err)
			return
		}
		updateCard(doneSent, act.Idx)
		h.Store.PendingDelete(act.Mid)
		cardLogf("sent reply for %s (candidate %d)", act.Mid, act.Idx)
	case "ignore":
		updateCard(doneIgnored, -1)
		h.Store.PendingDelete(act.Mid)
		cardLogf("ignored %s", act.Mid)
	case "copy":
		if !hasPending {
			cardLogf("copy: pending missing for %s", act.Mid)
			updateCard(doneStale, -1)
			return
		}
		for _, draft := range drafts {
			if err := h.CLI.SendTextAsBot(h.Self, draft); err != nil {
				cardLogf("draft text send failed for %s: %v", act.Mid, err)
				return
			}
		}
		cardLogf("draft text sent for %s (%d candidate(s))", act.Mid, len(drafts))
	default:
		cardLogf("unknown action %q for %s", act.Action, act.Mid)
	}
}

// handleBook 处理预约意向卡按钮。book：Claim（原子删除；room book 无幂等键，
// 双订防护只能靠 claim）→ 以认领到的行校验 idx 并预订——绝不出现「按过期快照
// 预订、却删掉刚覆盖进来的新参数」。idx 越界（旧卡点了被覆盖掉的候选）认领
// 作废、参数放回；预订失败不 re-put——按钮已被改卡移除，重试由模型按
// book-failed 事件重发新卡。预订同步执行、刻意用 Background 隔离关停取消
// （不入 goroutine，关停时不丢下进行到一半的预订）：正常数秒；room 挂死时
// consumer 与 SIGTERM 关停最坏被拖 roomBookTimeout（60s）——已知取舍，
// 宁可等预订收尾，不留「订没订上」的不定态。
func (h *CardHandler) handleBook(ev CardEvent, act cardAction, now int64) {
	if act.Action == "book-ignore" {
		bp, _ := h.Store.BookPendingGet(act.Mid)
		h.updateDoneCard(ev, bp.Card, doneIgnored, -1)
		h.Store.BookPendingDelete(act.Mid)
		cardLogf("book card ignored for %s", act.Mid)
		return
	}
	bp, claimed := h.Store.BookPendingClaim(act.Mid)
	if !claimed {
		cardLogf("book: pending missing or claimed for %s", act.Mid)
		h.updateDoneCard(ev, bp.Card, doneStale, -1)
		return
	}
	if act.Idx < 0 || act.Idx >= len(bp.Slots) {
		h.Store.BookPendingPut(act.Mid, bp, now)
		cardLogf("book: idx %d out of range for %s (%d slots)", act.Idx, act.Mid, len(bp.Slots))
		h.updateDoneCard(ev, bp.Card, doneStale, -1)
		return
	}

	slot := bp.Slots[act.Idx]
	res, err := h.Booker.Book(context.Background(), slot, bp.Title, bp.Participants)
	if err != nil {
		be, isBook := err.(*BookError)
		if !isBook {
			be = &BookError{Type: "internal", Message: err.Error()}
		}
		evlog.Info("card.book", "mid", act.Mid, "idx", act.Idx, "ok", false, "reason", be.Type)
		h.updateDoneCard(ev, bp.Card, doneBookFailed(be), -1)
		h.emit(BookFailedEvent{P: "book-failed", Title: bp.Title, Reason: be.Type, Msg: be.Message, Hint: be.Hint, Mid: act.Mid})
		cardLogf("book failed for %s: %v", act.Mid, be)
		return
	}
	evlog.Info("card.book", "mid", act.Mid, "idx", act.Idx, "ok", true, "room", res.Room)
	h.updateDoneCard(ev, bp.Card, doneBooked(res), act.Idx)
	h.emit(BookedEvent{P: "booked", Title: bp.Title, Room: res.Room, Date: res.Date,
		Start: res.Start, End: res.End, Mid: act.Mid, EventID: res.EventID})
	cardLogf("booked %s (%s %s-%s) for %s", res.Room, res.Date, res.Start, res.End, act.Mid)
}

// emit 向 stdout 事件流发一行 JSON 并留痕事件日志。
func (h *CardHandler) emit(v any) {
	if h.Out == nil {
		return
	}
	h.Out(EncodeLine(v))
	logEmit(v)
}
