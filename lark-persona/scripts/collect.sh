#!/usr/bin/env bash
# lark-persona collect — 飞书聊天记录本地归档器（lark-cli user 身份，只读）
#
# 用法:
#   collect.sh chats                       # 枚举会话元数据 → archive/chats.ndjson
#   collect.sh my-groups [--months 6]      # 我近 N 月发过言的群 cid 列表（messages-search）
#   collect.sh collect [--months 6] [--cid oc_x] [--p2p-only|--groups-only]
#   collect.sh trim                        # 纯管线: +chat-messages-list 响应 → 归档 NDJSON
#   collect.sh coverage                    # 归档覆盖报告（每会话最早归档月/条数）
#
# 归档布局（LP_DATA_DIR 可覆盖，默认 ~/.local/share/lark-persona）:
#   archive/chats.ndjson             会话元数据（每次 collect 重建）
#   archive/msgs/<chat_id>/YYYY-MM.ndjson   月度消息（升序）；文件存在=该月已采完
# 断点续传: 过去月按文件存在跳过，当前月每次重拉覆盖。诊断走 stderr。
set -uo pipefail

DATA_DIR="${LP_DATA_DIR:-$HOME/.local/share/lark-persona}"
STATE_DIR="${LP_STATE_DIR:-$HOME/.local/state/lark-persona}"
ARCHIVE="$DATA_DIR/archive"
PAGE_SLEEP="${LP_PAGE_SLEEP:-0.3}"
MAX_PAGES="${LP_MAX_PAGES:-400}"

get_self() { lark-cli auth status 2>/dev/null | jq -r '.identities.user.openId // empty'; }

if date -d @0 +%s >/dev/null 2>&1; then DATE_GNU=1; else DATE_GNU=0; fi

# 过去月文件是否已完整采集：存在 且 落盘时间晚于该数据月（即在月结束后采的）。
# 月中采过的过去月视为不完整（尾部缺失），跨月重跑时自动补拉。
month_done() {
  local mfile="$1" ym="$2" cur="$3" mt
  [ -f "$mfile" ] || return 1
  [ "$ym" = "$cur" ] && return 1
  if [ "$DATE_GNU" = 1 ]; then mt=$(date -d "@$(stat -c %Y "$mfile")" +%Y-%m)
  else mt=$(date -r "$(stat -f %m "$mfile")" +%Y-%m); fi
  [[ "$mt" > "$ym" ]]
}

# 本地时区偏移 "+0800" → "+08:00"
tz_offset() { local z; z=$(date +%z); printf '%s:%s' "${z:0:3}" "${z:3}"; }

