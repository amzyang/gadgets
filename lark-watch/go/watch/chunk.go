package watch

import "bytes"

// Chunk 是传输层分片事件（p:"chunk"）：Monitor 对 stdout 每行按 500 Unicode
// 字符截断，单行超限的事件按宽度切段逐段包装，消费方按 seq 升序拼接 data
// 还原原事件 JSON 再照常处理。分片由 emitLines 单次写出，物理连续、不与
// 其他事件交错。
type Chunk struct {
	P    string `json:"p"`
	Seq  int    `json:"seq"` // 1..Of
	Of   int    `json:"of"`
	Data string `json:"data"` // 原事件 JSON 的片段
}

// chunkLineMax 是 stdout 单行宽度上限（Monitor 每行 500 字符截断，留 20 余量）。
// LW_CHUNK_MAX 仅供端到端验证压低阈值，生产勿动（<51 时 data 预算耗尽）。
func chunkLineMax() int { return envInt("LW_CHUNK_MAX", 480) }

// chunkOverhead 是 Chunk 包装骨架的宽度预算：{"p":"chunk","seq":,"of":,"data":""}
// 固定 36 字符 + seq/of 位数余量。
const chunkOverhead = 50

// escapedWidth 是 rune 编码进 data JSON 字符串后的宽度：引号/反斜杠膨胀为
// 两字符（EncodeLine 输出无裸控制字符，SetEscapeHTML(false) 下无其他膨胀）；
// BMP 外字符按 2 计，兼容 Monitor 按 UTF-16 计数的可能。
func escapedWidth(r rune) int {
	if r == '"' || r == '\\' || r > 0xFFFF {
		return 2
	}
	return 1
}

// lineWidth 是整行的直通宽度（无二次转义，仅 BMP 外字符按 2 计）。
func lineWidth(s string) int {
	w := 0
	for _, r := range s {
		if r > 0xFFFF {
			w += 2
		} else {
			w++
		}
	}
	return w
}

// splitEscaped 按转义感知宽度贪心切段：每段编码进 data 后不超 budget，按
// rune 边界切、不裂 UTF-8。段尾可落在原 JSON 转义序列中间（如孤立 \）——
// 每个分片行自身仍是合法 JSON，消费方拼接完才解析，无碍。
func splitEscaped(s string, budget int) []string {
	var segs []string
	start, w := 0, 0
	for i, r := range s {
		rw := escapedWidth(r)
		if w+rw > budget {
			segs = append(segs, s[start:i])
			start, w = i, 0
		}
		w += rw
	}
	return append(segs, s[start:])
}

// EncodeLines 编码 v 为 stdout 行集：宽度在上限内原样单行（与 EncodeLine
// 字节一致）；超限拆为连续多行 chunk 分片。
func EncodeLines(v any) [][]byte {
	line := EncodeLine(v)
	s := string(line[:len(line)-1])
	if lineWidth(s) <= chunkLineMax() {
		return [][]byte{line}
	}
	segs := splitEscaped(s, chunkLineMax()-chunkOverhead)
	out := make([][]byte, 0, len(segs))
	for i, seg := range segs {
		out = append(out, EncodeLine(Chunk{P: "chunk", Seq: i + 1, Of: len(segs), Data: seg}))
	}
	return out
}

// emitLines 编码 v（超长自动分片）后单次写出——分片行在 stdout 物理连续，
// 不被并发 emit 交错——并留痕事件日志；分片时补一条 emit.chunked 审计。
func emitLines(out func([]byte), v any) {
	lines := EncodeLines(v)
	out(bytes.Join(lines, nil))
	logEmit(v)
	if len(lines) > 1 {
		evlog.Info("emit.chunked", "lines", len(lines))
	}
}
