package watch

import "sort"

// CatchupGroup 把分类后的积压消息按会话分组。
// 过滤边界：该会话游标（无游标用 floor），t >= 边界——分钟精度字符串比较，宁可重看不漏。
// 会话标签：首个非空群名 → 首个非空发送者名 → cid；排序：含 P0 的会话在前，再按 -条数、cid。
// peek 取每会话按时间升序的最后 peek 条，正文截 120 码点。
func CatchupGroup(msgs []Message, cursors map[string]string, floor string, peek int, truncated bool) Catchup {
	var kept []Message
	for _, m := range msgs {
		boundary := floor
		if c, ok := cursors[m.Cid]; ok {
			boundary = c
		}
		if m.T >= boundary {
			kept = append(kept, m)
		}
	}

	groups := map[string][]Message{}
	for _, m := range kept {
		groups[m.Cid] = append(groups[m.Cid], m)
	}

	chats := make([]CatchupChat, 0, len(groups))
	for cid, g := range groups {
		sort.SliceStable(g, func(i, j int) bool { return g[i].T < g[j].T })
		chat, p0 := "", 0
		var fromName string
		for _, m := range g {
			if chat == "" && m.Chat != nil {
				chat = *m.Chat
			}
			if fromName == "" && m.From != nil {
				fromName = *m.From
			}
			if m.P == "P0" {
				p0++
			}
		}
		if chat == "" {
			chat = fromName
		}
		if chat == "" {
			chat = cid
		}
		start := max(0, len(g)-peek)
		items := make([]PeekItem, 0, peek)
		for _, m := range g[start:] {
			items = append(items, PeekItem{
				Mid: m.Mid, From: m.From, Text: truncateRunes(m.Text, 120), T: m.T, P: m.P,
			})
		}
		chats = append(chats, CatchupChat{
			Cid: cid, Chat: chat, Ctype: g[0].Ctype, N: len(g), P0: p0,
			FirstT: g[0].T, LastT: g[len(g)-1].T,
			Link: chatOpenLink(cid),
			Peek: items,
		})
	}
	// 比较器含 cid 唯一决胜，是全序 → 无需稳定排序，map 遍历序不影响输出
	sort.Slice(chats, func(i, j int) bool {
		pi, pj := chats[i].P0 > 0, chats[j].P0 > 0
		if pi != pj {
			return pi
		}
		if chats[i].N != chats[j].N {
			return chats[i].N > chats[j].N
		}
		return chats[i].Cid < chats[j].Cid
	})
	return Catchup{P: "catchup", Floor: floor, Total: len(kept), Truncated: truncated, Chats: chats}
}
