package watch

import "time"

// 头像缓存 TTL：头像更换低频，正缓存 7 天；失败负缓存空 url 1 小时——
// 避免每条通知重复付一次 lark-cli exec 延迟。
const (
	avatarTTL    = int64(7 * 86400)
	avatarNegTTL = int64(3600)
)

// avatarResolver 解析通知横幅图标（飞书头像 URL）：群聊取群头像（key=cid）、
// 私聊取对方头像（key=fid，p2p 发送者即对方）。feishucdn URL 匿名可拉，
// alerter 自行下载渲染；坏 URL 不影响横幅投递。解析失败返回空串
// （横幅无图标，不阻断通知）。
type avatarResolver struct {
	CLI   LarkCLI
	Store *Store
	Now   func() int64 // 测试注入；默认 time.Now
}

func (r *avatarResolver) now() int64 {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().Unix()
}

// Resolve 返回批次的图标 URL，取 batch[0]（与 batchNotifyEnv 取 first 一致）。
func (r *avatarResolver) Resolve(batch []Message) string {
	return r.resolve(batch[0])
}

// resolve 单消息解析：缓存命中且未过 TTL 直接返回；否则现拉并落盘
// （拉取失败落空串负缓存）。key 无前缀——oc_/ou_ 前缀天然不撞。
func (r *avatarResolver) resolve(m Message) string {
	key, fetch := m.Cid, r.CLI.ChatAvatar
	if m.Ctype == "p2p" {
		key, fetch = m.Fid, r.CLI.UserAvatar
	}
	if key == "" {
		return ""
	}
	if url, at, ok := r.Store.AvatarGet(key); ok {
		ttl := avatarTTL
		if url == "" {
			ttl = avatarNegTTL
		}
		if r.now()-at < ttl {
			return url
		}
	}
	url, err := fetch(key)
	if err != nil {
		logf("avatar fetch failed for %s: %v", key, err)
		url = "" // 负缓存
	}
	r.Store.AvatarSet(key, url, r.now())
	return url
}
