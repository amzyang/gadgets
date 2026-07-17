package watch

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Card 2.0 结构（仅本工具用到的子集），发送/复制草稿/忽略三按钮。
// 发送按钮无二次确认弹窗（用户决策：单击即发，幂等键防连点）。

type cardText struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

type cardBehavior struct {
	Type  string         `json:"type"`
	Value map[string]any `json:"value"`
}

type cardElement struct {
	Tag       string         `json:"tag"`
	Content   string         `json:"content,omitempty"`
	Text      *cardText      `json:"text,omitempty"`
	Type      string         `json:"type,omitempty"`
	Width     string         `json:"width,omitempty"`
	Behaviors []cardBehavior `json:"behaviors,omitempty"`
}

type draftCard struct {
	Schema string `json:"schema"`
	Config struct {
		UpdateMulti bool   `json:"update_multi"`
		WidthMode   string `json:"width_mode"`
	} `json:"config"`
	Header struct {
		Title    cardText `json:"title"`
		Subtitle cardText `json:"subtitle"`
		Template string   `json:"template"`
	} `json:"header"`
	Body struct {
		Direction       string        `json:"direction"`
		Padding         string        `json:"padding"`
		VerticalSpacing string        `json:"vertical_spacing"`
		Elements        []cardElement `json:"elements"`
	} `json:"body"`
}

var atTagRe = regexp.MustCompile(`<at[^>]*>([^<]*)</at>`)

// escapeCardMarkdown 处理进入卡片 markdown 的用户文本：
// at 标记转 @名字，markdown 特殊字符转 HTML 实体（防注入/防格式碎裂）。
func escapeCardMarkdown(s string) string {
	s = atTagRe.ReplaceAllString(s, "@$1")
	r := strings.NewReplacer(
		"<", "&#60;", ">", "&#62;", "*", "&#42;",
		"[", "&#91;", "]", "&#93;", "~", "&#126;", "`", "&#96;",
	)
	return r.Replace(s)
}

func button(label, typ string, value map[string]any) cardElement {
	return cardElement{
		Tag:       "button",
		Text:      &cardText{Tag: "plain_text", Content: label},
		Type:      typ,
		Width:     "fill",
		Behaviors: []cardBehavior{{Type: "callback", Value: value}},
	}
}

// RenderDraftCard 渲染草稿确认卡片 JSON（模板实例化从模型侧下沉到二进制）。
// 仅 mid/draft 必给；scene/from/t/original 为展示字段，空值对应片段整体省略。
func RenderDraftCard(mid, scene, from, t, original, draft string) string {
	var c draftCard
	c.Schema = "2.0"
	c.Config.UpdateMulti = true
	c.Config.WidthMode = "default"
	c.Header.Title = cardText{Tag: "plain_text", Content: "回复草稿待确认"}
	var sub []string
	for _, s := range []string{scene, from, t} {
		if s != "" {
			sub = append(sub, s)
		}
	}
	c.Header.Subtitle = cardText{Tag: "plain_text", Content: strings.Join(sub, " · ")}
	c.Header.Template = "blue"

	c.Body.Direction = "vertical"
	c.Body.Padding = "12px 12px 16px 12px"
	c.Body.VerticalSpacing = "medium"
	if original != "" {
		quoted := "> " + strings.ReplaceAll(escapeCardMarkdown(original), "\n", "\n> ")
		if from != "" {
			quoted = fmt.Sprintf("**%s：**\n%s", escapeCardMarkdown(from), quoted)
		}
		c.Body.Elements = append(c.Body.Elements, cardElement{Tag: "markdown", Content: quoted})
	}
	// 代码围栏前须空行（飞书卡片 markdown 实测要求）；草稿内含围栏时降级为 '''
	fenceSafe := strings.ReplaceAll(draft, "```", "'''")
	c.Body.Elements = append(c.Body.Elements,
		cardElement{Tag: "markdown", Content: "**草稿**\n\n```\n" + fenceSafe + "\n```"},
		button("发送", "primary_filled", map[string]any{"action": "send", "mid": mid}),
		button("复制草稿", "default", map[string]any{"action": "copy", "mid": mid}),
		button("忽略", "default", map[string]any{"action": "ignore", "mid": mid}),
	)
	return encodeCompact(c)
}

// 完成态状态行 markdown（与卡片其余 markup 同层维护）。
const (
	statusSent    = "<font color='green'>✅ 已发送</font>"
	statusIgnored = "已忽略"
	statusStale   = "⚠️ 草稿已失效，请回终端处理"
	statusFailed  = "❌ 发送失败，请回终端处理"
)

// RenderDoneCard 基于卡片原稿生成完成态：去掉全部按钮，末尾追加状态行。
// 原稿必须是发卡时落盘的本地 JSON——回调返回的 card_content 是服务端 user_dsl
// 序列化，markdown 换行在往返中会丢失，仅作缺原稿时的兜底。
func RenderDoneCard(cardJSON, status string) (string, error) {
	var card map[string]any
	if err := json.Unmarshal([]byte(cardJSON), &card); err != nil {
		return "", err
	}
	body, ok := card["body"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("card has no body object")
	}
	elements, _ := body["elements"].([]any)
	kept := make([]any, 0, len(elements))
	for _, el := range elements {
		if m, ok := el.(map[string]any); ok && m["tag"] == "button" {
			continue
		}
		kept = append(kept, el)
	}
	kept = append(kept, map[string]any{"tag": "markdown", "content": status})
	body["elements"] = kept
	return encodeCompact(card), nil
}
