#!/usr/bin/env bash
# =============================================================================
# 添加上游 Account 并关联到一期的订阅型 Group
# =============================================================================
#
# 设计目标：API Key 全程只在「文件 → python → HTTP 请求体」这条路径上流动，
# 不 echo、不写日志、不进 shell 历史、不出现在任何命令行参数里。
#
# 用法：
#   umask 077 && cat > ~/.sub2api_upstream_key     # 粘贴 key，回车，Ctrl+D
#   export ADMIN_PASSWORD='...'
#   UPSTREAM_KEY_FILE=~/.sub2api_upstream_key PLATFORM=anthropic \
#     ./scripts/internal/add_upstream_account.sh
#
# 环境变量：
#   UPSTREAM_KEY_FILE  必填，存放 API Key 的文件路径（内容 = 纯 key，首尾空白会被去掉）
#   PLATFORM           必填，anthropic | openai
#   ADMIN_PASSWORD     必填
#   ACCOUNT_NAME       可选，默认 internal-<platform>-primary
#   UPSTREAM_BASE_URL  可选，中转站/自建网关的地址（官方 OpenAI 留空即可）。
#                      带不带 /v1 都行，buildOpenAIEndpointURL 会正确拼接。
#   ACCOUNT_ID         可选，给了就是【更新】该账号而不是新建
#   PROXY_ID           可选，出海代理的 proxy 记录 id；传 0 表示清除代理（中转站通常直连）
#   ADMIN_EMAIL / BASE_URL / PG_CONTAINER  同 configure_phase1.sh（ADMIN_EMAIL 必填）
#
# 它会把这个 Account 关联到 internal-<platform>-50 与 internal-<platform>-200 两个组。
# =============================================================================

set -euo pipefail

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
ADMIN_EMAIL="${ADMIN_EMAIL:?请设置 ADMIN_EMAIL=<管理员邮箱>}"
PG_CONTAINER="${PG_CONTAINER:-sub2api-postgres-dev}"

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[33m    ! %s\033[0m\n' "$*"; }
die()  { printf '\033[31m\n[FAIL] %s\033[0m\n' "$*" >&2; exit 1; }

[[ -n "${ADMIN_PASSWORD:-}" ]]    || die "请先 export ADMIN_PASSWORD='...'"
[[ -n "${UPSTREAM_KEY_FILE:-}" ]] || die "请设置 UPSTREAM_KEY_FILE=<存放 key 的文件路径>"
[[ -f "$UPSTREAM_KEY_FILE" ]]     || die "找不到文件：$UPSTREAM_KEY_FILE"
[[ -s "$UPSTREAM_KEY_FILE" ]]     || die "文件是空的：$UPSTREAM_KEY_FILE"
[[ -n "${PLATFORM:-}" ]]          || die "请设置 PLATFORM=anthropic 或 openai"
case "$PLATFORM" in anthropic|openai) ;; *) die "PLATFORM 只能是 anthropic / openai" ;; esac

ACCOUNT_NAME="${ACCOUNT_NAME:-internal-${PLATFORM}-primary}"
UPSTREAM_BASE_URL="${UPSTREAM_BASE_URL:-}"
ACCOUNT_ID="${ACCOUNT_ID:-}"
PROXY_ID="${PROXY_ID:-}"

# 文件权限体检（只提醒，不阻断）
PERM=$(stat -c '%a' "$UPSTREAM_KEY_FILE")
[[ "$PERM" == "600" || "$PERM" == "400" ]] \
  || warn "$UPSTREAM_KEY_FILE 权限是 $PERM，建议 chmod 600"

say "登录"
TOKEN=$(ADMIN_EMAIL="$ADMIN_EMAIL" python3 -c \
  'import json,os;print(json.dumps({"email":os.environ["ADMIN_EMAIL"],"password":os.environ["ADMIN_PASSWORD"]}))' \
  | curl -fsS -X POST "$BASE_URL/api/v1/auth/login" \
      -H 'Content-Type: application/json' -d @- \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["access_token"])') \
  || die "登录失败"
info "ok"

