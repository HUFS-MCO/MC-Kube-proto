# Runtime Escalation 기능

## 개요

MC-Kube는 이제 런타임을 두 단계로 나누어 관리합니다:
- **runtime_low**: 초기 보수적인 런타임 값
- **runtime_hi**: Overrun 감지 후 상승된 런타임 값

## 동작 방식

### 1. 초기 상태 (runtime_low)
- Pod가 처음 생성되면 `runtime_low` 값으로 RT 설정이 적용됩니다
- 이는 보수적인 값으로, 리소스를 절약하면서 작업을 시작합니다

### 2. Overrun 감지
- eBPF 기반 모니터링이 컨테이너의 Overrun 이벤트를 감지합니다
- Overrun 이벤트는 `/overrun` 엔드포인트로 전송됩니다

### 3. Runtime 상승 (runtime_hi)
- Overrun 감지 시 자동으로 `runtime_hi`로 전환됩니다
- McKube CR의 Status가 업데이트됩니다:
  - `currentRuntime`: "hi"
  - `lastOverrunTime`: 마지막 overrun 시간

### 4. CPU 회복 감지 (NEW!)
- `cpu_util_sender`가 CPU 사용률이 90% 미만으로 떨어진 후 5초 경과 시 `isCpuBusy=false` 전송
- Controller가 이를 감지하면 노드의 모든 RT Pod를 자동으로 `runtime_low`로 복귀
- McKube CR의 Status가 업데이트됩니다:
  - `currentRuntime`: "low"

## CRD 정의

```yaml
apiVersion: mcoperator.sdv.com/v1
kind: McKube
metadata:
  name: example-config
  namespace: default
spec:
  podname: "example-pod"
  criticality: "high"
  rtSettings:
    period: 1000000        # RT period (microseconds)
    runtime_low: 300000    # 초기 런타임 (300ms)
    runtime_hi: 500000     # 상승된 런타임 (500ms)
    core: "2-3"            # CPU 코어 범위 (optional)
```

## 사용 예제

### 1. McKube CR 생성

```bash
kubectl apply -f - <<EOF
apiVersion: mcoperator.sdv.com/v1
kind: McKube
metadata:
  name: my-rt-app-config
  namespace: default
spec:
  podname: "my-rt-app"
  rtSettings:
    period: 1000000
    runtime_low: 200000   # 보수적 시작
    runtime_hi: 400000    # Overrun 시 상승
    core: "1"
EOF
```

### 2. Pod 생성

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: my-rt-app
  namespace: default
  labels:
    sdv.com: "my-app"  # RT 워크로드 식별 라벨
spec:
  containers:
  - name: app
    image: my-rt-image:latest
EOF
```

### 3. 상태 확인

```bash
# McKube CR 상태 확인
kubectl get mckube my-rt-app-config -o yaml

# Status 예시:
# status:
#   currentRuntime: "hi"
#   lastOverrunTime: "2025-10-22T10:30:00Z"

# Pod annotation 확인
kubectl get pod my-rt-app -o jsonpath='{.metadata.annotations}'
```

## 동작 흐름

```
1. Pod 생성
   ↓
2. Webhook이 Pod를 intercept
   ↓
3. McKube CR에서 RTSettings 조회
   ↓
4. runtime_low로 초기 RT 설정 적용
   ↓
5. Pod annotation 추가:
   - mckube.io/rt-current: "low"
   - mckube.io/rt-runtime-low: "300000"
   - mckube.io/rt-runtime-hi: "500000"
   ↓
6. eBPF 모니터링 시작
   ↓
7. [Overrun 감지]
   ↓
8. Controller의 handleOverrunEvent() 호출
   ↓
9. runtime_hi로 런타임 변경
   ↓
10. McKube CR Status 업데이트 (currentRuntime: "hi")
   ↓
11. [CPU 사용률 정상화]
    - CPU < 90% 지속 5초
    ↓
12. cpu_util_sender가 isCpuBusy=false 전송
    ↓
13. Controller의 handleCPURecovery() 호출
    ↓
14. 모든 RT Pod를 runtime_low로 복귀
    ↓
15. McKube CR Status 업데이트 (currentRuntime: "low")
```

## 모니터링

### 로그 확인

```bash
# Controller 로그에서 overrun 이벤트 확인
kubectl logs -n mc-kube-system deployment/mc-kube-controller-manager | grep Overrun

# 예시 출력:
# Overrun Event Detected: node=worker1, containerID=abc123, timestamp=1234567890
# Escalating pod runtime from low to hi: pod=my-rt-app, runtime_low=300000, runtime_hi=500000
# Successfully escalated pod runtime to hi: pod=my-rt-app, newRuntime=500000
```

### Metrics

현재 runtime 상태를 확인할 수 있는 annotation:
- `mckube.io/rt-current`: "low" 또는 "hi"

## 주의사항

1. **runtime_low < runtime_hi**: runtime_low는 항상 runtime_hi보다 작아야 합니다
2. **sdv.com 라벨**: Pod는 반드시 `sdv.com` 라벨을 가져야 RT 처리됩니다
3. **Overrun 감지**: eBPF 모니터링이 활성화되어 있어야 합니다
4. **양방향 전환**: 이제 low↔hi 양방향 전환을 모두 지원합니다
   - Overrun 감지 → runtime_hi로 상승
   - CPU 정상화 (isCpuBusy=false) → runtime_low로 복귀
5. **cpu_util_sender 필수**: CPU 상태 모니터링을 위해 cpu_util_sender가 실행 중이어야 합니다

## 트러블슈팅

### Runtime이 변경되지 않는 경우

1. McKube CR Status 확인:
   ```bash
   kubectl describe mckube <name>
   ```

2. Pod annotation 확인:
   ```bash
   kubectl get pod <name> -o yaml | grep mckube.io
   ```

3. Controller 로그 확인:
   ```bash
   kubectl logs -n mc-kube-system deployment/mc-kube-controller-manager
   ```

4. Overrun Listener 확인:
   ```bash
   # Controller가 8090 포트에서 listening 하는지 확인
   kubectl logs -n mc-kube-system deployment/mc-kube-controller-manager | grep "Starting overrun listener"
   ```

## 향후 개선 사항

- [x] hi→low 자동 복귀 기능 (CPU 정상화 시)
- [ ] 다단계 runtime (low/medium/hi)
- [ ] Overrun 빈도에 따른 동적 조정
- [ ] Prometheus metrics 추가
- [ ] 복귀 지연 시간 설정 가능 (현재 5초 고정)
