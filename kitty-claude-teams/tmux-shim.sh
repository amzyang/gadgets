#!/usr/bin/env bash
# tmux-shim.sh — fake `tmux` for Claude Code Agent Teams, backed by kitty.
#
# Claude Code's experimental Agent Teams feature drives "split pane" mode by
# shelling out to the real `tmux` binary (split-window / send-keys / select-pane
# / kill-pane / capture-pane / display-message). Anthropic only supports tmux and
# iTerm2 for this — kitty is not supported natively.
#
# This script is the same trick cmux uses for Ghostty: the launcher puts a symlink
# named `tmux` (pointing here) at the front of PATH and exports $TMUX/$TMUX_PANE so
# Claude believes it is inside tmux. Every tmux subcommand Claude issues lands here
# and is translated into a kitty remote-control (`kitten @`) call, so teammates
# spawn as native kitty splits instead of tmux panes.
#
# It is deliberately a *subset* shim: the verbs Claude's teams mode actually uses
# are translated; everything else is a silent success (exit 0), which is what cmux
# does too. Set KCT_DEBUG=1 to trace every invocation to stderr.

set -uo pipefail

STATE_DIR="${KCT_STATE_DIR:-${XDG_RUNTIME_DIR:-/tmp}/kitty-claude-teams/default}"
MAP="$STATE_DIR/map"          # lines: "<tmux-pane-id> <kitty-window-id>"
COUNTER="$STATE_DIR/counter"  # monotonic pane-id counter
mkdir -p "$STATE_DIR"
[ -f "$MAP" ]     || : > "$MAP"
[ -f "$COUNTER" ] || echo 0 > "$COUNTER"

# Directory this shim lives in (= the bin dir we prepended to PATH); needed so the
# env we inject into teammate windows keeps the shim on their PATH.
SHIM_DIR="$(cd "$(dirname "$0")" && pwd)"

log() { [ -n "${KCT_DEBUG:-}" ] && printf '[tmux-shim] %s\n' "$*" >&2 || true; }
log "argv: $*"

# ---- kitty remote-control dispatcher -----------------------------------------
if command -v kitten >/dev/null 2>&1; then
  KITTEN=(kitten @)
else
  KITTEN=(kitty +kitten @)
fi
[ -n "${KCT_KITTY_TO:-}" ] && KITTEN+=(--to "$KCT_KITTY_TO")
kc() { "${KITTEN[@]}" "$@"; }

# ---- pane-id <-> kitty-window-id map -----------------------------------------
map_get() { awk -v k="$1" '$1==k{print $2; exit}' "$MAP"; }
map_put() { # <tmux-pane-id> <kitty-window-id>
  local k="$1" v="$2" tmp="$MAP.tmp"
  awk -v k="$k" -v v="$v" '
    $1==k{print k" "v; done=1; next} {print}
    END{if(!done) print k" "v}' "$MAP" > "$tmp" && mv "$tmp" "$MAP"
}
map_del() { local k="$1" tmp="$MAP.tmp"; awk -v k="$k" '$1!=k' "$MAP" > "$tmp" && mv "$tmp" "$MAP"; }
new_pane_id() { local n; n=$(( $(cat "$COUNTER") + 1 )); echo "$n" > "$COUNTER"; printf '%%%s' "$n"; }

# Expand the tmux format placeholders Claude uses for pane ids. (Quoted patterns
# force literal matching; an unquoted '#{pane_id}' default would close the ${...}.)
fmt_pane() {
  local f="${1:-}" p="$2"
  [ -z "$f" ] && f='#{pane_id}'
  f="${f//"#{pane_id}"/$p}"
  f="${f//"#D"/$p}"
  printf '%s\n' "$f"
}

# Resolve a tmux `-t` target to a kitty window id.
resolve_kid() {
  local t="${1:-}"
  case "$t" in
    %*) map_get "$t" ;;                       # known tmux pane id
    "") map_get "${TMUX_PANE:-%0}" ;;         # default: the current pane
    *)  echo "$t" ;;                          # assume it is already a kitty id
  esac
}

