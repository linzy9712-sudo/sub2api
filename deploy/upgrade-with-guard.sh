#!/usr/bin/env bash
# upgrade-with-guard.sh — 维护本地 billingguard 补丁并滚动升级（无需合入官方）
#
# 维护模型：
#   - 官方仓库加一个 remote（默认叫 upstream），fork 里维护一个补丁分支（默认 guard），
#     补丁 = billingguard 组件 + 3 个计费挂载点 + main.go 装配；
#   - 每次官方发新版：本脚本把补丁分支 rebase 到上游最新代码 → 构建 → 备份 → 替换 → 重启；
#   - rebase 冲突时脚本中止，手动解决冲突后重跑即可；
#   - 备份文件与失败自动回滚保证随时可退。
#
# 首次准备（一次性）：
#   git remote add upstream https://github.com/Wei-Shaw/sub2api.git
#   git checkout -b guard            # 把补丁提交放到这个分支
#   git push -u myfork guard        # 在服务器/CI 之外再留一份补丁（灾备）
#
# 用法:
#   sudo ./deploy/upgrade-with-guard.sh                 # rebase 到 upstream/main 并升级
#   sudo ./deploy/upgrade-with-guard.sh v0.1.180        # rebase 到指定 release tag
#   BUILD_TAGS=none sudo ./deploy/upgrade-with-guard.sh # 不内嵌前端（纯 API 网关）
#
# 回滚：
#   sudo cp /opt/sub2api/sub2api.bak-<时间戳> /opt/sub2api/sub2api
#   sudo systemctl restart sub2api
set -euo pipefail

SRC_DIR=${SRC_DIR:-/opt/sub2api/src}      # 源码目录（你的 fork clone）
BIN_DIR=${BIN_DIR:-/opt/sub2api}          # 二进制目录（与 systemd unit 一致）
SERVICE=${SERVICE:-sub2api}
GUARD_BRANCH=${GUARD_BRANCH:-guard}       # 承载补丁的分支
BASE_REMOTE=${BASE_REMOTE:-upstream}      # 官方仓库 remote；未配置时退回 origin
BUILD_TAGS=${BUILD_TAGS:-embed}           # embed=带管理界面; none=纯 API
VERSION_SUFFIX=${VERSION_SUFFIX:-guard}   # 版本戳后缀，如 0.1.180-guard.1
GO=${GO:-go}

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
STAMP="${NEW_VER}-${VERSION_SUFFIX}.1"

# 构建
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

echo "==> 构建 $STAMP (tags: ${TAG_FLAG:-none})"
CGO_ENABLED=0 "$GO" build $TAG_FLAG -trimpath \
  -ldflags "-s -w -X main.Version=$STAMP -X main.Commit=$(git rev-parse --short HEAD)" \
  -o /tmp/sub2api.new ./cmd/server

# 备份 + 原子替换 + 重启
TS="$(date +%Y%m%d%H%M%S)"
BACKUP="$BIN_DIR/sub2api.bak-$TS"
cp -a "$BIN_DIR/sub2api" "$BACKUP"
mv /tmp/sub2api.new "$BIN_DIR/sub2api.new"
chmod 0755 "$BIN_DIR/sub2api.new"
mv "$BIN_DIR/sub2api.new" "$BIN_DIR/sub2api"

echo "==> 重启 $SERVICE"
systemctl restart "$SERVICE"
sleep 5
if systemctl is-active --quiet "$SERVICE"; then
  echo "升级成功: $STAMP"
  echo "备份文件: $BACKUP（确认稳定后可删除）"
  # 只保留最近 2 个脚本备份，避免小磁盘被备份撑满
  ls -1t "$BIN_DIR"/sub2api.bak-* 2>/dev/null | tail -n +3 | xargs -r rm -f
else
  echo "启动失败，回滚到备份..." >&2
  cp -a "$BACKUP" "$BIN_DIR/sub2api"
  systemctl restart "$SERVICE"
  echo "已回滚，请检查: journalctl -u $SERVICE -n 100" >&2
  exit 1
fi
