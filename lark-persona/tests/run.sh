#!/usr/bin/env bash
# lark-persona 统计管线测试：纯管道，不打任何 API。
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COLLECT="$TESTS_DIR/../scripts/collect.sh"
GRAPH="$TESTS_DIR/../scripts/graph.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

check() {
  local name="$1" actual="$2" expected="$3"
  if diff -u "$expected" "$actual" >"$TMP/diff.out" 2>&1; then
    echo "PASS $name"
    pass=$((pass + 1))
  else
    echo "FAIL $name"
    sed 's/^/  /' "$TMP/diff.out"
    fail=$((fail + 1))
  fi
}

# 1. trim：原始响应 → 升序 NDJSON，滤已撤回、保留 app 发送者（打标 ftype）
"$COLLECT" trim <"$TESTS_DIR/fixtures/raw-messages-response.json" >"$TMP/trim.out" 2>/dev/null
check "trim" "$TMP/trim.out" "$TESTS_DIR/expected/trim.ndjson"

# 2. p2p-stats（alice）：消息量/发起比/响应中位/短确认/指派，>240min 断会话段
"$GRAPH" p2p-stats --self ou_SELF --peer ou_alice \
  <"$TESTS_DIR/fixtures/p2p-alice.ndjson" >"$TMP/alice.out" 2>/dev/null
check "p2p-stats-alice" "$TMP/alice.out" "$TESTS_DIR/expected/p2p-alice.json"

# 3. p2p-stats（boss）：确认与指派高度不对称的上级形态
"$GRAPH" p2p-stats --self ou_SELF --peer ou_boss \
  <"$TESTS_DIR/fixtures/p2p-boss.ndjson" >"$TMP/boss.out" 2>/dev/null
check "p2p-stats-boss" "$TMP/boss.out" "$TESTS_DIR/expected/p2p-boss.json"

# 4. group-stats：@方向不对称、@我且带指派、群主标记
"$GRAPH" group-stats --self ou_SELF --cid oc_g1 --owner ou_bob --name 测试群 \
  <"$TESTS_DIR/fixtures/group-g1.ndjson" >"$TMP/group.out" 2>/dev/null
check "group-stats" "$TMP/group.out" "$TESTS_DIR/expected/group-g1.ndjson"

# 5. aggregate：跨会话合并 + 上下级信号评分（boss 应判为上级候选）
cat "$TESTS_DIR/expected/p2p-alice.json" "$TESTS_DIR/expected/p2p-boss.json" \
    "$TESTS_DIR/expected/group-g1.ndjson" \
  | "$GRAPH" aggregate >"$TMP/contacts.out" 2>/dev/null
check "aggregate" "$TMP/contacts.out" "$TESTS_DIR/expected/contacts.json"

# 6. report：contacts → Markdown 草表（golden）
"$GRAPH" report --top 20 <"$TESTS_DIR/expected/contacts.json" >"$TMP/report.out" 2>/dev/null
check "report" "$TMP/report.out" "$TESTS_DIR/expected/report.md"

check_status() {
  local name="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo "PASS $name"
    pass=$((pass + 1))
  else
    echo "FAIL $name (expected exit $expected, got $actual)"
    fail=$((fail + 1))
  fi
}

# 6.5 month-done：增量跳过判定（当月/缺文件/月中落盘 → 不完整；月后落盘 → 完整）
mf="$TMP/2026-06.ndjson"; touch "$mf"
"$COLLECT" month-done "$TMP/missing.ndjson" 2026-05 2026-07; check_status "month-done-missing" 1 $?
"$COLLECT" month-done "$mf" 2026-07 2026-07; check_status "month-done-current" 1 $?
touch -t 202606151200 "$mf"
"$COLLECT" month-done "$mf" 2026-06 2026-07; check_status "month-done-midmonth" 1 $?
touch -t 202607011200 "$mf"
"$COLLECT" month-done "$mf" 2026-06 2026-07; check_status "month-done-complete" 0 $?

# 7. org-merge：通讯录 ground truth 合并（直属上级/同组平级/直属下属；无数据者保持推断）
"$GRAPH" org-merge --self ou_SELF --org "$TESTS_DIR/fixtures/org.ndjson" \
  <"$TESTS_DIR/expected/contacts.json" >"$TMP/contacts-org.out" 2>/dev/null
