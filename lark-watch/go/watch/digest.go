package watch

import "sort"

// BuildDigest 把缓冲的 P1 消息按会话聚合成摘要。
// 会话标签：首个非空群名，缺失回退 cid；peek 取该会话最新一条（t 相等取后者），
// "发送者: 正文" 截 60 码点；排序按 [-条数, cid]。
func BuildDigest(msgs []Message) Digest {
	type group struct {
		chat   string
		latest Message
		n      int
	}
	groups := map[string]*group{}
	for _, m := range msgs {
		g, ok := groups[m.Cid]
		if !ok {
			g = &group{latest: m}
			groups[m.Cid] = g
		}
		if g.chat == "" {
			g.chat = deref(m.Chat)
		}
		if m.T >= g.latest.T {
			g.latest = m
		}
		g.n++
	}
	chats := make([]DigestChat, 0, len(groups))
	for cid, g := range groups {
		from := "?"
		if g.latest.From != nil {
			from = *g.latest.From
		}
		chat := g.chat
		if chat == "" {
			chat = cid
		}
		chats = append(chats, DigestChat{
			Chat: chat,
			Cid:  cid,
			N:    g.n,
			Peek: truncateRunes(from+": "+g.latest.Text, 60),
			Link: chatOpenLink(cid),
		})
	}
	sort.Slice(chats, func(i, j int) bool {
		if chats[i].N != chats[j].N {
			return chats[i].N > chats[j].N
		}
		return chats[i].Cid < chats[j].Cid
	})
	return Digest{P: "digest", N: len(msgs), Chats: chats}
}

// ShouldFlush 判定摘要缓冲是否该 flush（时间经参数注入，可测）。
func ShouldFlush(count, max int, last, now, window int64) bool {
	return count > 0 && (count >= max || now-last >= window)
}
