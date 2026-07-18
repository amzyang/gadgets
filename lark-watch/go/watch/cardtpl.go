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
	ElementID string         `json:"element_id,omitempty"`
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

// padCardFences 适配飞书卡片 markdown 方言：开围栏前须空行才渲染为代码块
// （post 的 md tag 无此要求）；前一行非空时补空行，闭围栏不动（补了会混入代码内容）。
func padCardFences(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, l := range lines {
		if strings.HasPrefix(l, "```") {
			if !inFence && len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			inFence = !inFence
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// RenderDraftCard 渲染草稿确认卡片 JSON（模板实例化从模型侧下沉到二进制）。
// 仅 mid/drafts 必给；scene/from/t/original 为展示字段，空值对应片段整体省略。
// format=="markdown" 时草稿按 markdown 渲染（预览≈对方所见），否则包围栏展示源文。
// drafts 为候选列表（len >= 1）：单条时布局与文案同单草稿；多条时每条标注圈号
// （①②③…，SKILL.md 约束 1–3 条，圈号字符到 ⑳ 为止）并各带自己的发送按钮。
func RenderDraftCard(mid, scene, from, t, original string, drafts []string, format string) string {
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
	single := len(drafts) == 1
	for i, draft := range drafts {
		head, label := "**草稿**", "发送"
		if !single {
			head = fmt.Sprintf("**草稿 %c**", '①'+i)
			label = fmt.Sprintf("发送 %c", '①'+i)
		}
		draftMD := head + "\n\n" + padCardFences(draft)
		if format != "markdown" {
			// 代码围栏前须空行（飞书卡片 markdown 实测要求）；草稿内含围栏时降级为 '''
			draftMD = head + "\n\n```\n" + strings.ReplaceAll(draft, "```", "'''") + "\n```"
		}
		c.Body.Elements = append(c.Body.Elements,
			cardElement{Tag: "markdown", ElementID: fmt.Sprintf("draft-%d", i), Content: draftMD},
			button(label, "primary_filled", map[string]any{"action": "send", "mid": mid, "idx": i}),
		)
	}
	c.Body.Elements = append(c.Body.Elements,
		button("复制草稿", "default", map[string]any{"action": "copy", "mid": mid}),
		button("忽略", "default", map[string]any{"action": "ignore", "mid": mid}),
	)
	return encodeCompact(c)
}

// doneState 是卡片完成态：title 覆盖头部标题（脱离「待确认」），
// status 是追加到卡片末尾的状态行 markdown（与卡片其余 markup 同层维护）。
type doneState struct {
	title  string
	status string
}

var (
	doneSent    = doneState{title: "回复已发送", status: "<font color='green'>✅ 已发送</font>"}
	doneIgnored = doneState{title: "草稿已忽略", status: "已忽略"}
	doneStale   = doneState{title: "草稿已失效", status: "⚠️ 草稿已失效，请回终端处理"}
	doneFailed  = doneState{title: "回复发送失败", status: "❌ 发送失败，请回终端处理"}
)

// RenderDoneCard 基于卡片原稿生成完成态：更新头部标题、去掉全部按钮、末尾追加状态行。
// keepIdx >= 0 时只保留 element_id 为 draft-<keepIdx> 的候选块（发送成功后卡片
// 只留所发的那条）；keepIdx < 0 保留全部候选。旧卡片无 element_id，自然全保留。
// 原稿必须是发卡时落盘的本地 JSON——回调返回的 card_content 是服务端 user_dsl
// 序列化，markdown 换行在往返中会丢失，仅作缺原稿时的兜底（该路径只走 doneStale，
// 不做候选过滤，因此不依赖 element_id 在服务端往返中保留）。
func RenderDoneCard(cardJSON string, st doneState, keepIdx int) (string, error) {
	var card map[string]any
	if err := json.Unmarshal([]byte(cardJSON), &card); err != nil {
		return "", err
	}
	if st.title != "" {
		if header, ok := card["header"].(map[string]any); ok {
			if title, ok := header["title"].(map[string]any); ok {
				title["content"] = st.title
			}
		}
	}
	body, ok := card["body"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("card has no body object")
	}
	elements, _ := body["elements"].([]any)
	kept := make([]any, 0, len(elements))
	for _, el := range elements {
		m, isMap := el.(map[string]any)
		if isMap && m["tag"] == "button" {
			continue
		}
		if keepIdx >= 0 && isMap {
			if id, _ := m["element_id"].(string); strings.HasPrefix(id, "draft-") && id != fmt.Sprintf("draft-%d", keepIdx) {
				continue
			}
		}
		kept = append(kept, el)
	}
	kept = append(kept, map[string]any{"tag": "markdown", "content": st.status})
	body["elements"] = kept
	return encodeCompact(card), nil
}
