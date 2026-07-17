#!/usr/bin/env bash
# lark-persona graph — 从归档统计关系图谱（纯 jq，零 LLM、零 API）
#
# 用法:
#   graph.sh p2p-stats --self ou_x --peer ou_y [--name 张三] [--gap-min 240]
#       stdin: 单个 p2p 会话归档 NDJSON（升序）→ stdout: 单行统计 JSON
#   graph.sh group-stats --self ou_x --cid oc_z [--owner ou_o] [--name 群名]
#       stdin: 单个群归档 NDJSON → stdout: 每联系人一行统计 JSON
#   graph.sh aggregate            # stdin: 上两者输出合流 → contacts JSON 数组
#   graph.sh report [--top 20]    # stdin: contacts JSON → Markdown 草表
#   graph.sh run [--top 20]       # 全档案编排：读 archive/ → evidence/contacts.json + report.md
#
# 上下级推断只产「草表 + 证据」，准绳是人工校正的 seeds.yaml（LLM 蒸馏阶段消费）。
set -uo pipefail

DATA_DIR="${LP_DATA_DIR:-$HOME/.local/share/lark-persona}"
ARCHIVE="$DATA_DIR/archive"

# 短确认（收到/好的类，≤16 字符全句匹配）与指派语气（弱信号，仅作证据）
ACK_RE='^[[:space:]]*(收到|好的|好嘞|好滴|好|嗯+|哦+|OK|ok|Ok|okay|👌|明白|了解|已完成|已处理|没问题|可以|行|是的|对的?|辛苦了?|谢谢|多谢)[[:space:]!！。.~～👌🙏]*$'
DIR_RE='(帮我|麻烦|尽快|优先|安排一下|跟进一下|同步一下|今天之?内|明天之?前|发我|处理一下|支持一下|拉个群)'

