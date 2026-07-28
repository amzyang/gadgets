package watch

import "encoding/json"

// joinLink 构造直接入会深链。lark:// 对 vc.feishu.cn 域同样生效：直接唤起
// 客户端进入会中，不经浏览器中转；applink 形态（videochat/open）不支持按
// 会议号入会，已实测排除（2026-07-27）。
func joinLink(meetingNo string) string { return "lark://vc.feishu.cn/j/" + meetingNo }

// meetNumberFromContent 从 video_chat 消息原始 content（JSON 字符串，形如
// {"topic":…,"meet_number":"992101084","start_time":…}）提取会议号——消息
// 本身就是这场会的邀请，权威关联，不受 user_active_meeting 对新会议的时序
// 滞后影响（该滞后曾让「加入」必然退化）。vc_meeting（会议分享）的 content
// 结构未实测，缺字段/坏 JSON 一律返回空串，横幅退化为打开消息，宁缺勿错。
// 会议号在此闭合为纯数字（notify.md 对 LW_JOIN_LINK 的形态承诺）：该值会进
// `open` 与用户自定义脚本环境，非数字串同样退化为空串。
func meetNumberFromContent(content string) string {
	var c struct {
		MeetNumber string `json:"meet_number"`
	}
	if json.Unmarshal([]byte(content), &c) != nil {
		return ""
	}
	for _, r := range c.MeetNumber {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return c.MeetNumber
}
