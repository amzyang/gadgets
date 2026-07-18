package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// LarkCLI 是对 lark-cli 二进制的唯一边界（鉴权/token 刷新/渲染全部委托给它）。
// 测试注入 fake 实现。
type LarkCLI interface {
	AuthSelf() (AuthInfo, error)
	Search(start, end string) ([]byte, error)
	ChatList() ([]ChatMeta, error)
	ChatMessages(cid, start string) ([]byte, error)
	ReplyAsUser(mid, draft, format string) error
	SendTextAsBot(userID, text string) error
	SendCardToUser(userID, cardJSON string) error
	UpdateCard(token, cardJSON string) error
	EventConsumeCmd(ctx context.Context) *exec.Cmd
}

// messages-search 的实现边界：单次调用最多 searchMaxPages 页 × searchPageSize 条，
// API 回溯窗口上限 searchMaxLookbackDays 天。catchup 的上限提示由此推导。
const (
	searchMaxLookbackDays = 7
	searchPageSize        = 50
	searchMaxPages        = 40
)

// ExecError 携带 stderr 片段，供上层做错误分类（如 NeedUserAuthorization）。
type ExecError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("lark-cli %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *ExecError) Unwrap() error { return e.Err }

// IsAuthError 识别 user token 失效类错误（对齐 bash 版的 stderr 匹配）。
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range []string{"NeedUserAuthorization", "99991663", "auth login"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// IsRestrictedModeError 识别群防泄密模式导致的消息读取禁止（错误码 231203）。
// 限制加在会话级别，与 token/scope 无关，且 search 通道同样被屏蔽——重试无意义。
func IsRestrictedModeError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "231203") || strings.Contains(s, "Restricted Mode")
}

// ExecLarkCLI 通过 exec 调用 lark-cli。
type ExecLarkCLI struct {
	Bin string // 默认 "lark-cli"
}

func (c *ExecLarkCLI) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "lark-cli"
}

// run 执行命令并要求信封 ok==true。
func (c *ExecLarkCLI) run(args ...string) ([]byte, error) {
	cmd := exec.Command(c.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.Bytes()
	if err != nil {
		return out, &ExecError{Args: args, Stderr: truncateRunes(stderr.String()+stdout.String(), 400), Err: err}
	}
	var env struct {
		OK *bool `json:"ok"`
	}
	if jsonErr := json.Unmarshal(out, &env); jsonErr == nil && env.OK != nil && !*env.OK {
		return out, &ExecError{Args: args, Stderr: truncateRunes(string(out), 400), Err: fmt.Errorf("ok=false")}
	}
	return out, nil
}

// AuthInfo 是 `auth status` 里 daemon 关心的子集。
type AuthInfo struct {
	OpenID           string
	RefreshExpiresAt time.Time // 刷新期截止；零值 = CLI 未提供
}

func (c *ExecLarkCLI) AuthSelf() (AuthInfo, error) {
	out, err := c.run("auth", "status")
	if err != nil {
		return AuthInfo{}, err
	}
	return parseAuthStatus(out)
}

// parseAuthStatus 校验 user 身份可用并提取 AuthInfo。
func parseAuthStatus(out []byte) (AuthInfo, error) {
	var st struct {
		Identities struct {
			User struct {
				Available        bool      `json:"available"`
				OpenID           string    `json:"openId"`
				RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
			} `json:"user"`
		} `json:"identities"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return AuthInfo{}, err
	}
	u := st.Identities.User
	if !u.Available || u.OpenID == "" {
		return AuthInfo{}, fmt.Errorf("auth status: user identity unavailable, run `lark-cli auth login --domain im,contact`")
	}
	return AuthInfo{OpenID: u.OpenID, RefreshExpiresAt: u.RefreshExpiresAt}, nil
}

func (c *ExecLarkCLI) Search(start, end string) ([]byte, error) {
	return c.run("im", "+messages-search", "--as", "user",
		"--start", start, "--end", end,
		"--exclude-sender-type", "bot", "--no-reactions",
		"--page-size", strconv.Itoa(searchPageSize), "--page-all", "--format", "json")
}

// ChatList 返回当前用户的会话（p2p+群，含免打扰群），按 active_time 降序。
// 免打扰 ≠ 不采集：排除 muted 会让 2/3 会话只能靠低频 search 兜底（分钟级延迟），
// 噪音控制由分级（P1→digest）和 ignore 规则承担。
func (c *ExecLarkCLI) ChatList() ([]ChatMeta, error) {
	out, err := c.run("im", "+chat-list", "--as", "user",
		"--types", "p2p,group", "--sort", "active_time",
		"--page-size", "50", "--format", "json")
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			Chats []struct {
				ChatID   string `json:"chat_id"`
				Name     string `json:"name"`
				ChatMode string `json:"chat_mode"`
			} `json:"chats"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, err
	}
	metas := make([]ChatMeta, 0, len(env.Data.Chats))
	for _, ch := range env.Data.Chats {
		metas = append(metas, ChatMeta{Cid: ch.ChatID, Name: ch.Name, Mode: ch.ChatMode})
	}
	return metas, nil
}

// ChatMessages 拉取单会话自 start（ISO 8601）以来的消息，升序。
func (c *ExecLarkCLI) ChatMessages(cid, start string) ([]byte, error) {
	return c.run("im", "+chat-messages-list", "--as", "user",
		"--chat-id", cid, "--start", start, "--order", "asc",
		"--no-reactions", "--page-size", "50", "--format", "json")
}

// replyArgs 构造回复 argv：format=="markdown" 走 --markdown
// （lark-cli 自动包装为 post 富文本），否则 --text 纯文本。
func replyArgs(mid, draft, format string) []string {
	flag := "--text"
	if format == "markdown" {
		flag = "--markdown"
	}
	return []string{"im", "+messages-reply", "--message-id", mid, "--as", "user",
		"--idempotency-key", mid, flag, draft}
}

func (c *ExecLarkCLI) ReplyAsUser(mid, draft, format string) error {
	_, err := c.run(replyArgs(mid, draft, format)...)
	return err
}

func (c *ExecLarkCLI) SendTextAsBot(userID, text string) error {
	_, err := c.run("im", "+messages-send", "--user-id", userID, "--as", "bot", "--text", text)
	return err
}

func (c *ExecLarkCLI) SendCardToUser(userID, cardJSON string) error {
	_, err := c.run("im", "+messages-send", "--user-id", userID, "--as", "bot",
		"--msg-type", "interactive", "--content", cardJSON)
	return err
}

func (c *ExecLarkCLI) UpdateCard(token, cardJSON string) error {
	payload := encodeCompact(struct {
		Token string          `json:"token"`
		Card  json.RawMessage `json:"card"`
	}{Token: token, Card: json.RawMessage(cardJSON)})
	_, err := c.run("api", "POST", "/open-apis/interactive/v1/card/update", "--as", "bot",
		"--data", payload)
	return err
}

// EventConsumeCmd 构造卡片回调 consume 子进程命令（binary 与 argv 归本边界所有，
// 进程监督在 run.go）。
func (c *ExecLarkCLI) EventConsumeCmd(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, c.bin(), "event", "consume", "card.action.trigger", "--as", "bot")
}