# ---------- p2p 单会话统计 ----------
p2p_stats() {
  local self="" peer="" name="" gap=240
  while [ $# -gt 0 ]; do case "$1" in
    --self) self="$2"; shift 2 ;;
    --peer) peer="$2"; shift 2 ;;
    --name) name="$2"; shift 2 ;;
    --gap-min) gap="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  jq -c -n --arg self "$self" --arg peer "$peer" --arg name "$name" \
        --argjson gap "$gap" --arg ack "$ACK_RE" --arg dir "$DIR_RE" '
    def median: sort
      | if length == 0 then null
        elif length % 2 == 1 then .[length / 2 | floor]
        else (.[length / 2 - 1] + .[length / 2]) / 2 end;
    def is_ack: ((.text | length) <= 16) and (.text | test($ack));
    def is_dir: .text | test($dir);

    [inputs | select(.ftype == "user") | select((.text // "") != "")]
    | map(. + {e: (.t | strptime("%Y-%m-%d %H:%M") | mktime)}) as $m
    | if ($m | length) == 0 then empty else
        ($gap * 60) as $gs
        | [range(1; $m | length) | {cur: $m[.], prev: $m[. - 1]}] as $pairs
        | ([$m[0]] + [$pairs[] | select(.cur.e - .prev.e > $gs) | .cur]) as $inits
        | [$pairs[] | select(.cur.fid != .prev.fid and (.cur.e - .prev.e) <= $gs)
            | {who: .cur.fid, lat: ((.cur.e - .prev.e) / 60)}] as $resp
        | { kind: "p2p", peer: $peer,
            name: (if $name != "" then $name
                   else ([$m[] | select(.fid == $peer) | .from | select(. != null)] | last) end),
            n_me: ([$m[] | select(.fid == $self)] | length),
            n_peer: ([$m[] | select(.fid == $peer)] | length),
            init_me: ([$inits[] | select(.fid == $self)] | length),
            init_peer: ([$inits[] | select(.fid == $peer)] | length),
            resp_med_me: ([$resp[] | select(.who == $self) | .lat] | median),
            resp_med_peer: ([$resp[] | select(.who == $peer) | .lat] | median),
            ack_me: ([$m[] | select(.fid == $self) | select(is_ack)] | length),
            ack_peer: ([$m[] | select(.fid == $peer) | select(is_ack)] | length),
            dir_me: ([$m[] | select(.fid == $self) | select(is_dir)] | length),
            dir_peer: ([$m[] | select(.fid == $peer) | select(is_dir)] | length),
            first_t: $m[0].t, last_t: $m[-1].t }
      end'
}

# ---------- 群单会话统计（按联系人展开）----------
group_stats() {
  local self="" cid="" owner="" name=""
  while [ $# -gt 0 ]; do case "$1" in
    --self) self="$2"; shift 2 ;;
    --cid) cid="$2"; shift 2 ;;
    --owner) owner="$2"; shift 2 ;;
    --name) name="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  jq -c -n --arg self "$self" --arg cid "$cid" --arg owner "$owner" \
        --arg name "$name" --arg dir "$DIR_RE" '
    [inputs | select(.ftype == "user") | select((.text // "") != "")] as $m
    | ($m | map(select(.fid == $self))) as $mine
    | ("<at user_id=\"" + $self + "\"") as $at_self
    | $m
    | map(select(.fid != $self))
    | group_by(.fid)
    | .[]
    | .[0].fid as $p
    | { kind: "group", peer: $p,
        name: ([.[].from | select(. != null)] | last),
        cid: $cid, gname: (if $name != "" then $name else null end),
        g_msgs: length,
        at_me: ([.[] | select(.text | contains($at_self))] | length),
        at_by_me: ([$mine[] | select(.text | contains("<at user_id=\"" + $p + "\""))] | length),
        dir_at_me: ([.[] | select((.text | contains($at_self)) and (.text | test($dir)))] | length),
        owner: ($p == $owner) }'
}

# ---------- 聚合：合并同一联系人跨会话数据 + 上下级信号评分 ----------
aggregate() {
  jq -c -s '
    def nz: . // 0;
    group_by(.peer)
    | map(
        . as $rows
        | ([$rows[] | select(.kind == "p2p")] | first) as $p
        | [$rows[] | select(.kind == "group")] as $g
        | ($g | map(.g_msgs) | add | nz) as $gmsgs
        | { fid: $rows[0].peer,
            name: ([$rows[].name | select(. != null)] | first),
            p2p: $p,
            groups: { n: ($g | length), msgs: $gmsgs,
                      at_me: ($g | map(.at_me) | add | nz),
                      at_by_me: ($g | map(.at_by_me) | add | nz),
                      dir_at_me: ($g | map(.dir_at_me) | add | nz),
                      owned: ([$g[] | select(.owner)] | length),
                      names: [$g[].gname | select(. != null)] },
            total: ((if $p then $p.n_me + $p.n_peer else 0 end) + $gmsgs) }
        | . + { signals: (
            [ (if .p2p and .p2p.ack_me >= 5 and .p2p.ack_me >= 2 * ([.p2p.ack_peer, 1] | max)
                 then {dir: "sup", pts: 2, why: "我对TA高频短确认(\(.p2p.ack_me):\(.p2p.ack_peer))"} else empty end),
              (if .p2p and .p2p.dir_peer >= 3 and .p2p.dir_peer >= 2 * ([.p2p.dir_me, 1] | max)
                 then {dir: "sup", pts: 2, why: "TA对我指派语气多(\(.p2p.dir_peer):\(.p2p.dir_me))"} else empty end),
              (if .groups.dir_at_me >= 3
                 then {dir: "sup", pts: 1, why: "群里TA常@我并带指派(\(.groups.dir_at_me)次)"} else empty end),
              (if .groups.owned >= 1
                 then {dir: "sup", pts: 1, why: "TA是\(.groups.owned)个共同群群主"} else empty end),
              (if .p2p and .p2p.ack_peer >= 5 and .p2p.ack_peer >= 2 * ([.p2p.ack_me, 1] | max)
                 then {dir: "sub", pts: 2, why: "TA对我高频短确认(\(.p2p.ack_peer):\(.p2p.ack_me))"} else empty end),
              (if .p2p and .p2p.dir_me >= 3 and .p2p.dir_me >= 2 * ([.p2p.dir_peer, 1] | max)
                 then {dir: "sub", pts: 2, why: "我对TA指派语气多(\(.p2p.dir_me):\(.p2p.dir_peer))"} else empty end) ]) }
        | ([.signals[] | select(.dir == "sup") | .pts] | add // 0) as $sup
        | ([.signals[] | select(.dir == "sub") | .pts] | add // 0) as $sub
        | . + { guess: (if $sup - $sub >= 3 then "上级候选"
                        elif $sub - $sup >= 3 then "下属候选"
                        else "平级/待定" end) })
    | sort_by(-.total)'
}

# ---------- 通讯录 ground truth 合并：contacts JSON + org.ndjson → 加 relation ----------
org_merge() {
  local self="" org=""
  while [ $# -gt 0 ]; do case "$1" in
    --self) self="$2"; shift 2 ;;
    --org) org="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  jq -c --arg self "$self" --slurpfile orgrows "$org" '
    ($orgrows | INDEX(.fid)) as $byid
    | def chain($id; $n):
        if $id == null or $n <= 0 then []
        else [$id] + chain($byid[$id].leader // null; $n - 1) end;
    chain($byid[$self].leader // null; 6) as $up
    | ($byid[$self].depts // []) as $mydepts
    | map(
        .fid as $f
        | ($byid[$f] // null) as $o
        | (if ($up | length) > 0 and $f == $up[0] then "直属上级"
           elif ([$up[1:][] | select(. == $f)] | length) > 0 then "隔级上级"
           elif $o and $o.leader == $self then "直属下属"
           elif $o and $o.leader != null and $o.leader == ($up[0] // "") then "同组平级"
           elif $o and (($o.depts // []) | any(. as $d | ($mydepts | index($d)) != null)) then "同部门"
           elif $o then "跨部门"
           else null end) as $rel
        | . + (if $rel then {relation: $rel, relation_src: "org"} else {} end))'
}

# ---------- 报告渲染 ----------
report() {
  local top=20
  while [ $# -gt 0 ]; do case "$1" in
    --top) top="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  jq -r --argjson top "$top" '
    def f: if . == null then "-" else (. * 10 | round / 10 | tostring) end;
    "# lark-persona 关系图谱草表",
    "",
    "纯统计推断（本地归档，零 LLM）。请人工校正「上下级推断」后落 `seeds.yaml`。",
    "",
    "## Top \($top) 高频伙伴",
    "",
    "| # | 姓名 | 总量 | p2p 我/TA | 发起 我/TA | 回复中位min 我/TA | 短确认 我/TA | 指派 我/TA | @ 我→TA/TA→我 | 共同群 | 推断 |",
    "|---|------|------|-----------|------------|--------------------|---------------|-------------|----------------|--------|------|",
    (.[:$top] | to_entries[] | .value as $c | .key + 1 as $i |
      "| \($i) | \($c.name // $c.fid) | \($c.total) "
      + "| \(if $c.p2p then "\($c.p2p.n_me)/\($c.p2p.n_peer)" else "-" end) "
      + "| \(if $c.p2p then "\($c.p2p.init_me)/\($c.p2p.init_peer)" else "-" end) "
      + "| \(if $c.p2p then "\($c.p2p.resp_med_me | f)/\($c.p2p.resp_med_peer | f)" else "-" end) "
      + "| \(if $c.p2p then "\($c.p2p.ack_me)/\($c.p2p.ack_peer)" else "-" end) "
      + "| \(if $c.p2p then "\($c.p2p.dir_me)/\($c.p2p.dir_peer)" else "-" end) "
      + "| \($c.groups.at_by_me)/\($c.groups.at_me) "
      + "| \($c.groups.n) | \($c.relation // $c.guess) |"),
    "",
    "## 上下级推断草表（含证据，请校正）",
    "",
    ((map(select(.relation != null or .guess != "平级/待定")) |
      if length == 0 then "（无强信号，全部归为平级/待定）"
      else .[] | (if .relation != null
        then "- **\(.name // .fid)** → \(.relation)（通讯录 ground truth）"
        else "- **\(.name // .fid)** → \(.guess)：\([.signals[].why] | join("；"))" end) end)),
    "",
    "## seeds.yaml 模板（校正后存 ~/.local/share/lark-persona/seeds.yaml）",
    "",
    "```yaml",
    "# 人工覆盖层（优先级最高，高于通讯录 org.ndjson 与统计推断；只写需纠正的人）",
    "leader:            # 直属上级 open_id 或姓名",
    "peers: []          # 平级协作",
    "reports: []        # 下属",
    "```"'
}

# ---------- 蒸馏增量检查：manifest 游标 vs 归档新增量（纯本地，零 API/LLM）----------
# manifest.json 由每轮蒸馏收尾时写入（self/corpus_until/各层样本量/关系卡清单）。
# 输出 NDJSON：style 层是否过期、每张关系卡新增量、新进 top15 未建卡者。
stale() {
  local manifest="$DATA_DIR/persona/manifest.json" card_thr=150 style_pct=50
  while [ $# -gt 0 ]; do case "$1" in
    --manifest) manifest="$2"; shift 2 ;;
    --card-threshold) card_thr="$2"; shift 2 ;;
    --style-percent) style_pct="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  [ -f "$manifest" ] || { echo "[graph] 缺 $manifest（每轮蒸馏收尾生成），无法判增量" >&2; exit 1; }
  local self cu
  self=$(jq -r '.self' "$manifest")
  cu=$(jq -r '.corpus_until' "$manifest")

  local map dir cid meta mode tid n_self n_all up_new=0 peer_new=0 group_new=0
  map=$(mktemp)
  for dir in "$ARCHIVE/msgs"/*/; do
    cid=$(basename "$dir")
    meta=$(jq -c --arg c "$cid" 'select(.cid == $c)' "$ARCHIVE/chats.ndjson" | head -1)
    [ -n "$meta" ] || continue
    mode=$(jq -r '.mode' <<<"$meta")
    tid=$(jq -r '.tid // empty' <<<"$meta")
    read -r n_self n_all < <(cat "$dir"/*.ndjson 2>/dev/null \
      | jq -rs --arg s "$self" --arg cu "$cu" \
          '[.[] | select(.type == "text" and .t > $cu)] as $new
           | "\($new | map(select(.fid == $s)) | length)\t\($new | length)"')
    printf '%s\t%s\n' "$cid" "$n_all" >>"$map"
    if [ "$mode" = "p2p" ]; then
      [ "$tid" = "$self" ] && continue
      [ "$(jq -r '.tt // empty' <<<"$meta")" = "bot" ] && continue
      if jq -e --arg t "$tid" '.style.up.peer_ids | index($t)' "$manifest" >/dev/null; then
        up_new=$((up_new + n_self))
      else
        peer_new=$((peer_new + n_self))
      fi
    else
      group_new=$((group_new + n_self))
    fi
  done

  local layer new base s
  for layer in up peer group; do
    case "$layer" in up) new=$up_new ;; peer) new=$peer_new ;; group) new=$group_new ;; esac
    base=$(jq -r --arg l "$layer" '.style[$l].n // 0' "$manifest")
    if [ "$base" -eq 0 ]; then
      [ "$new" -gt 0 ] && s=true || s=false
    elif [ $((new * 100)) -ge $((base * style_pct)) ]; then s=true; else s=false; fi
    printf '{"kind":"style","layer":"%s","new":%s,"base":%s,"stale":%s}\n' "$layer" "$new" "$base" "$s"
  done

  local fid name ccid cnew
  while IFS= read -r fid; do
    name=$(jq -r --arg f "$fid" '.contacts[$f].name // ""' "$manifest")
    ccid=$(jq -r --arg t "$fid" 'select(.mode == "p2p" and .tid == $t) | .cid' "$ARCHIVE/chats.ndjson" | head -1)
    cnew=$(awk -F'\t' -v c="$ccid" '$1==c{print $2; found=1} END{if(!found) print 0}' "$map")
    [ "$cnew" -ge "$card_thr" ] && s=true || s=false
    printf '{"kind":"card","fid":"%s","name":"%s","new":%s,"stale":%s}\n' "$fid" "$name" "$cnew" "$s"
  done < <(jq -r '.contacts | keys[]' "$manifest")

  if [ -f "$DATA_DIR/evidence/contacts.json" ]; then
    jq -c --argjson m "$(jq '.contacts' "$manifest")" '
      [.[] | select(.p2p != null)] | sort_by(-(.p2p.n_me + .p2p.n_peer)) | .[:15]
      | to_entries[] | select(($m[.value.fid] // null) == null)
      | {kind: "card-missing", fid: .value.fid, name: .value.name, rank: (.key + 1)}' \
      "$DATA_DIR/evidence/contacts.json"
  fi
  rm -f "$map"
}

# ---------- 全档案编排 ----------
run() {
  local top=20 gap=240
  while [ $# -gt 0 ]; do case "$1" in
    --top) top="$2"; shift 2 ;;
    --gap-min) gap="$2"; shift 2 ;;
    *) shift ;;
  esac; done
  [ -f "$ARCHIVE/chats.ndjson" ] || { echo "[graph] 缺 $ARCHIVE/chats.ndjson，先跑 collect.sh collect" >&2; exit 1; }
  local self
  self=$(lark-cli auth status 2>/dev/null | jq -r '.identities.user.openId // empty')
  [ -n "$self" ] || { echo "[graph] 无法获取用户身份" >&2; exit 1; }

  local ev="$DATA_DIR/evidence" rows dir cid meta mode
  mkdir -p "$ev"
  rows="$ev/rows.ndjson"
  : >"$rows"
  for dir in "$ARCHIVE/msgs"/*/; do
    cid=$(basename "$dir")
    meta=$(jq -c --arg c "$cid" 'select(.cid == $c)' "$ARCHIVE/chats.ndjson" | head -1)
    [ -n "$meta" ] || continue
    mode=$(jq -r '.mode' <<<"$meta")
    # 自聊会话（peer=自己）只留档案作风格语料，不进关系统计
    [ "$(jq -r '.tid // empty' <<<"$meta")" = "$self" ] && continue
    if [ "$mode" = "p2p" ]; then
      cat "$dir"/*.ndjson 2>/dev/null \
        | p2p_stats --self "$self" --peer "$(jq -r '.tid // empty' <<<"$meta")" \
            --name "$(jq -r '.name // empty' <<<"$meta")" --gap-min "$gap" >>"$rows"
    else
      cat "$dir"/*.ndjson 2>/dev/null \
        | group_stats --self "$self" --cid "$cid" \
            --owner "$(jq -r '.owner // empty' <<<"$meta")" \
            --name "$(jq -r '.name // empty' <<<"$meta")" >>"$rows"
    fi
  done
  aggregate <"$rows" >"$ev/contacts.json"
  if [ -f "$ARCHIVE/org.ndjson" ]; then
    org_merge --self "$self" --org "$ARCHIVE/org.ndjson" <"$ev/contacts.json" >"$ev/contacts.json.tmp" \
      && mv "$ev/contacts.json.tmp" "$ev/contacts.json"
  fi
  report --top "$top" <"$ev/contacts.json" >"$DATA_DIR/report.md"
  echo "[graph] $(jq 'length' "$ev/contacts.json") 位联系人 → $ev/contacts.json, $DATA_DIR/report.md" >&2
}

case "${1:-}" in
  p2p-stats) shift; p2p_stats "$@" ;;
  group-stats) shift; group_stats "$@" ;;
  aggregate) shift; aggregate "$@" ;;
  org-merge) shift; org_merge "$@" ;;
  report) shift; report "$@" ;;
  stale) shift; stale "$@" ;;
  run) shift; run "$@" ;;
  *) echo "usage: graph.sh p2p-stats|group-stats|aggregate|org-merge|report|run ..." >&2; exit 2 ;;
esac
