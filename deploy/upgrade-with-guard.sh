#!/usr/bin/env bash
# upgrade-with-guard.sh — 维护本地 billingguard 补丁并滚动升级（无需合入官方）
#
# 维护模型：
#   - 官方仓库加 remote（默认 upstream），补丁在 feat/linzy9712/guard_exception_billing 分支；
#   - 每次官方发新版：本脚本把补丁分支 rebase 到上游最新代码 → 构建 → 部署；
#   - rebase 冲突时脚本中止，手动解决冲突后重跑即可；
#   - 部署失败自动回滚，备份只保留最近 2 个。
#
# 两种运行模式：
#   A) 本机构建 + 远程部署（推荐，一条命令完成全程）：
#        DEPLOY_HOST=root@38.244.20.147 DEPLOY_SSH_PORT=5684 ./deploy/upgrade-with-guard.sh
#   B) 服务器上自构建自部署（不设 DEPLOY_HOST，行为不变）。
#
# 可选参数：
#   ./deploy/upgrade-with-guard.sh v0.1.180   # rebase 到指定 release tag（默认 upstream/main）
#   BUILD_TAGS=none ...                       # 不内嵌前端（纯 API 网关）
#
# 回滚（服务器上）：
#   cp /opt/sub2api/sub2api.bak-<时间戳> /opt/sub2api/sub2api && systemctl restart sub2api
set -euo pipefail

SRC_DIR=${SRC_DIR:-$(cd "$(dirname "$0")/.." && pwd)}   # 源码目录（含本脚本的仓库）
GUARD_BRANCH=${GUARD_BRANCH:-feat/linzy9712/guard_exception_billing}  # 承载补丁的分支
BASE_REMOTE=${BASE_REMOTE:-upstream}      # 官方仓库 remote；未配置时退回 origin
BUILD_TAGS=${BUILD_TAGS:-embed}           # embed=带管理界面; none=纯 API
VERSION_SUFFIX=${VERSION_SUFFIX:-guard}   # 版本戳后缀，如 0.1.180-guard.1
GO=${GO:-go}

# —— 远程部署配置（模式 A；不设 DEPLOY_HOST 则走本地模式 B）——
DEPLOY_HOST=${DEPLOY_HOST:-}              # 如 root@38.244.20.147
DEPLOY_SSH_KEY=${DEPLOY_SSH_KEY:-$HOME/.ssh/id_ed25519}
DEPLOY_SSH_PORT=${DEPLOY_SSH_PORT:-22}
REMOTE_BIN_DIR=${REMOTE_BIN_DIR:-/opt/sub2api}
REMOTE_SERVICE=${REMOTE_SERVICE:-sub2api}

cd "$SRC_DIR"

# 工作区必须干净（rebase 前提）
if [ -n "$(git status --porcelain)" ]; then
  echo "工作区有未提交改动，请先处理：git status" >&2
  exit 1
fi

# 拉取官方最新代码（官方 remote 不存在时退回 origin）
if ! git fetch --tags "$BASE_REMOTE" 2>/dev/null; then
  BASE_REMOTE=origin
  git fetch --tags origin
fi

# 补丁分支 rebase 到目标版本
BASE_REF="${1:-$BASE_REMOTE/main}"
git checkout -q "$GUARD_BRANCH"
echo "==> rebase $GUARD_BRANCH onto $BASE_REF"
if ! git rebase "$BASE_REF"; then
  echo "rebase 冲突。补丁很小（独立包 + 3 处插入），冲突通常只是上下文错位：" >&2
  echo "  1) 查看:  git -C $SRC_DIR status" >&2
  echo "  2) 手动解决后: git -C $SRC_DIR add -A && git -C $SRC_DIR rebase --continue" >&2
  echo "  3) 放弃本次升级: git -C $SRC_DIR rebase --abort" >&2
  exit 1
fi

# 版本戳：优先精确 tag，其次最近的 tag，最后 VERSION 文件 + 后缀。
# 内置更新的版本比较只取前三位数字（后缀被忽略），0.1.180-guard.1 与官方
# 0.1.180 等价：后台版本显示正确，且不会误提示「可更新到 0.1.180」。
NEW_VER="$(git describe --tags --exact-match "$BASE_REF" 2>/dev/null | sed 's/^v//' || true)"
if [ -z "$NEW_VER" ]; then
  NEW_VER="$(git describe --tags --abbrev=0 "$BASE_REF" 2>/dev/null | sed 's/^v//' || true)"
fi
if [ -z "$NEW_VER" ]; then
  NEW_VER="$(tr -d '\r\n' < backend/cmd/server/VERSION)"
fi
# —— 构建 ——
cd "$SRC_DIR/backend"
TAG_FLAG=""
if [ "$BUILD_TAGS" != "none" ]; then
  if [ ! -f internal/web/dist/index.html ]; then
    echo "缺少前端构建产物 internal/web/dist/。如需管理界面，先执行：" >&2
    echo "  cd $SRC_DIR/frontend && pnpm install && pnpm build" >&2
    echo "否则请用 BUILD_TAGS=none 重新运行（不内嵌前端）。" >&2
    exit 1
  fi
  TAG_FLAG="-tags=embed"
fi

