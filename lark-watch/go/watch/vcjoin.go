package watch

import "strings"

// ActiveMeeting 是进行中会议的通知侧子集（user_active_meeting 响应裁剪）。
type ActiveMeeting struct {
	ID    string `json:"meeting_id"`
	No    string `json:"meeting_no"`
	Title string `json:"meeting_title"`
}

// joinLink 构造直接入会深链。lark:// 对 vc.feishu.cn 域同样生效：直接唤起
// 客户端进入会中，不经浏览器中转；applink 形态（videochat/open）不支持按
// 会议号入会，已实测排除（2026-07-27）。
func joinLink(meetingNo string) string { return "lark://vc.feishu.cn/j/" + meetingNo }

// matchJoinLink 为 VC 批次挑选进行中会议，返回直接入会深链；挑不出返回空串，
// 横幅退化为打开消息。会议列表（现在谁在开会）与邀请消息（这条消息邀我进
// 哪场）是两个无同步的数据源，唯一的关联手段是名字命中，因此**单会议也必须
// 命中**——本人所在群常有长期挂着的会，新邀请落入 user_active_meeting 又
// 滞后于 IM 消息，盲信唯一会议会把人送进无关的会；代价是自定义主题的预约
// 会议（标题不含群名/人名）拿不到深链，宁缺勿错。batch 在外层（横幅标题/
// 正文/点正文均锚定批次首条，「加入」的指向须一致），每个名字先精确
// 「<名>的视频会议」再退子串（短名嵌套如「研发」vs「研发中心」不误配）。
func matchJoinLink(batch []Message, meetings []ActiveMeeting) string {
	var live []ActiveMeeting
	for _, mt := range meetings {
		if mt.No != "" {
			live = append(live, mt)
		}
	}
	for _, m := range batch {
		for _, name := range []string{deref(m.Chat), deref(m.From)} {
			if name == "" {
				continue
			}
			for _, mt := range live {
				if mt.Title == name+"的视频会议" {
					return joinLink(mt.No)
				}
			}
			for _, mt := range live {
				if strings.Contains(mt.Title, name) {
					return joinLink(mt.No)
				}
			}
		}
	}
	return ""
}
