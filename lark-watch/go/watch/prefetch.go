package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Resource.Kind 取值。
const (
	ResDoc   = "doc"
	ResImage = "image"
	ResFile  = "file"
)

// detectMaxPerMessage 是单条消息的资源检出上限；下载侧另有更紧的
// p0MaxResources/digestResPerChat 截断。
const detectMaxPerMessage = 5

// digestResPerChat 是 digest 每会话（peek 消息）携带并预取的资源上限。
const digestResPerChat = 2

// docURLRe 识别正文里的飞书云文档链接（docx/wiki，含查询串与 #share- 锚点；
// 排除引号/括号等定界符，markdown 链接与卡片 JSON 内嵌 URL 都能干净截出）。
// 其余云资源类型（sheets/base/minutes）不预取，仍走模型手动流程。
var docURLRe = regexp.MustCompile(`https?://[A-Za-z0-9.-]+\.(?:feishu\.cn|larksuite\.com)/(?:docx|wiki)/[A-Za-z0-9]+[-A-Za-z0-9._~/?#&=%+]*`)

// resKeyRe 校验图片/文件 key 形状：真实飞书 key 形如 img_v3_.../file_v3_...，
// 只含字母数字下划线连字符。正文可含任意 `[Image: …]` 文本，伪造 key 会被
// 当作 spool 目录名（download 里 filepath.Join(SpoolDir, "res", mid, ref)），
// 校验挡住其中的 `/`、`..` 等路径穿越分量。
var resKeyRe = regexp.MustCompile(`^(?:img|file)_[A-Za-z0-9_-]+$`)

// DetectResources 从消息原始 content（截断前全量，见 toMessage）识别可预取
// 资源：云文档链接、图片 key、文件附件 key。纯函数，下载 IO 在 Prefetcher。
// media/audio/sticker 整条跳过（模型无法消费，下载纯浪费）；(kind,ref) 去重，
// 上限 detectMaxPerMessage，顺序 doc → image → file（文档信息量最大，下游
// 截断时优先保留）。
func DetectResources(mid, msgType, content string) []Resource {
	switch msgType {
	case "media", "audio", "sticker":
		return nil
	}
	var out []Resource
	seen := map[string]bool{}
	add := func(kind, ref, name string) {
		if ref == "" || len(out) >= detectMaxPerMessage || seen[kind+":"+ref] {
			return
		}
		if kind != ResDoc && !resKeyRe.MatchString(ref) {
			return // 图片/文件 ref 须是飞书 key 形状（挡路径穿越、省无效 CLI 调用）
		}
		seen[kind+":"+ref] = true
		out = append(out, Resource{Kind: kind, Ref: ref, Name: name, Mid: mid})
	}
	for _, u := range docURLRe.FindAllString(content, -1) {
		add(ResDoc, u, "")
	}
	for _, m := range imgBracketRe.FindAllStringSubmatch(content, -1) {
		add(ResImage, m[1], "")
	}
	for _, m := range imgMarkdownRe.FindAllStringSubmatch(content, -1) {
		add(ResImage, m[1], "")
	}
	for _, m := range fileTagRe.FindAllStringSubmatch(content, -1) {
		add(ResFile, m[1], m[2])
	}
	return out
}

// mergeResources 合并聚合组内各条消息的资源：最新消息优先（倒序遍历）、
// (kind,ref) 去重、截 max。空输入保持 nil（omitempty 字节稳定）。
func mergeResources(groups [][]Resource, max int) []Resource {
	var out []Resource
	seen := map[string]bool{}
	for i := len(groups) - 1; i >= 0; i-- {
		for _, r := range groups[i] {
			if len(out) >= max {
				return out
			}
			if seen[r.Kind+":"+r.Ref] {
				continue
			}
			seen[r.Kind+":"+r.Ref] = true
			out = append(out, r)
		}
	}
	return out
}

// capResources 截取前 max 个资源（nil 安全，不改原切片）。
func capResources(rs []Resource, max int) []Resource {
	if len(rs) <= max {
		return rs
	}
	return rs[:max:max]
}

// ---------- 预取 IO 外壳 ----------