check "org-merge" "$TMP/contacts-org.out" "$TESTS_DIR/expected/contacts-org.json"

# 8. report（org 合并后）：表格与草表优先展示 ground truth
"$GRAPH" report --top 20 <"$TESTS_DIR/expected/contacts-org.json" >"$TMP/report-org.out" 2>/dev/null
check "report-org" "$TMP/report-org.out" "$TESTS_DIR/expected/report-org.md"

# 9. stale：蒸馏增量检查（manifest 游标 vs 归档新增；层阈值 50%、卡阈值可调）
D="$TMP/lpdata"
mkdir -p "$D/archive/msgs/oc_up" "$D/archive/msgs/oc_p1" "$D/archive/msgs/oc_g1" "$D/persona" "$D/evidence"
cat >"$D/archive/chats.ndjson" <<'EOF'
{"cid":"oc_up","mode":"p2p","name":"老板","owner":null,"tt":"user","tid":"ou_boss","external":false}
{"cid":"oc_p1","mode":"p2p","name":"Alice","owner":null,"tt":"user","tid":"ou_alice","external":false}
{"cid":"oc_g1","mode":"group","name":"测试群","owner":"ou_bob","tt":null,"tid":null,"external":false}
EOF
cat >"$D/archive/msgs/oc_up/2026-07.ndjson" <<'EOF'
{"mid":"u1","cid":"oc_up","fid":"ou_SELF","ftype":"user","from":"我","type":"text","t":"2026-07-05 10:00","pos":1,"text":"旧消息"}
{"mid":"u2","cid":"oc_up","fid":"ou_SELF","ftype":"user","from":"我","type":"text","t":"2026-07-12 10:00","pos":2,"text":"新1"}
{"mid":"u3","cid":"oc_up","fid":"ou_SELF","ftype":"user","from":"我","type":"text","t":"2026-07-13 10:00","pos":3,"text":"新2"}
EOF
cat >"$D/archive/msgs/oc_p1/2026-07.ndjson" <<'EOF'
{"mid":"p1","cid":"oc_p1","fid":"ou_SELF","ftype":"user","from":"我","type":"text","t":"2026-07-12 09:00","pos":1,"text":"新"}
{"mid":"p2","cid":"oc_p1","fid":"ou_alice","ftype":"user","from":"Alice","type":"text","t":"2026-07-12 09:01","pos":2,"text":"a1"}
{"mid":"p3","cid":"oc_p1","fid":"ou_alice","ftype":"user","from":"Alice","type":"text","t":"2026-07-12 09:02","pos":3,"text":"a2"}
{"mid":"p4","cid":"oc_p1","fid":"ou_alice","ftype":"user","from":"Alice","type":"text","t":"2026-07-12 09:03","pos":4,"text":"a3"}
EOF
cat >"$D/archive/msgs/oc_g1/2026-07.ndjson" <<'EOF'
{"mid":"g1","cid":"oc_g1","fid":"ou_SELF","ftype":"user","from":"我","type":"text","t":"2026-07-12 11:00","pos":1,"text":"群新1"}
{"mid":"g2","cid":"oc_g1","fid":"ou_SELF","ftype":"user","from":"我","type":"text","t":"2026-07-12 11:05","pos":2,"text":"群新2"}
EOF
cat >"$D/persona/manifest.json" <<'EOF'
{"version":1,"self":"ou_SELF","distilled_at":"2026-07-10","corpus_until":"2026-07-10 00:00",
 "style":{"up":{"n":4,"peer_ids":["ou_boss"]},"peer":{"n":4},"group":{"n":6}},
 "contacts":{"ou_alice":{"name":"Alice","n":4}}}
EOF
cp "$TESTS_DIR/expected/contacts-org.json" "$D/evidence/contacts.json"
LP_DATA_DIR="$D" "$GRAPH" stale --card-threshold 3 >"$TMP/stale.out" 2>/dev/null
check "stale" "$TMP/stale.out" "$TESTS_DIR/expected/stale.ndjson"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
