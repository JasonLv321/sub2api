#!/usr/bin/env bash
# =============================================================================
# 一期内部化配置脚本（002a / knowledge §13）
# =============================================================================
#
# 做五件事，全部幂等，任何一步失败立即停止：
#   1. 关闭八项商业 settings（直接写 DB，绕开 PUT /admin/settings 的非指针 bool 坑）
#   2. 六个第三方登录 provider 钉死 false + 写入邮箱域白名单
#   3. 创建 department 属性定义（select 型，options = 下面的一级部门清单）
#   4. 创建 4 个订阅型 Group（anthropic/openai × 50/200）
#   5. 回读校验并打印「还差什么」
#
# 本脚本【不会】创建上游 Account —— 那需要你的真实 API Key，请在后台
# 「账号管理」里加，然后把它关联到这 4 个 Group（或用 SOURCE_GROUP_ID 复制）。
#
# 用法：
#   export ADMIN_PASSWORD='...'                  # 必填，不要写进文件
#   ./configure_phase1.sh --dry-run              # 先看它要做什么
#   ./configure_phase1.sh                        # 真正执行
#   SOURCE_GROUP_ID=1 ./configure_phase1.sh      # 建组时从 1 号组复制 Account
#
# 环境变量：
#   ADMIN_PASSWORD   必填
#   ADMIN_EMAIL      必填，管理员邮箱
#   BASE_URL         默认 http://127.0.0.1:8080
#   PG_CONTAINER     默认 sub2api-postgres-dev
#   SOURCE_GROUP_ID  可选，建组时复制其 Account 池
#
# 已知限制：
#   - settings 直接写 DB，写完【必须重启应用容器】才会生效
#     （settings 缓存 + 002c 的支付路由启动期快照都要重启才刷新）
#   - department 属性若已存在，脚本【不会】改它的 options。改 code 会静默
#     产生孤儿数据（见 §13.4 与 review 记录），需要人工决策。
# =============================================================================

set -euo pipefail

# ---------------------------------------------------------------------------
# ⬇⬇⬇ 执行前先改这里：一级部门清单（value=稳定 code，label=显示名）
#      code 只允许新增和停用，【不要】因为部门改名就改 code。
# ---------------------------------------------------------------------------
DEPARTMENTS=(
  "engineering:研发部"
  "product:产品部"
  "operations:运营部"
  "finance:财务部"
  "hr:人力资源部"
)

# ---------------------------------------------------------------------------
# ⬇⬇⬇ 执行前先改这里：公司邮箱域白名单
#      写进 registration_email_suffix_whitelist。格式 "@example.com"，
#      "*.example.com" 通配子域。⚠️ 留空 = 全放行（见 registration_email_policy.go）。
# ---------------------------------------------------------------------------
EMAIL_SUFFIX_PLACEHOLDER="@company.example.com"
EMAIL_SUFFIX_WHITELIST=(
  "@company.example.com"
)

# 平台 × 额度档矩阵（§13.3.2）。加平台/加档位纯配置，照格式往下加即可。
# 注意变量名不能叫 GROUPS —— 那是 bash 内置数组（当前用户的组 ID），会被覆盖。
GROUP_MATRIX=(
  "anthropic:50"
  "anthropic:200"
  "openai:50"
  "openai:200"
)

BASE_URL="${BASE_URL:-http://127.0.0.1:8080}"
ADMIN_EMAIL="${ADMIN_EMAIL:?请设置 ADMIN_EMAIL=<管理员邮箱>}"
PG_CONTAINER="${PG_CONTAINER:-sub2api-postgres-dev}"
SOURCE_GROUP_ID="${SOURCE_GROUP_ID:-}"

DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

SETTINGS_KEYS=(
  registration_enabled
  payment_enabled
  promo_code_enabled
  invitation_code_enabled
  affiliate_enabled
  affiliate_admin_recharge_enabled
  available_channels_enabled
  dingtalk_connect_bypass_registration
)

# 六个第三方登录 provider 的总开关。一期全部钉死 false：DB 无键时
# effectiveEmailOAuthConfig（setting_oauth.go:351,359）回落 YAML，等于"没配就是关"，
# 但那是隐式的 —— 显式写库才挡得住往 config.yaml 里加一段就被打开。
# 二期接企业 OIDC 时，只把 oidc_connect_enabled 改回 true，其余五个保持 false。
AUTH_PROVIDER_KEYS=(
  github_oauth_enabled
  google_oauth_enabled
  oidc_connect_enabled
  linuxdo_connect_enabled
  wechat_connect_enabled
  dingtalk_connect_enabled
)