say "查找目标 Group（internal-${PLATFORM}-50 / -200）"
GROUP_IDS=$(curl -fsS -H "Authorization: Bearer $TOKEN" \
  "$BASE_URL/api/v1/admin/groups/all?include_inactive=true" \
  | PLATFORM="$PLATFORM" python3 -c '
import sys, json, os
plat = os.environ["PLATFORM"]
want = {"internal-%s-50" % plat, "internal-%s-200" % plat}
ids = [g["id"] for g in json.load(sys.stdin)["data"] if g["name"] in want]
if len(ids) != 2:
    sys.exit("只找到 %d 个目标 Group，期望 2 个（先跑 configure_phase1.sh）" % len(ids))
print(",".join(str(i) for i in ids))
') || die "Group 查找失败"
info "group_ids = $GROUP_IDS"

if [[ -n "$ACCOUNT_ID" ]]; then
  say "更新 Account #$ACCOUNT_ID（key 不会出现在任何输出里）"
else
  say "创建 Account（key 不会出现在任何输出里）"
fi
[[ -n "$UPSTREAM_BASE_URL" ]] && info "base_url = $UPSTREAM_BASE_URL"
# key 由 python 直接从文件读入并拼进 JSON，再经 stdin 交给 curl。
# 全程不经过命令行参数、不经过环境变量、不 echo。
if [[ -n "$ACCOUNT_ID" ]]; then
  METHOD=PUT; ENDPOINT="/api/v1/admin/accounts/$ACCOUNT_ID"
else
  METHOD=POST; ENDPOINT="/api/v1/admin/accounts"
fi

RESP=$(UPSTREAM_KEY_FILE="$UPSTREAM_KEY_FILE" PLATFORM="$PLATFORM" \
       ACCOUNT_NAME="$ACCOUNT_NAME" GROUP_IDS="$GROUP_IDS" \
       UPSTREAM_BASE_URL="$UPSTREAM_BASE_URL" PROXY_ID="$PROXY_ID" python3 -c '
import json, os
with open(os.environ["UPSTREAM_KEY_FILE"], "r", encoding="utf-8") as fh:
    key = fh.read().strip()
if not key:
    raise SystemExit("key 文件内容为空")
creds = {"api_key": key}
base = os.environ.get("UPSTREAM_BASE_URL", "").strip()
if base:
    creds["base_url"] = base
body = {
    "name": os.environ["ACCOUNT_NAME"],
    "platform": os.environ["PLATFORM"],
    "type": "apikey",
    "credentials": creds,
    "concurrency": 5,
    "priority": 0,
    "group_ids": [int(i) for i in os.environ["GROUP_IDS"].split(",")],
}
pid = os.environ.get("PROXY_ID", "").strip()
if pid == "0":
    body["proxy_id"] = None      # 显式清除代理
elif pid:
    body["proxy_id"] = int(pid)
print(json.dumps(body))
' | curl -fsS -X "$METHOD" "$BASE_URL$ENDPOINT" \
      -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d @-) \
  || die "$METHOD Account 失败（多半是 key 格式、base_url 或平台不匹配）"

# 只打印安全字段；credentials 服务端已 redact，这里再兜一层
echo "$RESP" | python3 -c '
import sys, json
d = json.load(sys.stdin).get("data") or {}
print("    id=%s name=%s platform=%s type=%s status=%s" % (
    d.get("id"), d.get("name"), d.get("platform"), d.get("type"), d.get("status")))
'

say "回读校验：四个 Group 的 account_count"
docker exec "$PG_CONTAINER" psql -U sub2api -d sub2api -c "
SELECT g.name, g.platform, g.subscription_type, g.status,
       COUNT(ag.account_id) AS account_count
FROM groups g LEFT JOIN account_groups ag ON ag.group_id = g.id
WHERE g.name LIKE 'internal-%'
GROUP BY g.id, g.name, g.platform, g.subscription_type, g.status
ORDER BY g.platform, g.name;"

say "下一步"
info "1. 用测试 Key 发一个真实请求，确认不是 503（空池）也不是 404（模型不支持）"
info "2. 另一个平台的 key 就再跑一次本脚本，换 PLATFORM 和 UPSTREAM_KEY_FILE"
info "3. 都通了就可以跑 002b 导入测试部门了"
warn "验证完记得删掉 key 文件：shred -u $UPSTREAM_KEY_FILE"
