#!/usr/bin/env bash
set -euo pipefail

# ===============================
# cpu-util-sender systemd installer (no args)
# ===============================

SERVICE_NAME="cpu-util-sender"
BIN_PATH="/usr/local/bin/${SERVICE_NAME}"
ENV_DIR="/etc/${SERVICE_NAME}"
ENV_FILE="${ENV_DIR}/env"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

log() { echo -e "[${SERVICE_NAME}] $*"; }
die() { echo -e "[${SERVICE_NAME}] ERROR: $*" >&2; exit 1; }

# ---- prerequisites
command -v go >/dev/null 2>&1 || die "Go가 필요합니다. (예: sudo apt-get install -y golang-go)"
command -v systemctl >/dev/null 2>&1 || die "systemd 환경이 필요합니다."
command -v sudo >/dev/null 2>&1 || die "sudo 가 필요합니다."
[[ -f "./main.go" ]] || die "현재 디렉토리에 main.go가 없습니다."

# ---- defaults (설치 후 /etc/.../env 로 수정 가능)
DEFAULT_CONTROLLER_URL="${CONTROLLER_URL:-http://127.0.0.1:8080/report}"
DEFAULT_INTERVAL="${INTERVAL:-5s}"
DEFAULT_PROC_PATH="${PROC_PATH:-/proc/stat}"

# hostname 을 소문자로 강제 변환
RAW_NODE_NAME="$(hostname -f 2>/dev/null || hostname)"
DEFAULT_NODE_NAME=$(echo "$RAW_NODE_NAME" | tr 'A-Z' 'a-z')

log "설치 매개변수"
echo "  CONTROLLER_URL = ${DEFAULT_CONTROLLER_URL}"
echo "  INTERVAL       = ${DEFAULT_INTERVAL}"
echo "  PROC_PATH      = ${DEFAULT_PROC_PATH}"
echo "  NODE_NAME      = ${DEFAULT_NODE_NAME}"

# ---- build (일반 권한으로 임시 위치에 빌드)
log "빌드: ./main.go → (임시 바이너리)"
TMP_BIN="$(mktemp -p . ${SERVICE_NAME}.XXXXXX)"
trap 'rm -f "${TMP_BIN}" 2>/dev/null || true' EXIT
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${TMP_BIN}" main.go

# ---- privileged install (root로 시스템 경로 배치)
log "바이너리 설치: ${BIN_PATH}"
sudo install -D -o root -g root -m 0755 "${TMP_BIN}" "${BIN_PATH}"

# ---- 환경 파일
log "환경 파일 생성: ${ENV_FILE}"
sudo mkdir -p "${ENV_DIR}"
sudo tee "${ENV_FILE}" >/dev/null <<EOF
# cpu-util-sender environment
CONTROLLER_URL="${DEFAULT_CONTROLLER_URL}"
INTERVAL="${DEFAULT_INTERVAL}"
PROC_PATH="${DEFAULT_PROC_PATH}"
NODE_NAME="${DEFAULT_NODE_NAME}"
# 수정 후: sudo systemctl restart ${SERVICE_NAME}
EOF
sudo chown root:root "${ENV_FILE}"
sudo chmod 0644 "${ENV_FILE}"

# ---- systemd unit (root 실행 + KUBECONFIG/HOME 지정)
log "systemd 유닛 생성: ${UNIT_FILE}"
sudo tee "${UNIT_FILE}" >/dev/null <<'EOF'
[Unit]
Description=CPU util-sender (reads /proc/stat and posts to custom controller)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
Environment=HOME=/root
Environment=KUBECONFIG=/root/.kube/config
EnvironmentFile=/etc/cpu-util-sender/env
ExecStart=/usr/local/bin/cpu-util-sender \
  -controller-url="${CONTROLLER_URL}" \
  -interval="${INTERVAL}" \
  -proc="${PROC_PATH}" \
  -node="${NODE_NAME}" \
  -log-json=true

Restart=always
RestartSec=2s

[Install]
WantedBy=multi-user.target
EOF
sudo chown root:root "${UNIT_FILE}"
sudo chmod 0644 "${UNIT_FILE}"

# ---- enable & start
log "systemd 리로드 및 서비스 활성화"
sudo systemctl daemon-reload
sudo systemctl enable --now "${SERVICE_NAME}.service"

sleep 1
log "설치 완료 ✅"
echo "상태 확인:    sudo systemctl status ${SERVICE_NAME} --no-pager"
echo "실시간 로그:  journalctl -u ${SERVICE_NAME} -f"
echo "환경 수정:    sudo nano ${ENV_FILE}  (수정 후: sudo systemctl restart ${SERVICE_NAME})"