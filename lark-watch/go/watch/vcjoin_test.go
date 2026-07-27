package watch

import "testing"

func TestJoinLink(t *testing.T) {
	if got := joinLink("424223711"); got != "lark://vc.feishu.cn/j/424223711" {
		t.Errorf("joinLink: got %q", got)
	}
}

// 匹配策略：按标题含群名/发起人名匹配，单会议也必须命中（会议列表与邀请
// 消息无同步，盲信唯一会议会误入无关的会）；缺会议号、无匹配、名字为空
// 一律返回空串（横幅退化为打开消息，宁缺勿错）。
func TestMatchJoinLink(t *testing.T) {
	group := []Message{{Chat: strPtr("测试群"), From: strPtr("张三"), Ctype: "group", Type: "vc_meeting"}}
	cases := []struct {
		name     string
		batch    []Message
		meetings []ActiveMeeting
		want     string
	}{
		{"无进行中会议", group, nil, ""},
		{"唯一会议名字命中", group,
			[]ActiveMeeting{{No: "111222333", Title: "测试群的视频会议"}},
			"lark://vc.feishu.cn/j/111222333"},
		{"唯一会议不相关宁缺勿错", group,
			[]ActiveMeeting{{No: "111222333", Title: "别的群的视频会议"}}, ""},
		{"唯一会议缺会议号", group,
			[]ActiveMeeting{{Title: "测试群的视频会议"}}, ""},
		{"多会议按群名匹配", group,
			[]ActiveMeeting{
				{No: "111111111", Title: "别的群的视频会议"},
				{No: "222222222", Title: "测试群的视频会议"},
			}, "lark://vc.feishu.cn/j/222222222"},
		{"多会议按发起人匹配",
			[]Message{{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat"}},
			[]ActiveMeeting{
				{No: "111111111", Title: "别的群的视频会议"},
				{No: "333333333", Title: "李四的视频会议"},
			}, "lark://vc.feishu.cn/j/333333333"},
		{"多会议无匹配宁缺勿错", group,
			[]ActiveMeeting{
				{No: "111111111", Title: "A群的视频会议"},
				{No: "222222222", Title: "B群的视频会议"},
			}, ""},
		{"批次首条优先于会议返回顺序",
			[]Message{
				{From: strPtr("李四"), Ctype: "p2p", Type: "video_chat"},
				{Chat: strPtr("测试群"), From: strPtr("张三"), Ctype: "group", Type: "vc_meeting"},
			},
			[]ActiveMeeting{
				{No: "222222222", Title: "测试群的视频会议"},
				{No: "333333333", Title: "李四的视频会议"},
			}, "lark://vc.feishu.cn/j/333333333"},
		{"精确标题优先于子串", []Message{{Chat: strPtr("研发"), Ctype: "group", Type: "vc_meeting"}},
			[]ActiveMeeting{
				{No: "111111111", Title: "研发中心周会群的视频会议"},
				{No: "222222222", Title: "研发的视频会议"},
			}, "lark://vc.feishu.cn/j/222222222"},
		{"名字为空不得误配",
			[]Message{{Ctype: "p2p", Type: "video_chat"}},
			[]ActiveMeeting{
				{No: "111111111", Title: "A群的视频会议"},
				{No: "222222222", Title: "B群的视频会议"},
			}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchJoinLink(c.batch, c.meetings); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// user_active_meeting 响应裁剪为 ActiveMeeting 子集；坏 JSON 报错。
func TestParseActiveMeetings(t *testing.T) {
	out := []byte(`{"ok":true,"data":{"meetings":[
		{"meeting_id":"7667011109862853619","meeting_no":"424223711","meeting_title":"测试群的视频会议"}]}}`)
	ms, err := parseActiveMeetings(out)
	if err != nil || len(ms) != 1 ||
		ms[0] != (ActiveMeeting{ID: "7667011109862853619", No: "424223711", Title: "测试群的视频会议"}) {
		t.Fatalf("got %v, %v", ms, err)
	}
	if _, err := parseActiveMeetings([]byte("not json")); err == nil {
		t.Error("bad json: want error")
	}
}
