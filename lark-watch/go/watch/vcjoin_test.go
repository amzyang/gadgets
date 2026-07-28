package watch

import "testing"

func TestJoinLink(t *testing.T) {
	if got := joinLink("424223711"); got != "lark://vc.feishu.cn/j/424223711" {
		t.Errorf("joinLink: got %q", got)
	}
}

// content 是本条邀请的权威关联，缺字段/坏 JSON 一律空串（横幅退化为打开
// 消息，宁缺勿错）。真实样本取自 2026-07-28 实测 video_chat 消息。
func TestMeetNumberFromContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"真实样本",
			`{"topic":"AIGC开发站会群 -  agent线上化+养号托号的视频会议","meet_number":"992101084","start_time":"1785201892000","end_time":"1785202484000"}`,
			"992101084"},
		{"无 meet_number", `{"text":"在吗"}`, ""},
		{"非数字会议号不进 open", `{"meet_number":"992101084; rm -rf /tmp/x"}`, ""},
		{"坏 JSON", "not json", ""},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := meetNumberFromContent(c.content); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
