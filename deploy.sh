#!/usr/bin/env bash
# ============================================================================
# deploy.sh — WorkBuddy2API 服务器更新脚本（幂等，可反复执行）
# ----------------------------------------------------------------------------
# 用法（在服务器 /opt/codebuddy2api 下）：
#   ./deploy.sh            # 拉取最新代码 + 重建并重启容器
#   ./deploy.sh --no-pull  # 跳过 git pull，仅用当前代码重建
#   ./deploy.sh --rollback # 回滚到上一个镜像（重建前自动打 tag）
#
# 为什么可以「随时更新」：
#   - 源码在 git 仓（github.com/icebears111/workbuddy2api），服务器只读部署
#   - 凭证 auths/、状态 data/、配置 config.json 是挂载文件，不进镜像，
#     重建容器不丢账号、不丢冷却状态
#   - 重建前自动把当前镜像打上 rollback-<时间戳> tag，一键可回
#
# 首次部署（或迁移到本脚本管理）：
#   1) 确认本目录已有 auths/ data/ config.json（从旧部署拷来即可）
#   2) git remote 配好（deploy key 或 https）
#   3) ./deploy.sh
# ============================================================================
set -euo pipefail
cd "$(dirname "$0")"

TAG="rollback-$(date +%Y%m%d-%H%M%S)"
DO_PULL=1
DO_ROLLBACK=0

for arg in "$@"; do
  case "$arg" in
    --no-pull)  DO_PULL=0 ;;
    --rollback) DO_ROLLBACK=1 ;;
    *) echo "未知参数: $arg" >&2; exit 2 ;;
  esac
done

COMPOSE_FILE="docker-compose.server.yml"
[ -f "$COMPOSE_FILE" ] || { echo "缺少 $COMPOSE_FILE（服务器编排文件）" >&2; exit 1; }
[ -f config.json ] || { echo "缺少 config.json（网关 api_key 等配置）" >&2; exit 1; }
[ -d auths ] || { echo "缺少 auths/（账号凭证目录）" >&2; exit 1; }

if [ "$DO_ROLLBACK" = "1" ]; then
  PREV=$(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^codebuddy2api:rollback-' | sort | tail -1 || true)
  [ -n "$PREV" ] || { echo "没有可回滚的镜像"; exit 1; }
  echo "==> 回滚到 $PREV"
  docker tag "$PREV" codebuddy2api:latest
  docker compose -f "$COMPOSE_FILE" up -d --no-build
  docker compose -f "$COMPOSE_FILE" ps
  echo "回滚完成：$PREV → codebuddy2api:latest"
  exit 0
fi

if [ "$DO_PULL" = "1" ]; then
  echo "==> git pull"
  git pull --ff-only
fi

# 迁移/接管：历史上容器可能是 docker run 手工创建的（无 compose 标签），
# 同名容器会挡住 compose up。数据全在挂载目录（auths/ data/ config.json），
# 移除旧容器不丢任何东西。先记下运行镜像名，供随后备份。
RUN_IMG=$(docker inspect --format '{{.Config.Image}}' codebuddy2api 2>/dev/null || true)
if docker inspect codebuddy2api >/dev/null 2>&1; then
  PROJ=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' codebuddy2api 2>/dev/null || true)
  if [ -z "$PROJ" ]; then
    echo "==> 旧容器非 compose 管理，先移除以便接管（挂载数据不受影响）"
    docker rm -f codebuddy2api >/dev/null
  fi
fi

echo "==> 备份当前运行镜像为 $TAG"
if [ -n "$RUN_IMG" ] && docker image inspect "$RUN_IMG" >/dev/null 2>&1; then
  docker tag "$RUN_IMG" "codebuddy2api:$TAG"
  echo "    $RUN_IMG → codebuddy2api:$TAG"
elif docker image inspect codebuddy2api:latest >/dev/null 2>&1; then
  docker tag codebuddy2api:latest "codebuddy2api:$TAG"
  echo "    codebuddy2api:latest → codebuddy2api:$TAG"
else
  echo "    （无镜像可备份；通常为首次部署）"
fi

echo "==> 构建并重启"
docker compose -f "$COMPOSE_FILE" up -d --build

echo "==> 等待健康检查"
ok=0
for i in $(seq 1 20); do
  st=$(docker inspect --format '{{.State.Health.Status}}' codebuddy2api 2>/dev/null || echo starting)
  [ "$st" = "healthy" ] && { ok=1; break; }
  sleep 3
done

echo "==> 自检"
docker compose -f "$COMPOSE_FILE" ps
KEY=$(python3 -c "import json;print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null || true)
if [ -n "$KEY" ]; then
  # 容器内自检（不经 nginx，验证应用本身）
  docker exec codebuddy2api wget -q -O - --header="Authorization: Bearer $KEY" \
    http://127.0.0.1:7865/v1/models >/dev/null 2>&1 \
    && echo "应用自检：/v1/models OK" || echo "应用自检：/v1/models 失败（看 docker logs codebuddy2api）"
else
  echo "（config.json 未读到 api_key，跳过应用自检）"
fi

if [ "$ok" = "1" ]; then
  echo "部署完成：容器 healthy（备份 tag: $TAG；回滚：./deploy.sh --rollback）"
else
  echo "警告：容器未在 60s 内变为 healthy，请查 docker logs codebuddy2api" >&2
  exit 1
fi
