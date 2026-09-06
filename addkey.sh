#!/usr/bin/env bash
# addkey.sh — 往 auths/ 目录添加一个 CodeBuddy 凭证（ck_ API Key）。
#
# 用法:
#   ./addkey.sh                       # 交互式输入
#   ./addkey.sh ck_xxxx [备注名]      # 直接指定
#
# 添加后在管理后台点「重载目录」，或重启服务即可生效。
set -euo pipefail
cd "$(dirname "$0")"

AUTH_DIR="${CB2A_AUTH_DIR:-./auths}"
mkdir -p "$AUTH_DIR"

if [[ $# -ge 1 ]]; then
    KEY="$1"
    NAME="${2:-}"
else
    read -rp "CodeBuddy API Key (ck_...): " KEY
    read -rp "备注名（可留空）: " NAME
fi

KEY="$(echo "$KEY" | tr -d '[:space:]')"
if [[ -z "$KEY" ]]; then
    echo "错误：API Key 不能为空" >&2
    exit 1
fi

UID_STR="${NAME:-}"
if [[ -z "$UID_STR" ]]; then
    UID_STR="key-$(printf '%s' "$KEY" | cksum | cut -d' ' -f1)"
fi

OUT="$AUTH_DIR/codebuddy-${UID_STR}.json"
python3 - "$KEY" "$UID_STR" "$OUT" <<'PY'
import json, sys
key, uid, out = sys.argv[1], sys.argv[2], sys.argv[3]
with open(out, "w", encoding="utf-8") as f:
    json.dump({"api_key": key, "uid": uid, "nickname": uid, "type": "api_key"},
              f, ensure_ascii=False, indent=2)
print("已写入: " + out)
PY

echo ""
echo "提示：管理后台 /admin → 「重载目录」后生效。"
