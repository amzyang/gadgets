package watch

import (
	"regexp"
	"sort"
	"strings"
)

var (
	imgMarkdownRe = regexp.MustCompile(`!\[Image\]\([^)]*\)`)
	imgBracketRe  = regexp.MustCompile(`\[Image: [^\]]*\]`)
	fileTagRe     = regexp.MustCompile(`<file key="[^"]*"(?: name="([^"]*)")?[^>]*>`)
	cardTitleRe   = regexp.MustCompile(`^<card title="([^"]*)"`)
)

// normalizeMedia 把 lark-cli 渲染的非文本占位符归一为短标记（[图片]/[文件:名]/
// [卡片:标题]/[合并转发]），避免 peek 宽度被资源 key 占满、并让非文本消息一眼可辨。
// 卡片与合并转发整体折叠；图片/文件可内联出现在正文中，逐个替换。
func normalizeMedia(text string) string {
	if strings.HasPrefix(text, "<forwarded_messages>") {
		return "[合并转发]"
	}
	if strings.HasPrefix(text, "<card") {
		if m := cardTitleRe.FindStringSubmatch(text); m != nil {
			return "[卡片:" + m[1] + "]"
		}
		return "[卡片]"
	}
	text = imgMarkdownRe.ReplaceAllString(text, "[图片]")
	text = imgBracketRe.ReplaceAllString(text, "[图片]")
	return fileTagRe.ReplaceAllStringFunc(text, func(tag string) string {
		if m := fileTagRe.FindStringSubmatch(tag); m[1] != "" {
			return "[文件:" + m[1] + "]"
		}
		return "[文件]"
	})
}

// BuildDigest 把缓冲的 P1 消息按会话聚合成摘要。
// 会话标签：首个非空群名，缺失回退 cid；peek 取该会话最新一条（t 相等取后者），
// "发送者: 正文" 截 60 码点（正文先经 normalizeMedia 归一）；排序按 [-条数, cid]。
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
			Peek: truncateRunes(from+": "+normalizeMedia(g.latest.Text), 60),
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