# --- 小工具 -----------------------------------------------------------------

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[33m    ! %s\033[0m\n' "$*"; }
die()  { printf '\033[31m\n[FAIL] %s\033[0m\n' "$*" >&2; exit 1; }

# 从 stdin 的 JSON 里按 python 表达式取值，取不到就报错退出
jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print($1)"; }

psql_do() {
  docker exec -i "$PG_CONTAINER" psql -U sub2api -d sub2api -v ON_ERROR_STOP=1 "$@"
}

require() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

# --- 前置检查 ---------------------------------------------------------------

require curl
require python3
require docker
[[ -n "${ADMIN_PASSWORD:-}" ]] || die "请先 export ADMIN_PASSWORD='...'"

docker inspect "$PG_CONTAINER" >/dev/null 2>&1 \
  || die "找不到 postgres 容器：$PG_CONTAINER（服务起来了吗？）"

say "目标环境"
info "BASE_URL     = $BASE_URL"
info "ADMIN_EMAIL  = $ADMIN_EMAIL"
info "PG_CONTAINER = $PG_CONTAINER"
info "部门清单     = ${DEPARTMENTS[*]}"
info "Group 矩阵   = ${GROUP_MATRIX[*]}"
info "Account 来源 = ${SOURCE_GROUP_ID:-（未指定，建出来的组会是空池）}"
(( DRY_RUN )) && warn "DRY-RUN 模式：只打印，不做任何写操作"

# --- 登录 -------------------------------------------------------------------

say "1/6 登录取 admin token"
LOGIN_BODY=$(ADMIN_EMAIL="$ADMIN_EMAIL" python3 -c \
  'import json,os;print(json.dumps({"email":os.environ["ADMIN_EMAIL"],"password":os.environ["ADMIN_PASSWORD"]}))')

TOKEN=$(curl -fsS -X POST "$BASE_URL/api/v1/auth/login" \
  -H 'Content-Type: application/json' -d "$LOGIN_BODY" \
  | jget 'd["data"]["access_token"]') || die "登录失败，检查邮箱/密码/服务是否在跑"
info "拿到 token（${#TOKEN} 字符）"

api() { # api METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -fsS -X "$method" "$BASE_URL$path" \
      -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$body"
  else
    curl -fsS -X "$method" "$BASE_URL$path" -H "Authorization: Bearer $TOKEN"
  fi
}

# --- 2. 商业 settings -------------------------------------------------------

say "2/6 关闭八项商业 settings"
CURRENT=$(psql_do -tAc "SELECT key||'='||value FROM settings WHERE key IN ($(
  printf "'%s'," "${SETTINGS_KEYS[@]}" | sed 's/,$//')) ORDER BY key;" || true)
if [[ -n "$CURRENT" ]]; then
  info "当前 DB 中已有："; while read -r l; do [[ -n "$l" ]] && info "  $l"; done <<<"$CURRENT"
else
  info "当前 DB 中这八项一条都没有（走的是代码默认值）"
fi

if (( DRY_RUN )); then
  warn "DRY-RUN：跳过写入。将把上述八项全部 upsert 为 'false'"
else
  psql_do -q <<SQL
BEGIN;
INSERT INTO settings (key, value, updated_at)
SELECT key, 'false', NOW()
FROM unnest(ARRAY[$(printf "'%s'," "${SETTINGS_KEYS[@]}" | sed 's/,$//')]) AS keys(key)
ON CONFLICT (key) DO UPDATE
  SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at;
COMMIT;
SQL
  info "八项已写入 false"
fi

# --- 3. 认证 provider 收口 --------------------------------------------------

say "3/6 收口第三方登录 provider + 邮箱域白名单"
CURRENT_AUTH=$(psql_do -tAc "SELECT key||'='||value FROM settings WHERE key IN ($(
  printf "'%s'," "${AUTH_PROVIDER_KEYS[@]}" | sed 's/,$//')) ORDER BY key;" || true)
if [[ -n "$CURRENT_AUTH" ]]; then
  info "当前 DB 中已有："; while read -r l; do [[ -n "$l" ]] && info "  $l"; done <<<"$CURRENT_AUTH"
