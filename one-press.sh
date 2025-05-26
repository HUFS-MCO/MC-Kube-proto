#!/bin/bash

# 사용법 안내
if [ -z "$1" ]; then
  echo "사용법: $0 <이미지명:태그>"
  echo "예: $0 woya031/mckube:0.0.3"
  exit 1
fi

IMG="$1"

echo "[1/3] Docker Build 시작: $IMG"
make docker-build IMG=$IMG || { echo "docker-build 실패"; exit 1; }

echo "[2/3] Docker Push 시작: $IMG"
make docker-push IMG=$IMG || { echo "docker-push 실패"; exit 1; }

echo "[3/3] Kubernetes Deploy 시작: $IMG"
make deploy IMG=$IMG || { echo "deploy 실패"; exit 1; }

echo "모든 작업 완료"