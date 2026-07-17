package watch

import (
	"encoding/json"
	"sort"
	"strings"
)

type rawSender struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	SenderType string `json:"sender_type"`
}

type rawMention struct {
	ID string `json:"id"`
}

type rawMessage struct {
	MessageID      string       `json:"message_id"`
	ChatID         string       `json:"chat_id"`
	ChatType       string       `json:"chat_type"`
	ChatName       string       `json:"chat_name"`
	Sender         rawSender    `json:"sender"`
	Mentions       []rawMention `json:"mentions"`
	MsgType        string       `json:"msg_type"`
	Content        string       `json:"content"`
	MessageAppLink string       `json:"message_app_link"`
	CreateTime     string       `json:"create_time"`
}

type listEnvelope struct {
	Data struct {
		Messages []rawMessage `json:"messages"`
		HasMore  bool         `json:"has_more"`
	} `json:"data"`
}

// toMessage 把单条 raw 消息转为统一形态：正文按码点截 500，applink 换 lark:// scheme。
func toMessage(m rawMessage) Message {
	var chat, from *string
	if m.ChatName != "" {
		chat = &m.ChatName
	}
	if m.Sender.Name != "" {
		from = &m.Sender.Name
	}
	var atIDs []string
	for _, mt := range m.Mentions {
		if mt.ID != "" {
			atIDs = append(atIDs, mt.ID)
		}
	}
	return Message{
		Mid:   m.MessageID,
		Cid:   m.ChatID,
		Ctype: m.ChatType,
		Chat:  chat,
		From:  from,
		Fid:   m.Sender.ID,
		Ftype: m.Sender.SenderType,
		Type:  m.MsgType,
		Text:  truncateRunes(m.Content, 500),
		Link:  toLarkScheme(m.MessageAppLink),
		T:     m.CreateTime,
		AtIDs: atIDs,
	}
}

// Trim 把 messages-search 原始响应（data.messages 信封）裁剪为统一消息形态，
// 按 create_time 升序。chat-messages-list 的响应共用此形态。
func Trim(raw []byte) (msgs []Message, hasMore bool, err error) {
	var env listEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false, err
	}
	items := env.Data.Messages
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreateTime < items[j].CreateTime })
	msgs = make([]Message, 0, len(items))
	for _, m := range items {
		msgs = append(msgs, toMessage(m))
	}
	return msgs, env.Data.HasMore, nil
}

// TrimChatMessages 把 chat-messages-list 原始响应裁剪为统一消息形态。
// 该接口响应不含 chat_name/chat_type，由调用方从 chat-list 元数据注入；
// p2p 会话对齐既有契约 Chat 置 null。
func TrimChatMessages(raw []byte, chatName, chatType string) ([]Message, bool, error) {
	msgs, hasMore, err := Trim(raw)
	if err != nil {
		return nil, false, err
	}
	for i := range msgs {
		msgs[i].Ctype = chatType
		if chatType != "p2p" && chatName != "" {
			msgs[i].Chat = &chatName
		} else {
			msgs[i].Chat = nil
		}
	}
	return msgs, hasMore, nil
}

// toLarkScheme 把 API 返回的 https applink 换成 lark:// scheme（直接唤起客户端，
// 不经浏览器跳转），与 ApplinkHost 构造的链接保持一致。
func toLarkScheme(link string) string {
	return strings.Replace(link, "https://applink.feishu.cn", ApplinkHost, 1)
}