# ---- strip tmux global options, find the subcommand --------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    -V) echo "tmux 3.5 (kitty-claude-teams shim)"; exit 0 ;;
    -L|-S|-f) shift 2 ;;                       # take an argument
    -2|-C|-u|-v|-N|-q|-8|-l|-T) shift ;;       # bare global flags
    --) shift; break ;;
    -*) shift ;;                               # unknown global flag
    *) break ;;
  esac
done
[ $# -eq 0 ] && exit 0
CMD="$1"; shift
log "cmd=$CMD rest=$*"

case "$CMD" in
# ---- create a teammate pane --------------------------------------------------
split-window|splitw|new-window|neww)
  horiz=0; nofocus=0; printid=0; fmt=""; cwd="current"; target=""; shellcmd=(); injenv=()
  case "$CMD" in new-window|neww) wintype="tab" ;; *) wintype="window" ;; esac
  while [ $# -gt 0 ]; do
    case "$1" in
      -h) horiz=1; shift ;;                    # tmux -h = left/right
      -v) horiz=0; shift ;;                    # tmux -v = top/bottom
      -d) nofocus=1; shift ;;
      -P) printid=1; shift ;;
      -F) fmt="$2"; shift 2 ;;
      -c) cwd="$2"; shift 2 ;;
      -t) target="$2"; shift 2 ;;
      -e) injenv+=("$2"); shift 2 ;;           # KEY=VAL to set in the new pane
      -l|-p|-x|-y) shift 2 ;;                   # size args — ignore value
      -b|-f|-I|-Z|-a|-k) shift ;;               # misc flags — ignore
      --) shift; shellcmd=("$@"); break ;;
      -*) shift ;;
      *)  shellcmd=("$@"); break ;;
    esac
  done

  loc="hsplit"; [ "$horiz" = 1 ] && loc="vsplit"
  args=(launch --type="$wintype" --location="$loc" --cwd="$cwd")
  [ "$nofocus" = 1 ] && args+=(--keep-focus)

  pane="$(new_pane_id)"
  # Keep Claude's world consistent inside the teammate window: same shim on PATH,
  # same fake $TMUX, this window's own $TMUX_PANE, teams flag, shared state dir.
  args+=(--env "PATH=$SHIM_DIR:${PATH}")
  args+=(--env "TMUX=${TMUX:-$STATE_DIR/fake-tmux-socket,0,0}")
  args+=(--env "TMUX_PANE=$pane")
  args+=(--env "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1")
  args+=(--env "KCT_STATE_DIR=$STATE_DIR")
  [ -n "${KCT_KITTY_TO:-}" ] && args+=(--env "KCT_KITTY_TO=$KCT_KITTY_TO")
  for e in "${injenv[@]:-}"; do [ -n "$e" ] && args+=(--env "$e"); done
  [ ${#shellcmd[@]} -gt 0 ] && args+=(-- "${shellcmd[@]}")

  kid="$(kc "${args[@]}")" || { log "launch failed"; exit 1; }
  map_put "$pane" "$kid"
  log "created pane=$pane kitty=$kid loc=$loc"

  [ "$printid" = 1 ] && fmt_pane "$fmt" "$pane"
  ;;

# ---- type into a teammate pane -----------------------------------------------
send-keys|send)
  target=""; literal=0; keys=()
  while [ $# -gt 0 ]; do
    case "$1" in
      -t) target="$2"; shift 2 ;;
      -l) literal=1; shift ;;
      -N) shift 2 ;;                            # repeat count — ignore
      -H|-R|-M|-K|-F) shift ;;                  # other flags — ignore
      -X) shift; break ;;                       # copy-mode command — ignore rest
      --) shift; keys+=("$@"); break ;;
      -*) shift ;;
      *)  keys+=("$@"); break ;;
    esac
  done
  kid="$(resolve_kid "$target")"; [ -z "$kid" ] && exit 0
  text=""
  for k in "${keys[@]:-}"; do
    if [ "$literal" = 1 ]; then text+="$k"; continue; fi
    case "$k" in
      Enter|C-m|KPEnter) text+=$'\n' ;;
      Space)             text+=' ' ;;
      Tab|C-i)           text+=$'\t' ;;
      Escape|C-\[)       text+=$'\e' ;;
      C-c)               text+=$'\003' ;;
      C-u)               text+=$'\025' ;;
      C-d)               text+=$'\004' ;;
      BSpace)            text+=$'\177' ;;
      *)                 text+="$k" ;;
    esac
  done
  # We build real control bytes above, so send-text sees them verbatim.
  kc send-text --match "id:$kid" -- "$text" >/dev/null 2>&1 || true
  ;;