else
  info "当前 DB 中这六项一条都没有（隐式回落 YAML，等于关但没钉死）"
fi

WHITELIST_JSON=$(printf '%s\n' "${EMAIL_SUFFIX_WHITELIST[@]}" \
  | python3 -c 'import json,sys;print(json.dumps([l.strip() for l in sys.stdin if l.strip()]))')
info "邮箱域白名单 = $WHITELIST_JSON"
for suffix in "${EMAIL_SUFFIX_WHITELIST[@]}"; do
  if [[ "$suffix" == "$EMAIL_SUFFIX_PLACEHOLDER" ]]; then
    warn "⚠️  白名单里还是占位符 $EMAIL_SUFFIX_PLACEHOLDER —— 上线前必须换成真实公司域名"
  fi
done

if (( DRY_RUN )); then
  warn "DRY-RUN：跳过写入。将把上述六项 upsert 为 'false'，并写入白名单"
else
  psql_do -q <<SQL
BEGIN;
INSERT INTO settings (key, value, updated_at)
SELECT key, 'false', NOW()
FROM unnest(ARRAY[$(printf "'%s'," "${AUTH_PROVIDER_KEYS[@]}" | sed 's/,$//')]) AS keys(key)
ON CONFLICT (key) DO UPDATE
  SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at;
INSERT INTO settings (key, value, updated_at)
VALUES ('registration_email_suffix_whitelist', \$json\$$WHITELIST_JSON\$json\$, NOW())
ON CONFLICT (key) DO UPDATE
  SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at;
COMMIT;
SQL
  info "六项 provider 已写入 false，白名单已写入"
fi

# --- 4. department 属性 -----------------------------------------------------

say "4/6 创建 department 属性定义"
EXISTING_ATTR=$(api GET /api/v1/admin/user-attributes \
  | jget 'next((a["key"] for a in d["data"] if a["key"]=="department"), "")')

if [[ "$EXISTING_ATTR" == "department" ]]; then
  warn "department 属性已存在，跳过创建。"
  warn "脚本【不会】自动改 options —— 改 code 会让已有用户值变成孤儿，"
  warn "并让 002b 导入脚本整体退出。要改请人工确认后单独操作。"
  api GET /api/v1/admin/user-attributes | python3 -c '
import sys, json
for a in json.load(sys.stdin)["data"]:
    if a["key"] == "department":
        opts = a.get("options") or []
        print("    现有 options:", ", ".join(
            o["value"] + "(" + o["label"] + ")" for o in opts))
'
else
  ATTR_BODY=$(python3 - "${DEPARTMENTS[@]}" <<'PY'
import json, sys
opts = []
for item in sys.argv[1:]:
    value, _, label = item.partition(":")
    opts.append({"value": value, "label": label or value})
print(json.dumps({
    "key": "department",
    "name": "部门",
    "description": "员工当前成本归属部门（一级部门，code 稳定不随改名变化）",
    "type": "select",
    "options": opts,
    "required": True,
    "validation": {},
    "placeholder": "请选择部门",
    "enabled": True,
}, ensure_ascii=False))
PY
)
  if (( DRY_RUN )); then
    warn "DRY-RUN：跳过创建。请求体："; echo "$ATTR_BODY" | python3 -m json.tool
  else
    api POST /api/v1/admin/user-attributes "$ATTR_BODY" >/dev/null
    info "department 属性已创建（${#DEPARTMENTS[@]} 个部门）"
  fi
fi

# --- 5. 订阅型 Group --------------------------------------------------------

say "5/6 创建 4 个订阅型 Group"
EXISTING_GROUPS=$(api GET '/api/v1/admin/groups/all?include_inactive=true' \
  | jget '"\n".join(g["name"] for g in d["data"])')