BUILD_OUT=/tmp/sub2api.new
GOOS_FLAG=""
if [ -n "$DEPLOY_HOST" ]; then
  SSH_CMD="ssh -i $DEPLOY_SSH_KEY -p $DEPLOY_SSH_PORT -o ConnectTimeout=10 $DEPLOY_HOST"
  SCP_CMD="scp -i $DEPLOY_SSH_KEY -P $DEPLOY_SSH_PORT -o ConnectTimeout=10"
  REMOTE_ARCH="$($SSH_CMD uname -m)"
  case "$REMOTE_ARCH" in
    x86_64|amd64) GOOS_FLAG="GOOS=linux GOARCH=amd64" ; BUILD_OUT=/tmp/sub2api-linux-amd64.new ;;
    aarch64|arm64) GOOS_FLAG="GOOS=linux GOARCH=arm64" ; BUILD_OUT=/tmp/sub2api-linux-arm64.new ;;
    *) echo "不支持的远端架构: $REMOTE_ARCH" >&2; exit 1 ;;
  esac
  echo "==> 远端架构: $REMOTE_ARCH"
fi

# 版本后缀自动递增：读远端当前版本，同基准版本时计数器 +1，避免版本号倒退
# （如远端 0.1.176-guard.3，同基准升级后为 0.1.176-guard.4）。
SUFFIX_CNT=1
if [ -n "$DEPLOY_HOST" ]; then
  REMOTE_VER="$($SSH_CMD "\"$REMOTE_BIN_DIR/sub2api\" -version" 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+-[A-Za-z0-9._-]+' | head -1 || true)"
  if [[ "$REMOTE_VER" == "${NEW_VER}-${VERSION_SUFFIX}."* ]]; then
    REMOTE_CNT="${REMOTE_VER##*.}"
    if [[ "$REMOTE_CNT" =~ ^[0-9]+$ ]]; then
      SUFFIX_CNT=$((REMOTE_CNT + 1))
    fi
  fi
fi
STAMP="${NEW_VER}-${VERSION_SUFFIX}.${SUFFIX_CNT}"

echo "==> 构建 $STAMP (tags: ${TAG_FLAG:-none})"
CGO_ENABLED=0 $GOOS_FLAG "$GO" build $TAG_FLAG -trimpath \
  -ldflags "-s -w -X main.Version=$STAMP -X main.Commit=$(git rev-parse --short HEAD)" \
  -o "$BUILD_OUT" ./cmd/server

if [ -n "$DEPLOY_HOST" ]; then
  # —— 模式 A：远程部署 ——
  echo "==> 上传到 $DEPLOY_HOST:/tmp/sub2api-new"
  $SCP_CMD "$BUILD_OUT" "$DEPLOY_HOST:/tmp/sub2api-new"

  echo "==> 远端备份 + 原子替换 + 重启"
  $SSH_CMD bash -s <<REMOTE
set -euo pipefail
BIN_DIR="$REMOTE_BIN_DIR"
SERVICE="$REMOTE_SERVICE"
cp -a "\$BIN_DIR/sub2api" "\$BIN_DIR/sub2api.bak-\$(date +%Y%m%d%H%M%S)"
mv /tmp/sub2api-new "\$BIN_DIR/sub2api.new"
chown --reference="\$BIN_DIR/sub2api" "\$BIN_DIR/sub2api.new" 2>/dev/null || true
chmod --reference="\$BIN_DIR/sub2api" "\$BIN_DIR/sub2api.new"
mv "\$BIN_DIR/sub2api.new" "\$BIN_DIR/sub2api"
systemctl restart "\$SERVICE"
sleep 5
if ! systemctl is-active --quiet "\$SERVICE"; then
  LATEST="\$(ls -1t "\$BIN_DIR"/sub2api.bak-* 2>/dev/null | head -1)"
  if [ -n "\$LATEST" ]; then
    cp -a "\$LATEST" "\$BIN_DIR/sub2api"
    systemctl restart "\$SERVICE"
  fi
  echo "REMOTE_RESTART_FAILED"
  exit 1
fi
# 只保留最近 2 个备份，避免小磁盘被撑满
ls -1t "\$BIN_DIR"/sub2api.bak-* 2>/dev/null | tail -n +3 | xargs -r rm -f
echo "REMOTE_RESTART_OK"
REMOTE

  echo "==> 远端验证"
  $SSH_CMD "\"$REMOTE_BIN_DIR/sub2api\" -version" || true
  $SSH_CMD "journalctl -u $REMOTE_SERVICE -n 60 | grep -i BillingGuard | tail -2" || true
  echo "升级部署完成: $STAMP"
else
  # —— 模式 B：本机（服务器上）自构建自部署 ——
  BIN_DIR=${BIN_DIR:-/opt/sub2api}
  SERVICE=${SERVICE:-sub2api}
  TS="$(date +%Y%m%d%H%M%S)"
  BACKUP="$BIN_DIR/sub2api.bak-$TS"
  cp -a "$BIN_DIR/sub2api" "$BACKUP"
  mv "$BUILD_OUT" "$BIN_DIR/sub2api.new"
  chmod 0755 "$BIN_DIR/sub2api.new"
  mv "$BIN_DIR/sub2api.new" "$BIN_DIR/sub2api"

  echo "==> 重启 $SERVICE"
  systemctl restart "$SERVICE"
  sleep 5
  if systemctl is-active --quiet "$SERVICE"; then
    echo "升级成功: $STAMP"
    echo "备份文件: $BACKUP（确认稳定后可删除）"
    ls -1t "$BIN_DIR"/sub2api.bak-* 2>/dev/null | tail -n +3 | xargs -r rm -f
  else
    echo "启动失败，回滚到备份..." >&2
    cp -a "$BACKUP" "$BIN_DIR/sub2api"
    systemctl restart "$SERVICE"
    echo "已回滚，请检查: journalctl -u $SERVICE -n 100" >&2
    exit 1
  fi
fi