// 预取参数（对齐 envInt/LW_* 风格）。budget 是一批（一次 P0 组 / 一次 digest
// flush）的共享预算，由调用方以 ctx 承载。
func prefetchEnabled() bool { return os.Getenv("LW_PREFETCH") != "0" }
func prefetchTimeout() time.Duration {
	return time.Duration(envInt("LW_PREFETCH_TIMEOUT", 15)) * time.Second
}
func prefetchBudget() time.Duration {
	return time.Duration(envInt("LW_PREFETCH_BUDGET", 45)) * time.Second
}

// 内联截断（码点）：P0 事件资源多留（细判/起草直接用），digest 一次带多个
// 会话从紧；全文都在 Path，超出部分模型按需 Read。
const (
	p0InlineMax     = 2000
	digestInlineMax = 500
)

// docCacheTTL：文档会演化，命中超龄即重取；图片/文件按 file_key 内容不变，
// 产物存在即命中、不设 TTL。
const docCacheTTL = int64(24 * 3600)

// spoolMaxAge 是产物清扫年龄（秒），缓存行随产物同龄删除。
const spoolMaxAge = int64(7 * 24 * 3600)

// digestPrefetchChats 是一次 digest flush 参与预取的会话数上限（热度序前 N）。
const digestPrefetchChats = 5

// fileExtOK 是文件附件预取白名单（模型可消费的类型；对齐 content-fetch.md
// 「pdf/图片/纯文本直接看，zip/安装包/音视频不下载」）。
var fileExtOK = map[string]bool{
	".pdf": true, ".txt": true, ".md": true, ".markdown": true, ".csv": true, ".tsv": true,
	".log": true, ".json": true, ".ndjson": true, ".xml": true, ".yaml": true, ".yml": true,
	".toml": true, ".ini": true, ".conf": true, ".sql": true, ".diff": true, ".patch": true,
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".java": true, ".kt": true, ".rb": true, ".rs": true, ".c": true, ".h": true,
	".cpp": true, ".cc": true, ".sh": true, ".fish": true, ".html": true, ".css": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true,
	".heic": true, ".svg": true,
}

// fileConsumable 按扩展名判断模型能否消费。name 为空视为未知、放行
// （服务端文件名下载后再补校，见 download）。
func fileConsumable(name string) bool {
	if name == "" {
		return true
	}
	return fileExtOK[strings.ToLower(filepath.Ext(name))]
}

var docTokenRe = regexp.MustCompile(`/(?:docx|wiki)/([A-Za-z0-9]+)`)

// docToken 从文档 URL 提取 token（缓存键与落盘文件名）。
func docToken(ref string) string {
	if m := docTokenRe.FindStringSubmatch(ref); m != nil {
		return m[1]
	}
	return ref
}

// docCacheKey 是文档的缓存键：锚点/查询串归一掉，同文档不同锚点共享缓存
// （锚点只是选区提示，预取按全文获取与落盘，覆盖一切锚点）。
func docCacheKey(ref string) string { return "doc:" + docToken(ref) }

// docFetchRef 去掉查询串与锚点（带 #share- 锚点会触发 CLI 局部读取，缓存的
// 必须是全文）。
func docFetchRef(ref string) string {
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		return ref[:i]
	}
	return ref
}

// Prefetcher 是资源预取的 IO 外壳：DetectResources 检出的引用经它填充实际
// 内容，产物落 SpoolDir 并记入 res_cache（跨消息复用）。一切失败吞入
// Resource.Err——预取错误绝不冒泡，IsAuthError 冒泡会杀掉 poller。
type Prefetcher struct {
	CLI      LarkCLI
	Store    *Store // res_cache 缓存关联
	SpoolDir string
	Now      func() int64 // 测试注入；默认 time.Now
}

func (pf *Prefetcher) now() int64 {
	if pf.Now != nil {
		return pf.Now()
	}
	return time.Now().Unix()
}

// Fetch 就地填充一批资源。单资源超时 prefetchTimeout；批预算由调用方 ctx
// 承载，ctx 已取消/超预算的资源标 err 后原样发出（模型走手动回退）。
func (pf *Prefetcher) Fetch(ctx context.Context, rs []Resource, inlineMax int) {
	if len(rs) == 0 {
		return
	}
	start := time.Now()
	var ok, hit, fail int
	for i := range rs {
		h, err := pf.fetchOne(ctx, &rs[i], inlineMax)
		switch {
		case err != nil:
			rs[i].Err = truncateRunes(err.Error(), 200)
			fail++
		case h:
			hit++
			ok++
		default:
			ok++
		}
	}
	evlog.Info("prefetch.done", "n", len(rs), "ok", ok, "hit", hit, "fail", fail,
		"ms", time.Since(start).Milliseconds())
}

