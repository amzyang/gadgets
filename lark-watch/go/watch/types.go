package watch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ApplinkHost 是 applink 的品牌域（飞书）。lark:// scheme 直接唤起客户端，
// 不经浏览器跳转。
const ApplinkHost = "lark://applink.feishu.cn"

// chatOpenLink 构造打开会话的 applink。
func chatOpenLink(cid string) string {
	return ApplinkHost + "/client/chat/open?openChatId=" + cid
}

// logf 输出 [lark-watch] 前缀的 stderr 诊断日志（事件流走 stdout，互不污染）。
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[lark-watch] "+format+"\n", args...)
}

// Message 是过滤管线的统一消息形态。
// 字段声明顺序即输出 JSON 键序（golden fixtures 字节对齐）；
// 扫读友好：正文/发送者靠前，ID 类字段收尾（单行截断也能看到重点）。
// 同会话聚合事件（GroupP0）额外携带 N/Msgs，顶层字段取最后一条作代表，
// 单条事件两字段缺省、输出与聚合前字节一致。
type Message struct {
	P       string   `json:"p,omitempty"`
	N       int      `json:"n,omitempty"`       // 聚合条数，仅 ≥2 时出现
	Msgs    []P0Item `json:"msgs,omitempty"`    // 聚合子条目（时间升序），仅 N≥2 时出现
	Replied bool     `json:"replied,omitempty"` // 该消息之后本人已在同会话发言（大概率已亲自处理）
	Text    string   `json:"text"`
	From    *string  `json:"from"`
	Chat    *string  `json:"chat"`
	T       string   `json:"t"`
	Ctype   string   `json:"ctype"`
	Type    string   `json:"type"`
	Mid     string   `json:"mid"`
	Cid     string   `json:"cid"`
	Fid     string   `json:"fid"`
	Ftype   string   `json:"ftype"`
	Link    string   `json:"link"`
	AtIDs   []string `json:"-"` // mentions 的 open_id 列表；仅供分级判 @我，不进事件 JSON
}

// P0Item 是聚合事件的子条目：正文靠前、ID 收尾，键序哲学与 Message 一致。
// 会话级字段（chat/cid/ctype/link）由顶层承载，不在子条目里重复。
type P0Item struct {
	Text string  `json:"text"`
	From *string `json:"from"`
	T    string  `json:"t"`
	Type string  `json:"type"`
	Mid  string  `json:"mid"`
	Fid  string  `json:"fid"`
}

// ChatMeta 是 chat-list 返回的会话元数据（按 active_time 降序）。
type ChatMeta struct {
	Cid  string
	Name string
	Mode string // p2p | group
}

type DigestChat struct {
	Chat string `json:"chat"`
	Cid  string `json:"cid"`
	N    int    `json:"n"`
	Peek string `json:"peek"`
	Link string `json:"link"`
}

type Digest struct {
	P     string       `json:"p"`
	N     int          `json:"n"`
	Chats []DigestChat `json:"chats"`
}

type PeekItem struct {
	From *string `json:"from"`
	Text string  `json:"text"`
	T    string  `json:"t"`
	P    string  `json:"p"`
	Mid  string  `json:"mid"`
}

type CatchupChat struct {
	Cid    string     `json:"cid"`
	Chat   string     `json:"chat"`
	Ctype  string     `json:"ctype"`
	N      int        `json:"n"`
	P0     int        `json:"p0"`
	FirstT string     `json:"first_t"`
	LastT  string     `json:"last_t"`
	Link   string     `json:"link"`
	Peek   []PeekItem `json:"peek"`
}

type Catchup struct {
	P         string        `json:"p"`
	Floor     string        `json:"floor"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated"`
	Chats     []CatchupChat `json:"chats"`
}

type Alert struct {
	P    string `json:"p"`
	Kind string `json:"kind"`
	Msg  string `json:"msg"`
}

// NewAlert 构造 alert 事件（p 判别式归此处所有）。
func NewAlert(kind, msg string) Alert {
	return Alert{P: "alert", Kind: kind, Msg: msg}
}

type Backlog struct {
	P           string `json:"p"`
	OfflineSecs int64  `json:"offline_secs"`
}

// RestrictedChat 是开启防泄密模式而被跳过监控的群（API 无法读取，status 可见）。
type RestrictedChat struct {
	Cid   string `json:"cid"`
	Name  string `json:"name"`
	Since int64  `json:"since"`
}

// Status 是 status 子命令的健康 JSON。
type Status struct {
	Cursor               int64            `json:"cursor"`
	Heartbeat            int64            `json:"heartbeat"`
	HeartbeatAge         int64            `json:"heartbeat_age_secs"`
	ConsumerState        string           `json:"consumer_state"`
	Pending              int              `json:"pending"`
	DigestBuffered       int              `json:"digest_buffered"`
	LastFlush            int64            `json:"last_flush"`
	RestrictedChats      []RestrictedChat `json:"restricted_chats,omitempty"`
	AuthOK               bool             `json:"auth_ok"`
	AuthRefreshExpiresIn int64            `json:"auth_refresh_expires_in_secs,omitempty"`
	AuthWarning          string           `json:"auth_warning,omitempty"`
}

// EncodeLine 输出单行 JSON + 换行，不转义 HTML（对齐 jq 输出，golden 做字节断言）。
func EncodeLine(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // 输出结构均为本包类型，编码失败属编程错误
	}
	return buf.Bytes()
}

// encodeCompact 是 EncodeLine 去掉末尾换行的紧凑单行 JSON（卡片/API payload 用）。
func encodeCompact(v any) string {
	return strings.TrimSpace(string(EncodeLine(v)))
}

// Paths 汇总状态与配置目录，环境变量与 bash 版兼容。
type Paths struct {
	StateDir  string
	ConfigDir string
}

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	p := Paths{
		StateDir:  filepath.Join(home, ".local", "state", "lark-watch"),
		ConfigDir: filepath.Join(home, ".config", "lark-watch"),
	}
	if v := os.Getenv("LW_STATE_DIR"); v != "" {
		p.StateDir = v
	}
	if v := os.Getenv("LW_CONFIG_DIR"); v != "" {
		p.ConfigDir = v
	}
	return p
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func SeenMax() int    { return envInt("LW_SEEN_MAX", 5000) }
func HandledMax() int { return envInt("LW_HANDLED_MAX", 1000) }
func MaxGap() int64   { return int64(envInt("LW_MAX_GAP", 900)) }

// truncateRunes 按码点截断（对齐 jq 的 .[0:n] 语义，不能按字节截）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
