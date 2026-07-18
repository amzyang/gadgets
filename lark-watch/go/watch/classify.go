package watch

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// 规则配置文件名（ConfigDir 下），归本模块所有。
const (
	watchlistFile = "watchlist"
	keywordsFile  = "keywords"
	ignoreFile    = "ignore"
)

// Rules 是分级规则集，每 tick 从配置文件重建（修改即生效）。
// 正则方言为 Go RE2（自 bash 版的 POSIX ERE 迁移；无回溯引用/环视）。
type Rules struct {
	Self       string
	WatchUsers map[string]bool
	WatchChats map[string]bool
	WatchNames map[string]bool
	Keywords   []*regexp.Regexp
	Ignore     []*regexp.Regexp
}

// LoadRulesDir 从配置目录读取全部规则文件。
func LoadRulesDir(self, configDir string) Rules {
	return LoadRules(self,
		filepath.Join(configDir, watchlistFile),
		filepath.Join(configDir, keywordsFile),
		filepath.Join(configDir, ignoreFile))
}

// LoadRules 读取 watchlist/keywords/ignore；文件缺失视为空，坏正则跳过并 stderr 告警。
func LoadRules(self, watchlistPath, keywordsPath, ignorePath string) Rules {
	r := Rules{
		Self:       self,
		WatchUsers: map[string]bool{},
		WatchChats: map[string]bool{},
		WatchNames: map[string]bool{},
	}
	for _, line := range readConfigLines(watchlistPath) {
		switch {
		case strings.HasPrefix(line, "ou_"):
			r.WatchUsers[line] = true
		case strings.HasPrefix(line, "oc_"):
			r.WatchChats[line] = true
		default:
			r.WatchNames[line] = true
		}
	}
	r.Keywords = loadPatterns(keywordsPath)
	r.Ignore = loadPatterns(ignorePath)
	return r
}

// readConfigLines 读取配置行：去 # 注释、修剪空白、跳过空行。
func readConfigLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func loadPatterns(path string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, line := range readConfigLines(path) {
		re, err := regexp.Compile(line)
		if err != nil {
			logf("skipping invalid regex in %s: %s", path, line)
			continue
		}
		out = append(out, re)
	}
	return out
}

// vcTypes 是发起/分享音视频通话与会议的消息类型：实时性强，直接升 P0；
// 这类消息 content 常为空，豁免空文本丢弃。
var vcTypes = map[string]bool{"video_chat": true, "vc_meeting": true}

// Classify 对单条消息定级。返回 (打好 p 标签的消息, 是否保留)。
// 丢弃：自己发的 / 非 user 发送者 / 空文本（音视频会议除外）/ ignore 命中
// （ignore 可压掉 P0）。
// P0：音视频会议、p2p、@我（mentions 命中 self，或预渲染 content 的
// <at user_id="self"> 标记；@all 不算）、watchlist（重点人/群/名称精确匹配）、
// 关键词命中；其余 P1。
func (r Rules) Classify(m Message) (Message, bool) {
	if m.Fid == r.Self || m.Ftype != "user" || (m.Text == "" && !vcTypes[m.Type]) {
		return m, false
	}
	blob := m.Cid + " " + deref(m.Chat) + " " + deref(m.From) + " " + m.Text
	for _, re := range r.Ignore {
		if re.MatchString(blob) {
			return m, false
		}
	}
	m.P = "P1"
	if vcTypes[m.Type] ||
		m.Ctype == "p2p" ||
		slices.Contains(m.AtIDs, r.Self) ||
		strings.Contains(m.Text, `<at user_id="`+r.Self+`"`) ||
		r.WatchUsers[m.Fid] ||
		r.WatchChats[m.Cid] ||
		r.WatchNames[deref(m.From)] ||
		r.WatchNames[deref(m.Chat)] {
		m.P = "P0"
	} else {
		for _, re := range r.Keywords {
			if re.MatchString(m.Text) {
				m.P = "P0"
				break
			}
		}
	}
	return m, true
}

// SelfLastTimes 提取本人消息的每会话最新时间（cid → create_time，
// minuteLayout 字符串可直接比较）。本人消息随后会被 Classify 当噪音丢弃，
// 但「我已在该会话发过言」是 replied 注记（MarkReplied）的信号源。
func SelfLastTimes(msgs []Message, self string) map[string]string {
	out := map[string]string{}
	for _, m := range msgs {
		if m.Fid == self && m.T > out[m.Cid] {
			out[m.Cid] = m.T
		}
	}
	return out
}

// ClassifyAll 批量定级，仅保留通过的消息。
func (r Rules) ClassifyAll(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if tagged, keep := r.Classify(m); keep {
			out = append(out, tagged)
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