# ---------- 纯管线：+chat-messages-list 响应 → 归档 NDJSON（升序，滤已撤回）----------
trim() {
  jq -c '
    .data.messages // []
    | sort_by([.create_time, (.message_position | tonumber? // 0)])
    | .[]
    | select(.deleted != true)
    | { mid: .message_id, cid: .chat_id,
        fid: .sender.id, ftype: .sender.sender_type, from: (.sender.name // null),
        type: .msg_type, t: .create_time,
        pos: (.message_position | tonumber? // 0),
        text: ((.content // "") | .[0:2000]) }'
}

# ---------- 带退避的 lark-cli 调用：失败指数退避 60→600s，auth 失效直接退出 ----------
lark_call() {
  local out fails=0 wait_s errf="$STATE_DIR/last_err"
  while :; do
    if out=$(lark-cli "$@" 2>"$errf"); then
      printf '%s' "$out"
      return 0
    fi
    if grep -qiE 'NeedUserAuthorization|99991663|auth login' "$errf"; then
      echo "[collect] user token 失效，请 lark-cli auth login 后重跑" >&2
      exit 1
    fi
    fails=$((fails + 1))
    wait_s=$((60 * (1 << (fails - 1)))); [ "$wait_s" -gt 600 ] && wait_s=600
    echo "[collect] 调用失败 (#$fails)，退避 ${wait_s}s: $(head -c 200 "$errf" | tr '\n' ' ')" >&2
    sleep "$wait_s"
  done
}

# ---------- 枚举会话元数据 → archive/chats.ndjson ----------
chats_cmd() {
  mkdir -p "$ARCHIVE" "$STATE_DIR"
  local token="" out tmp="$ARCHIVE/chats.ndjson.tmp"
  : >"$tmp"
  while :; do
    local args=(im +chat-list --as user --types p2p,group --page-size 100 --format json)
    [ -n "$token" ] && args+=(--page-token "$token")
    out=$(lark_call "${args[@]}") || exit 1
    printf '%s' "$out" | jq -c '
      .data.chats // [] | .[]
      | { cid: .chat_id, mode: .chat_mode, name: (.name // null),
          owner: (.owner_id // null),
          tt: (.p2p_target_type // null), tid: (.p2p_target_id // null),
          external: (.external // false) }' >>"$tmp"
    token=$(printf '%s' "$out" | jq -r '.data.page_token // empty')
    [ -n "$token" ] || break
    sleep "$PAGE_SLEEP"
  done
  mv "$tmp" "$ARCHIVE/chats.ndjson"
  echo "[collect] chats: $(wc -l <"$ARCHIVE/chats.ndjson" | tr -d ' ') 个会话 → $ARCHIVE/chats.ndjson" >&2
}

# ---------- 月份工具：输出 "YYYY MM" 列表，从 N 个月前到当前月 ----------
month_list() {
  local months="$1" y m i
  y=$(date +%Y); m=$(date +%m); m=${m#0}
  i=$((months))
  while [ "$i" -ge 0 ]; do
    local yy=$y mm=$((m - i))
    while [ "$mm" -le 0 ]; do mm=$((mm + 12)); yy=$((yy - 1)); done
    printf '%04d %02d\n' "$yy" "$mm"
    i=$((i - 1))
  done
}

month_start_iso() { printf '%04d-%02d-01T00:00:00%s' "$1" "${2#0}" "$(tz_offset)"; }

next_month_start_iso() {
  local y="$1" m="${2#0}"
  m=$((m + 1)); [ "$m" -gt 12 ] && { m=1; y=$((y + 1)); }
  month_start_iso "$y" "$m"
}

# ---------- 我近 N 月发过言的群（messages-search sender=self，按月窗）----------
my_groups_cmd() {
  local months=6 self
  while [ $# -gt 0 ]; do case "$1" in
    --months) months="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  mkdir -p "$STATE_DIR"
  self=$(get_self)
  [ -n "$self" ] || { echo "[collect] 无法获取用户身份，请 lark-cli auth login" >&2; exit 1; }
  local y m out
  while read -r y m; do
    out=$(lark_call im +messages-search --as user --sender "$self" --chat-type group \
      --start "$(month_start_iso "$y" "$m")" --end "$(next_month_start_iso "$y" "$m")" \
      --page-size 50 --page-all --page-limit 40 --no-reactions --format json) || exit 1
    printf '%s' "$out" | jq -r '.data.messages // [] | .[].chat_id // empty'
    [ "$(printf '%s' "$out" | jq -r '.data.has_more // false')" = "true" ] \
      && echo "[collect] my-groups $y-$m 命中超 2000 条，群清单可能不全（仅影响发现，不影响归档）" >&2
    sleep "$PAGE_SLEEP"
  done < <(month_list "$months") | sort -u
}

# ---------- 采集单会话单月窗口 → 原子落盘 ----------
fetch_window() {
  local cid="$1" start="$2" end="$3" outfile="$4"
  local token="" out has_more pages=0 tmp="$outfile.tmp"
  : >"$tmp"
  while :; do
    local args=(im +chat-messages-list --as user --chat-id "$cid" \
      --start "$start" --end "$end" --order asc --page-size 50 --no-reactions --format json)
    [ -n "$token" ] && args+=(--page-token "$token")
    out=$(lark_call "${args[@]}") || return 1
    printf '%s' "$out" | trim >>"$tmp"
    has_more=$(printf '%s' "$out" | jq -r '.data.has_more // false')
    token=$(printf '%s' "$out" | jq -r '.data.page_token // empty')
    pages=$((pages + 1))
    if [ "$pages" -ge "$MAX_PAGES" ]; then
      echo "[collect] $cid $start 超过 $MAX_PAGES 页上限，截断（LP_MAX_PAGES 可调）" >&2
      break
    fi
    { [ "$has_more" = "true" ] && [ -n "$token" ]; } || break
    sleep "$PAGE_SLEEP"
  done
  jq -c -s 'sort_by([.t, .pos]) | .[]' "$tmp" >"$outfile.sorted" \
    && mv "$outfile.sorted" "$outfile" && rm -f "$tmp"
}

# ---------- 主采集 ----------
collect_cmd() {
  local months=6 only_cid="" p2p_only=0 groups_only=0
  while [ $# -gt 0 ]; do case "$1" in
    --months) months="$2"; shift 2 ;;
    --cid) only_cid="$2"; shift 2 ;;
    --p2p-only) p2p_only=1; shift ;;
    --groups-only) groups_only=1; shift ;;
    *) shift ;;
  esac; done
  mkdir -p "$ARCHIVE/msgs" "$STATE_DIR"

  local self
  self=$(get_self)
  [ -n "$self" ] || { echo "[collect] 无法获取用户身份，请 lark-cli auth login" >&2; exit 1; }

  local targets
  if [ -n "$only_cid" ]; then
    [ -f "$ARCHIVE/chats.ndjson" ] || chats_cmd
    targets="$only_cid"
  else
    chats_cmd  # 每次全量跑都刷新会话清单（约 7 次调用），新同事/新群才能进目标集
    local p2p="" grps=""
    [ "$groups_only" = 1 ] || p2p=$(jq -r 'select(.mode == "p2p" and .tt == "user") | .cid' "$ARCHIVE/chats.ndjson")
    if [ "$p2p_only" != 1 ]; then
      echo "[collect] 圈定我发过言的群（messages-search，约 $((months + 1)) 次调用）..." >&2
      local mine known
      mine=$(my_groups_cmd --months "$months") || exit 1
      known=$(jq -r 'select(.mode == "group") | .cid' "$ARCHIVE/chats.ndjson")
      grps=$(comm -12 <(printf '%s\n' "$mine" | sort -u) <(printf '%s\n' "$known" | sort -u))
    fi
    targets=$(printf '%s\n%s\n' "$p2p" "$grps" | grep -v '^$' || true)
  fi
  local total_chats
  total_chats=$(printf '%s\n' "$targets" | grep -c . || true)
  echo "[collect] 目标 $total_chats 个会话 × $((months + 1)) 个月窗" >&2

  local cur_month cid y m mfile n idx=0
  cur_month=$(date +%Y-%m)
  while IFS= read -r cid; do
    [ -n "$cid" ] || continue
    idx=$((idx + 1))
    mkdir -p "$ARCHIVE/msgs/$cid"
    while read -r y m; do
      mfile="$ARCHIVE/msgs/$cid/$y-$m.ndjson"
      month_done "$mfile" "$y-$m" "$cur_month" && continue
      fetch_window "$cid" "$(month_start_iso "$y" "$m")" "$(next_month_start_iso "$y" "$m")" "$mfile" || exit 1
      n=$(wc -l <"$mfile" | tr -d ' ')
      [ "$n" -gt 0 ] && echo "[collect] ($idx/$total_chats) $cid $y-$m: $n 条" >&2
      sleep "$PAGE_SLEEP"
    done < <(month_list "$months")
  done <<<"$targets"
  echo "[collect] 完成。运行 coverage 查看归档概况" >&2
}

# ---------- 组织关系 ground truth：通讯录逐人拉 leader/department ----------
# 需 scope: contact:user.department:readonly（+ base）。失败跳过（离职/不可见属预期），
# 不走 lark_call 的无限退避。
org_cmd() {
  mkdir -p "$ARCHIVE" "$STATE_DIR"
  local self
  self=$(get_self)
  [ -n "$self" ] || { echo "[collect] 无法获取用户身份" >&2; exit 1; }
  [ -f "$ARCHIVE/chats.ndjson" ] || { echo "[collect] 缺 chats.ndjson，先跑 chats" >&2; exit 1; }

  fetch_org_user() {
    lark-cli api GET "/open-apis/contact/v3/users/$1?user_id_type=open_id&department_id_type=open_department_id" \
      --as user 2>/dev/null \
      | jq -c 'select(.ok) | .data.user
               | {fid: .open_id, name: (.name // null),
                  leader: (.leader_user_id // null), depts: (.department_ids // [])}'
  }

  local tmp="$ARCHIVE/org.ndjson.tmp" fetched=$'\n' id row hops=0 skipped=0
  : >"$tmp"
  # 1) 自己 + 向上汇报链（≤6 跳）
  id="$self"
  while [ -n "$id" ] && [ "$hops" -le 6 ]; do
    case "$fetched" in *$'\n'"$id"$'\n'*) break ;; esac
    row=$(fetch_org_user "$id")
    [ -n "$row" ] || break
    printf '%s\n' "$row" >>"$tmp"
    fetched="${fetched}${id}"$'\n'
    id=$(jq -r '.leader // empty' <<<"$row")
    hops=$((hops + 1))
    sleep "$PAGE_SLEEP"
  done
  # 2) 全部 p2p 真人对端
  while IFS= read -r id; do
    case "$fetched" in *$'\n'"$id"$'\n'*) continue ;; esac
    row=$(fetch_org_user "$id")
    if [ -n "$row" ]; then printf '%s\n' "$row" >>"$tmp"; else skipped=$((skipped + 1)); fi
    fetched="${fetched}${id}"$'\n'
    sleep "$PAGE_SLEEP"
  done < <(jq -r 'select(.mode == "p2p" and .tt == "user") | .tid' "$ARCHIVE/chats.ndjson" | sort -u)
  mv "$tmp" "$ARCHIVE/org.ndjson"
  echo "[collect] org: $(wc -l <"$ARCHIVE/org.ndjson" | tr -d ' ') 人（跳过 $skipped 不可见/离职）→ $ARCHIVE/org.ndjson" >&2
}

# ---------- 覆盖报告 ----------
coverage_cmd() {
  [ -d "$ARCHIVE/msgs" ] || { echo "[collect] 尚无归档" >&2; exit 1; }
  local cid dir first total name
  printf '%-42s %-10s %8s  %s\n' "chat_id" "最早月" "条数" "名称"
  for dir in "$ARCHIVE/msgs"/*/; do
    cid=$(basename "$dir")
    first=$(find "$dir" -name '*.ndjson' -size +0c 2>/dev/null | sort | head -1 | xargs -I{} basename {} .ndjson)
    total=$(cat "$dir"/*.ndjson 2>/dev/null | wc -l | tr -d ' ')
    name=$(jq -r --arg c "$cid" 'select(.cid == $c) | .name // .tid // ""' "$ARCHIVE/chats.ndjson" 2>/dev/null | head -1)
    printf '%-42s %-10s %8s  %s\n' "$cid" "${first:--}" "$total" "$name"
  done
}

case "${1:-}" in
  chats) shift; chats_cmd "$@" ;;
  my-groups) shift; my_groups_cmd "$@" ;;
  collect) shift; collect_cmd "$@" ;;
  trim) shift; trim ;;
  org) shift; org_cmd "$@" ;;
  coverage) shift; coverage_cmd "$@" ;;
  month-list) shift; month_list "$@" ;;
  month-done) shift; month_done "$@" ;;
  *) echo "usage: collect.sh chats|my-groups|collect|trim|org|coverage ..." >&2; exit 2 ;;
esac