for spec in "${GROUP_MATRIX[@]}"; do
  platform="${spec%%:*}"; usd="${spec##*:}"
  name="internal-${platform}-${usd}"

  if grep -qx "$name" <<<"$EXISTING_GROUPS"; then
    info "已存在，跳过：$name"
    continue
  fi

  GROUP_BODY=$(python3 - "$name" "$platform" "$usd" "$SOURCE_GROUP_ID" <<'PY'
import json, sys
name, platform, usd, source = sys.argv[1:5]
body = {
    "name": name,
    "description": f"Internal {platform} USD {usd} rolling quota",
    "platform": platform,
    "rate_multiplier": 1,
    "is_exclusive": False,
    # 必须显式给：省略会变成 standard 型，走余额路径，静默错到没人发现
    "subscription_type": "subscription",
    "monthly_limit_usd": int(usd),
}
# daily/weekly 完全省略（不要传 null：DTO 会把显式 null 转成 0）
if source.strip():
    body["copy_accounts_from_group_ids"] = [int(source)]
print(json.dumps(body, ensure_ascii=False))
PY
)
  if (( DRY_RUN )); then
    warn "DRY-RUN：跳过创建 $name。请求体："; echo "$GROUP_BODY" | python3 -m json.tool
  else
    api POST /api/v1/admin/groups "$GROUP_BODY" >/dev/null
    info "已创建：$name（platform=$platform, monthly=$usd USD）"
  fi
done

# --- 6. 回读校验 ------------------------------------------------------------

say "6/6 回读校验"

echo
info "— settings（八项应全为 false）—"
psql_do -c "SELECT key, value FROM settings WHERE key IN ($(
  printf "'%s'," "${SETTINGS_KEYS[@]}" | sed 's/,$//')) ORDER BY key;"

info "— 第三方登录 provider（六项应全为 false）+ 邮箱域白名单 —"
psql_do -c "SELECT key, value FROM settings WHERE key IN ($(
  printf "'%s'," "${AUTH_PROVIDER_KEYS[@]}" | sed 's/,$//'),
  'registration_email_suffix_whitelist') ORDER BY key;"

info "— department 属性 —"
api GET /api/v1/admin/user-attributes | python3 -c '
import sys, json
found = False
for a in json.load(sys.stdin)["data"]:
    if a["key"] == "department":
        found = True
        print("    type=%s required=%s enabled=%s" % (
            a["type"], a["required"], a["enabled"]))
        for o in (a.get("options") or []):
            print("      - %-14s %s" % (o["value"], o["label"]))
if not found:
    print("    [!] 没有找到 department 属性")
'

info "— Group 矩阵（四项必须全部满足：subscription / active / account_count>0）—"
psql_do -c "
SELECT g.name, g.platform, g.subscription_type, g.status,
       g.monthly_limit_usd, COUNT(ag.account_id) AS account_count
FROM groups g LEFT JOIN account_groups ag ON ag.group_id = g.id
WHERE g.name LIKE 'internal-%'
GROUP BY g.id, g.name, g.platform, g.subscription_type, g.status, g.monthly_limit_usd
ORDER BY g.platform, g.monthly_limit_usd;"

EMPTY_POOLS=$(psql_do -tAc "
SELECT COUNT(*) FROM (
  SELECT g.id FROM groups g LEFT JOIN account_groups ag ON ag.group_id = g.id
  WHERE g.name LIKE 'internal-%'
  GROUP BY g.id HAVING COUNT(ag.account_id) = 0
) t;")

say "还差什么"
if (( DRY_RUN )); then
  warn "这是 DRY-RUN，什么都没做。去掉 --dry-run 再跑一次。"
else
  info "1. ⚠️  重启应用容器 —— settings 缓存与支付路由启动期快照都要重启才生效："
  info "     docker restart sub2api-dev"
fi
TOTAL_GROUPS=$(psql_do -tAc \
  "SELECT COUNT(*) FROM groups WHERE name LIKE 'internal-%';")

if [[ "${TOTAL_GROUPS:-0}" == "0" ]]; then
  warn "2. 还没有任何 internal-* Group（dry-run 或创建失败？）"
elif [[ "${EMPTY_POOLS:-0}" != "0" ]]; then
  warn "2. 有 $EMPTY_POOLS 个 internal-* Group 是空 Account 池。"
  warn "   空池的请求会在 Account 选择阶段返回 503（不是配置报错，很难查）。"
  warn "   请在后台「账号管理」加上游 API Key，并关联到对应平台的 Group，"
  warn "   或重跑本脚本时带上 SOURCE_GROUP_ID=<已验证的同平台组 ID>。"
else
  info "2. ✅ $TOTAL_GROUPS 个 internal-* Group 都有 Account。"
fi
info "3. 用测试 Key 发一个真实请求确认不是 503，再做 002b 批量导入。"
info "4. 有效期不在 Group 上配 —— bulk-assign 每次显式传 validity_days=36500。"
