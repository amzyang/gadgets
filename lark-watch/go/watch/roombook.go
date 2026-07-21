package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// BookSlot 是预约意向卡的一个候选时段（room book 的 -d/-t 参数形态）。
type BookSlot struct {
	Date string `json:"date"` // MM-DD
	Time string `json:"time"` // HH:MM-HH:MM
}

// BookResult 是预订成功的关键信息（room book 成功信封 data 子集）。
type BookResult struct {
	Room    string
	Date    string
	Start   string
	End     string
	EventID string // 日历日程 ID（room cancel 用）
}

// BookError 携带 room CLI 错误信封的分类信息，供改卡文案与 book-failed 事件消费。
type BookError struct {
	Type    string // no_room / conflict / holiday_skipped / no_participants / auth / validation / …
	Message string
	Hint    string
}

func (e *BookError) Error() string {
	return fmt.Sprintf("room book failed (%s): %s", e.Type, e.Message)
}

// RoomBooker 是对 room CLI 的唯一边界（会议室预订）。测试注入 fake。
type RoomBooker interface {
	Book(ctx context.Context, slot BookSlot, title string, participants []string) (BookResult, error)
}

// ExecRoomBooker 通过 exec 调用 room CLI（LW_ROOM_BIN 覆盖，默认 PATH 上的 room）。
type ExecRoomBooker struct {
	Bin string
}

func (b *ExecRoomBooker) bin() string {
	if b.Bin != "" {
		return b.Bin
	}
	if v := os.Getenv("LW_ROOM_BIN"); v != "" {
		return v
	}
	return "room"
}

const roomBookTimeout = 60 * time.Second

// Book 执行 room book --json（固定 argv 绝不过 shell，参数来自本地 SQLite）。
// 契约见 ~/.claude/skills/room/SKILL.md：exit 0 ⟺ 订上且 stdout 是成功信封；
// 失败时 stderr 最后一行是错误信封。返回的 error 恒为 *BookError。
// 调用与结果经 logCmd 留痕。
func (b *ExecRoomBooker) Book(ctx context.Context, slot BookSlot, title string, participants []string) (res BookResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, roomBookTimeout)
	defer cancel()
	args := []string{"book", "-d", slot.Date, "-t", slot.Time, "--title", title}
	for _, p := range participants {
		args = append(args, "-p", p)
	}
	args = append(args, "--json")
	cmd := exec.CommandContext(ctx, b.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	defer func() { logCmd(b.bin(), args, time.Since(start), stdout.Len(), err) }()
	if runErr := cmd.Run(); runErr != nil {
		return BookResult{}, parseBookError(stderr.Bytes(), runErr)
	}
	return parseBookSuccess(stdout.Bytes())
}

// parseBookSuccess 解析成功信封。exit 0 但解析不出时不误报「已预订成功细节」，
// 也提示用户核对（预订大概率已生效，重订会被 conflict 挡住，不致双订）。
func parseBookSuccess(out []byte) (BookResult, error) {
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			EventID   string `json:"event_id"`
			Date      string `json:"date"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Room      struct {
				Name string `json:"name"`
			} `json:"room"`
		} `json:"data"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &env); err != nil || !env.OK {
		return BookResult{}, &BookError{Type: "parse",
			Message: "room book 退出成功但输出无法解析（预订可能已生效）",
			Hint:    "用 room list --json 核对后再决定是否重订"}
	}
	d := env.Data
	return BookResult{Room: d.Room.Name, Date: d.Date, Start: d.StartTime, End: d.EndTime, EventID: d.EventID}, nil
}

// parseBookError 提取 stderr 最后一行的错误信封；非 JSON（flag 解析在 --json
// 生效前失败、room 未安装）降级为原始错误文本。
func parseBookError(stderrBuf []byte, runErr error) *BookError {
	lines := bytes.Split(bytes.TrimSpace(stderrBuf), []byte("\n"))
	last := bytes.TrimSpace(lines[len(lines)-1])
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if json.Unmarshal(last, &env) == nil && env.Error.Type != "" {
		return &BookError{Type: env.Error.Type, Message: env.Error.Message, Hint: env.Error.Hint}
	}
	return &BookError{Type: "exec", Message: runErr.Error(), Hint: truncateRunes(string(last), 200)}
}
