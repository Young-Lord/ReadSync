#!/usr/bin/env bash
# 一键部署脚本：commit(可选) + push → 等待 GitHub Actions 构建 → 下载 Windows 构建 → 部署到 Windows 服务器
#
# 用法：
#   ./deploy.sh                          # 仅 push 当前 HEAD 并部署
#   ./deploy.sh "commit message"         # 先提交所有更改，再 push 并部署
#
# 流程依据 DEPLOY_NOTES.md，任何一步失败立即中止。

set -euo pipefail

REPO="Young-Lord/ReadSync"
WORKFLOW="build.yml"
BRANCH="master"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/latest/readsync-windows-amd64.exe"
SSH_HOST="sdgh"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)
SERVICE_NAME="ReadSyncByNiko"
REMOTE_EXE_PATH="E:/WSL/server/ReadSync/ReadSync.exe"

TMP_EXE="$(mktemp --suffix=.exe)"
trap 'rm -f "$TMP_EXE"' EXIT

info() { printf '\033[1;34m[deploy]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[deploy] 错误：\033[0m %s\n' "$*" >&2; exit 1; }

ssh_cmd() { ssh "${SSH_OPTS[@]}" "$SSH_HOST" "$@"; }

# ---------- 1. 可选：提交 ----------
if [[ $# -ge 1 && -n "$1" ]]; then
  git add -A
  if git diff --cached --quiet; then
    info "没有待提交的更改，跳过 commit"
  else
    info "提交更改：$1"
    git commit -m "$1"
  fi
fi

# ---------- 2. push ----------
info "推送 ${BRANCH} 到 origin ..."
git push

HEAD_SHA="$(git rev-parse HEAD)"

# ---------- 3. 等待 CI 构建 ----------
info "等待 workflow 构建（${HEAD_SHA:0:7}）..."
RUN_ID=""
for _ in $(seq 1 15); do
  RUN_ID="$(gh run list --repo "$REPO" --workflow "$WORKFLOW" --branch "$BRANCH" --limit 10 \
    --json databaseId,headSha --jq ".[] | select(.headSha == \"$HEAD_SHA\") | .databaseId" | head -n1)"
  [[ -n "$RUN_ID" ]] && break
  sleep 5
done
[[ -n "$RUN_ID" ]] || die "未找到 commit ${HEAD_SHA:0:7} 对应的 workflow run"

info "workflow run #${RUN_ID} 构建中，等待完成（失败将中止）..."
gh run watch "$RUN_ID" --repo "$REPO" --exit-status

# ---------- 4. 下载最新 Windows 构建 ----------
info "下载最新构建 ..."
curl -fL --retry 3 --retry-delay 2 "$DOWNLOAD_URL" -o "$TMP_EXE"
if [[ "$(head -c 2 "$TMP_EXE")" != "MZ" ]]; then
  die "下载的文件不是有效的 PE 可执行文件（可能下载到了错误页面）"
fi
info "已下载构建（$(stat -c %s "$TMP_EXE") 字节）"

# ---------- 5. 部署到 Windows 服务器 ----------
info "启动 chisel-ssh 隧道 ..."
systemctl --user start chisel-ssh.service

info "等待 ssh 到 ${SSH_HOST} 可用 ..."
for _ in $(seq 1 12); do
  ssh_cmd "echo ok" >/dev/null 2>&1 && break
  sleep 5
done
ssh_cmd "echo ok" >/dev/null 2>&1 || die "无法连接 ${SSH_HOST}"

info "停止服务 ${SERVICE_NAME} ..."
if ! ssh_cmd "nssm stop ${SERVICE_NAME}"; then
  STATUS="$(ssh_cmd "nssm status ${SERVICE_NAME}" 2>/dev/null || true)"
  [[ "$STATUS" == *SERVICE_STOPPED* ]] || die "停止服务失败：${STATUS:-未知状态}"
  info "服务本就处于停止状态"
fi

info "上传并覆盖 ${REMOTE_EXE_PATH} ..."
scp -q "$TMP_EXE" "${SSH_HOST}:${REMOTE_EXE_PATH}"

info "启动服务 ${SERVICE_NAME} ..."
if ! ssh_cmd "nssm start ${SERVICE_NAME}"; then
  STATUS="$(ssh_cmd "nssm status ${SERVICE_NAME}" 2>/dev/null || true)"
  [[ "$STATUS" == *SERVICE_RUNNING* ]] || die "启动服务失败：${STATUS:-未知状态}"
  info "服务已在运行"
fi

info "部署完成"