# ---- focus a teammate pane ---------------------------------------------------
select-pane|selectp|select-window|selectw)
  target=""
  while [ $# -gt 0 ]; do case "$1" in -t) target="$2"; shift 2 ;; -*) shift ;; *) shift ;; esac; done
  kid="$(resolve_kid "$target")"
  [ -n "$kid" ] && kc focus-window --match "id:$kid" >/dev/null 2>&1 || true
  ;;

# ---- close a teammate pane ---------------------------------------------------
kill-pane|killp)
  target=""
  while [ $# -gt 0 ]; do case "$1" in -t) target="$2"; shift 2 ;; -a) shift ;; -*) shift ;; *) shift ;; esac; done
  kid="$(resolve_kid "$target")"
  [ -n "$kid" ] && kc close-window --match "id:$kid" >/dev/null 2>&1 || true
  case "$target" in %*) map_del "$target" ;; esac
  ;;

# ---- read a teammate pane's contents -----------------------------------------
capture-pane|capturep)
  target=""; printout=0
  while [ $# -gt 0 ]; do
    case "$1" in
      -p) printout=1; shift ;;
      -t) target="$2"; shift 2 ;;
      -S|-E|-b) shift 2 ;;                      # start/end/buffer — take a value
      -*) shift ;;
      *) shift ;;
    esac
  done
  # Real tmux only writes to stdout with -p; without it the capture goes to a
  # paste buffer. Honour that, or we corrupt Claude's stdout stream.
  if [ "$printout" = 1 ]; then
    kid="$(resolve_kid "$target")"
    [ -n "$kid" ] && kc get-text --match "id:$kid" 2>/dev/null || true
  fi
  ;;

# ---- answer pane/format queries ----------------------------------------------
display-message|display|displayp)
  target=""; printout=0; fmt=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -p) printout=1; shift ;;
      -t) target="$2"; shift 2 ;;
      -F) fmt="$2"; shift 2 ;;
      -*) shift ;;
      *)  fmt="$1"; shift ;;
    esac
  done
  if [ "$printout" = 1 ]; then
    pane="${target:-${TMUX_PANE:-%0}}"
    fmt_pane "$fmt" "$pane"
  fi
  ;;

# ---- enumerate panes ---------------------------------------------------------
list-panes|lsp)
  fmt=""
  while [ $# -gt 0 ]; do case "$1" in -F) fmt="$2"; shift 2 ;; -t|-s) shift 2 ;; -*) shift ;; *) shift ;; esac; done
  while read -r pane _; do
    [ -z "$pane" ] && continue
    fmt_pane "$fmt" "$pane"
  done < "$MAP"
  ;;

# ---- session existence checks ------------------------------------------------
has-session|has) exit 0 ;;        # yes, the "session" exists

# ---- everything else: succeed silently (set-option, set-hook, bind-key, …) ----
*) log "unhandled: $CMD $*"; exit 0 ;;
esac

exit 0