func (pf *Prefetcher) fetchOne(ctx context.Context, r *Resource, inlineMax int) (cacheHit bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("budget: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, prefetchTimeout())
	defer cancel()
	switch r.Kind {
	case ResDoc:
		return pf.fetchDoc(cctx, r, inlineMax)
	case ResImage, ResFile:
		return pf.download(cctx, r)
	default:
		return false, fmt.Errorf("unknown resource kind %q", r.Kind)
	}
}

// fetchDoc 获取云文档全文：命中缓存（TTL 内且产物在）读盘复用，未命中
// docs +fetch 后全文落盘、内联截断、写缓存行。
func (pf *Prefetcher) fetchDoc(ctx context.Context, r *Resource, inlineMax int) (bool, error) {
	key := docCacheKey(r.Ref)
	now := pf.now()
	if e, ok := pf.Store.ResCacheGet(key); ok && now-e.FetchedAt < docCacheTTL {
		if b, err := os.ReadFile(e.Path); err == nil {
			r.Name, r.Path, r.Content = e.Name, e.Path, truncateRunes(string(b), inlineMax)
			return true, nil
		}
		// 产物被清扫/丢失：视为 miss 重取
	}
	out, err := pf.CLI.DocsFetch(ctx, docFetchRef(r.Ref))
	if err != nil {
		return false, err
	}
	title, content, err := parseDocsFetch(out)
	if err != nil {
		return false, err
	}
	dir := filepath.Join(pf.SpoolDir, "docs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	path := filepath.Join(dir, docToken(r.Ref)+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, err
	}
	r.Name, r.Path, r.Content = title, path, truncateRunes(content, inlineMax)
	if err := pf.Store.ResCachePut(ResCacheEntry{Ref: key, Kind: ResDoc, Name: title, Path: path, FetchedAt: now}); err != nil {
		logf("res_cache put failed: %v", err)
	}
	return false, nil
}

// download 下载图片/文件：file_key 内容不变，缓存命中且产物在即复用；文件
// 附件先过扩展名白名单（无 name 属性的下载后按服务端文件名补校）。
func (pf *Prefetcher) download(ctx context.Context, r *Resource) (bool, error) {
	if r.Kind == ResFile && r.Name != "" && !fileConsumable(r.Name) {
		return false, fmt.Errorf("skip: 类型不可消费 %s", r.Name)
	}
	key := r.Kind + ":" + r.Ref
	if e, ok := pf.Store.ResCacheGet(key); ok {
		if _, err := os.Stat(e.Path); err == nil {
			r.Name, r.Path = e.Name, e.Path
			return true, nil
		}
	}
	dir := filepath.Join(pf.SpoolDir, "res", r.Mid, r.Ref)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	name, err := pf.CLI.ResourceDownload(ctx, r.Mid, r.Ref, r.Kind, dir)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, name)
	if r.Kind == ResFile && r.Name == "" && !fileConsumable(name) {
		os.Remove(path)
		return false, fmt.Errorf("skip: 类型不可消费 %s", name)
	}
	if r.Name == "" {
		r.Name = name
	}
	r.Path = path
	if err := pf.Store.ResCachePut(ResCacheEntry{Ref: key, Kind: r.Kind, Name: r.Name, Path: path, FetchedAt: pf.now()}); err != nil {
		logf("res_cache put failed: %v", err)
	}
	return false, nil
}

// Sweep 清扫超龄 spool 产物（docs/ 按文件、res/ 按 mid 目录）与同龄缓存行。
// best-effort：单项失败跳过，不阻塞轮询。
func (pf *Prefetcher) Sweep(now int64) {
	cutoff := time.Unix(now-spoolMaxAge, 0)
	removed := 0
	sweep := func(dir string, remove func(string) error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if remove(filepath.Join(dir, e.Name())) == nil {
				removed++
			}
		}
	}
	sweep(filepath.Join(pf.SpoolDir, "docs"), os.Remove)
	sweep(filepath.Join(pf.SpoolDir, "res"), os.RemoveAll)
	if err := pf.Store.ResCacheSweep(now - spoolMaxAge); err != nil {
		logf("res_cache sweep failed: %v", err)
	}
	if removed > 0 {
		evlog.Info("prefetch.sweep", "removed", removed)
	}
}
