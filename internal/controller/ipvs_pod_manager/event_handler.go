package ipvspodmanager

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcoperatorv1 "mc-kube/api/v1"
)

// EventHandler handles all event-driven operations for McKube RT controller
type EventHandler struct {
	client.Client
	DataCollector *DataCollector
	PodSpecEditor *PodSpecEditor
	// RT request sender function
	SendRTRequest func(nodeIP string, req CgroupRequest) error
}

// NodePressureState tracks pressure state for each node
type NodePressureState struct {
	AboveSec       int
	Tiers          []string
	CurrentTierIdx int
	CurrentTier    string
	PerTier        map[string]*perTierState
}

type perTierState struct {
	ElapsedSec      int
	DegradeCount    int // Degradation 시도 횟수
	EvictTried      bool
	MissingTicks    int
	LastDegradeTime int       // 마지막 Degradation 시도 타임 스탬프
	LastSeenTime    time.Time // 마지막으로 CPU 압박이 감지된 시간
}

// CgroupRequest for RT daemon communication
type CgroupRequest struct {
	ContainerID string  `json:"container_id"`
	Period      int     `json:"period"`
	Runtime     int     `json:"runtime"`
	Core        *string `json:"core,omitempty"`
}

// Constants used by event handler
const (
	targetNamespace      = "default"
	annUsageKey          = "mckube.sdv.com/cpu-usage"
	annDurKey            = "mckube.sdv.com/cpu-over90-duration-s"
	annCpuBusyKey        = "mckube.sdv.com/isCpuBusy"
	minMilli             = int64(10)
	tierMissingTolerance = 2
)

// Global variables for state management
var (
	pressureState   = make(map[string]*NodePressureState)
	processingNodes = make(map[string]bool)
	processingMutex sync.RWMutex

	// Pod runtime state tracking
	podRuntimeState   = make(map[string]string) // podName -> "low" or "hi"
	runtimeStateMutex sync.RWMutex

	// Criticality ranking
	criticalityRank = map[string]int{
		"Low":    0,
		"Middle": 1,
		"High":   2,
	}
)

// ===================== 이벤트 기반 처리 함수들 =====================

// HandleNodeCPUPressure handles node CPU pressure when usage is above 90%
func (eh *EventHandler) HandleNodeCPUPressure(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPUPressureHandler", "EventDriven", "node", nodeName)

	node := &corev1.Node{}
	if err := eh.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		logger.Error(err, "Failed to get node")
		return
	}

	ann := node.GetAnnotations()
	if ann == nil {
		logger.V(1).Info("Node has no annotations")
		return
	}

	cpuUsageStr := strings.TrimSpace(ann[annUsageKey])
	if cpuUsageStr == "" {
		logger.Error(fmt.Errorf("missing annotation %q", annUsageKey), "Failed to get CPU usage from node")
		return
	}

	cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
	if err != nil {
		logger.Error(err, "Failed to parse CPU usage from node annotation", "value", cpuUsageStr)
		return
	}

	overSecStr := strings.TrimSpace(ann[annDurKey])
	overSec := 0
	if overSecStr != "" {
		if sec, err := strconv.ParseInt(overSecStr, 10, 64); err == nil {
			if sec >= 0 {
				overSec = int(sec)
			}
		} else {
			logger.V(1).Info("Failed to parse over90 duration; treating as 0", "err", err.Error(), "value", overSecStr)
		}
	}

	rtData, err := eh.DataCollector.GetRealTimeData(ctx)
	if err != nil {
		logger.Error(err, "Failed to get RT data")
		return
	}

	podList := &corev1.PodList{}
	if err := eh.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
		logger.Error(err, "Failed to list pods", "namespace", targetNamespace)
		return
	}

	var nodePods []*corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName == nodeName {
			nodePods = append(nodePods, p)
		}
	}

	if len(nodePods) == 0 {
		logger.V(1).Info("No pods found on node")
		return
	}

	podMilli, err := eh.DataCollector.ListPodMilliCPUByNode(ctx, targetNamespace, node)
	if err != nil {
		logger.Error(err, "Failed to list pod milliCPU (requests-based)")
		return
	}

	state := pressureState[nodeName]
	if state == nil {
		state = &NodePressureState{
			AboveSec:       0,
			Tiers:          nil,
			CurrentTierIdx: 0,
			CurrentTier:    "",
			PerTier:        map[string]*perTierState{},
		}
		pressureState[nodeName] = state
	}
	state.Tiers = eh.collectSortedTiers(nodePods, rtData)
	state.AboveSec = overSec

	eh.reindexCurrentTier(state)

	// Tier가 없으면 스킵
	if len(state.Tiers) == 0 {
		logger.V(0).Info("No criticality tiers detected on node; skipping",
			"cpu(%)", fmt.Sprintf("%.1f", cpuUsage), "over90Sec", overSec)
		return
	}

	// Current tier 설정 (여전히 비어있다면 안전 초기화)
	if state.CurrentTier == "" || state.CurrentTierIdx >= len(state.Tiers) {
		state.CurrentTierIdx = 0
		state.CurrentTier = state.Tiers[0]
		if _, ok := state.PerTier[state.CurrentTier]; !ok {
			state.PerTier[state.CurrentTier] = &perTierState{}
		}
		logger.V(0).Info("Pinning current tier to lowest detected tier",
			"tier", state.CurrentTier)
	}

	// 현 Criticality에 대해 조치 가능한 Pod에 대한 처리 진행
	eh.processCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
}

// HandleCPURecovery handles CPU recovery when isCpuBusy=false
func (eh *EventHandler) HandleCPURecovery(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPURecovery", "Recovery", "node", nodeName)
	logger.V(0).Info("CPU recovered (isCpuBusy=false), reverting pods to runtime_low")

	// 노드의 pressure state 리셋
	if _, exists := pressureState[nodeName]; exists {
		logger.V(0).Info("Resetting node pressure state")
		delete(pressureState, nodeName)
	}

	// 해당 노드의 모든 Pod 조회
	podList := &corev1.PodList{}
	if err := eh.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
		logger.Error(err, "Failed to list pods", "namespace", targetNamespace)
		return
	}

	var nodePods []*corev1.Pod
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName == nodeName {
			// sdv.com 라벨이 있는 RT Pod만 처리
			if p.Labels != nil && p.Labels["sdv.com"] != "" {
				nodePods = append(nodePods, p)
			}
		}
	}

	if len(nodePods) == 0 {
		logger.V(1).Info("No RT pods found on node")
		return
	}

	logger.V(0).Info("Found RT pods to revert", "count", len(nodePods))

	// 각 Pod에 대해 runtime_low로 복귀 처리
	for _, pod := range nodePods {
		// 현재 runtime 상태 확인
		runtimeStateMutex.RLock()
		currentState := podRuntimeState[pod.Name]
		runtimeStateMutex.RUnlock()

		if currentState != "hi" {
			// 이미 low 상태이거나 설정되지 않음
			logger.V(1).Info("Pod not in hi state, skipping", "pod", pod.Name)
			continue
		}

		// McKube CR 조회
		mckubeList := &mcoperatorv1.McKubeList{}
		if err := eh.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
			logger.Error(err, "Failed to list McKube resources", "pod", pod.Name)
			continue
		}

		var targetMcKube *mcoperatorv1.McKube
		for i := range mckubeList.Items {
			if mckubeList.Items[i].Spec.PodName == pod.Name {
				targetMcKube = &mckubeList.Items[i]
				break
			}
		}

		if targetMcKube == nil || targetMcKube.Spec.RTSettings == nil {
			logger.V(1).Info("No McKube CR or RT settings found for pod", "pod", pod.Name)
			continue
		}

		// runtime_hi → runtime_low로 복귀
		logger.V(0).Info("Reverting pod runtime from hi to low",
			"pod", pod.Name,
			"runtime_hi", targetMcKube.Spec.RTSettings.RuntimeHi,
			"runtime_low", targetMcKube.Spec.RTSettings.RuntimeLow)

		// 모든 컨테이너에 runtime_low 적용
		nodeIP := pod.Status.HostIP
		if nodeIP == "" {
			logger.Error(fmt.Errorf("node IP not available"), "Failed to get node IP", "pod", pod.Name)
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.ContainerID == "" {
				continue
			}

			req := CgroupRequest{
				ContainerID: cs.ContainerID,
				Period:      targetMcKube.Spec.RTSettings.Period,
				Runtime:     targetMcKube.Spec.RTSettings.RuntimeLow,
				Core:        targetMcKube.Spec.RTSettings.Core,
			}

			if err := eh.SendRTRequest(nodeIP, req); err != nil {
				logger.Error(err, "Failed to apply runtime_low to container",
					"containerID", cs.ContainerID,
					"pod", pod.Name)
				continue
			}

			logger.V(0).Info("Successfully reverted container to runtime_low",
				"pod", pod.Name,
				"container", cs.Name,
				"runtime", targetMcKube.Spec.RTSettings.RuntimeLow)
		}

		// 상태 업데이트
		runtimeStateMutex.Lock()
		podRuntimeState[pod.Name] = "low"
		runtimeStateMutex.Unlock()

		// McKube CR 상태 업데이트
		targetMcKube.Status.CurrentRuntime = "low"
		if err := eh.Status().Update(ctx, targetMcKube); err != nil {
			logger.Error(err, "Failed to update McKube status", "pod", pod.Name)
		}
	}

	logger.V(0).Info("CPU recovery completed", "podsReverted", len(nodePods))
}

// processCurrentTier processes the current criticality tier with degradation and eviction logic
func (eh *EventHandler) processCurrentTier(ctx context.Context, logger logr.Logger, state *NodePressureState, nodePods []*corev1.Pod, rtData map[string]RealTimeData, podMilli map[string]int64, overSec int, cpuUsage float64) {
	curTier := state.CurrentTier
	pts := state.PerTier[curTier]
	if pts == nil {
		pts = &perTierState{}
		state.PerTier[curTier] = pts
	}

	targets := FilterPodsByCriticality(nodePods, rtData, curTier)
	targetsCount := len(targets)

	logger.V(0).Info("Node pressure snapshot",
		"cpu(%)", fmt.Sprintf("%.1f", cpuUsage),
		"over90Sec", overSec,
		"currentTier", curTier,
		"targetsCount", targetsCount,
		"degradeCount", pts.DegradeCount,
		"evictTried", pts.EvictTried,
	)

	// 동일 티어에서 처리될 Pod가 없다면, 다음 티어로 격상
	if targetsCount == 0 {
		pts.MissingTicks++
		logger.V(0).Info("No targets in current tier; incrementing MissingTicks",
			"tier", curTier, "missingTicks", pts.MissingTicks)

		if pts.MissingTicks >= tierMissingTolerance {
			if nextTier, ok := eh.nextHigherTier(curTier, state.Tiers); ok {
				logger.V(0).Info("Escalating to next criticality tier",
					"fromTier", curTier, "toTier", nextTier)
				state.CurrentTier = nextTier

				for i, t := range state.Tiers {
					if t == nextTier {
						state.CurrentTierIdx = i
						break
					}
				}
				if _, ok := state.PerTier[nextTier]; !ok {
					state.PerTier[nextTier] = &perTierState{}
				}
				// 재귀적으로 다음 티어 처리
				eh.processCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
			}
		}
		return
	}

	pts.MissingTicks = 0

	// CPU 임계점 지속 시간이 1초 이상이라면, Graceful degradation 반복 수행
	if overSec >= 1 && !pts.EvictTried {
		// 3초마다 또는 처음 시도할 때 degradation 수행
		shouldDegrade := pts.DegradeCount == 0 || (overSec-pts.LastDegradeTime >= 3)

		if shouldDegrade {
			top := PickHighestCPUFromMilli(targets, podMilli)
			if top != nil {
				// 현재 CPU 요청량 확인
				currentMilli := int64(0)
				if podMilli[top.Name] > 0 {
					currentMilli = podMilli[top.Name]
				}

				// minMilli 이하로는 degradation하지 않음
				if currentMilli > minMilli {
					degradeRatio := 0.2 // 20% (Graceful degradation 비율)
					logger.V(0).Info("Stage 1: Attempting graceful degradation on heaviest pod",
						"tier", curTier,
						"pod", top.Name,
						"over90Sec", overSec,
						"degradeAttempt", pts.DegradeCount+1,
						"currentMilliCPU", currentMilli,
						"degradeRatio", fmt.Sprintf("%.1f%%", degradeRatio*100))

					if err := eh.PodSpecEditor.DegradePodRequests(ctx, top, degradeRatio); err != nil {
						logger.Error(err, "Graceful degradation failed",
							"tier", curTier, "pod", top.Name, "attempt", pts.DegradeCount+1)
					} else {
						pts.DegradeCount++
						pts.LastDegradeTime = overSec
						logger.V(0).Info("Stage 1: Graceful degradation applied successfully",
							"tier", curTier,
							"pod", top.Name,
							"totalDegradations", pts.DegradeCount)
					}
				} else {
					logger.V(0).Info("Pod already at minimum CPU requests; no further degradation possible",
						"tier", curTier, "pod", top.Name, "currentMilliCPU", currentMilli, "minMilli", minMilli)
					pts.DegradeCount = 999 // 더 이상 degradation 불가
				}
			}
		}
	}

	// 90% 지속 시간이 10초 이상이라면, eviction
	if overSec >= 10 && pts.DegradeCount > 0 && !pts.EvictTried {
		victim := PickHighestCPUFromMilli(targets, podMilli)
		if victim != nil {
			logger.V(0).Info("Stage 2: Immediate eviction due to sustained high CPU",
				"tier", curTier, "pod", victim.Name, "over90Sec", overSec)
			if err := eh.evictPod(ctx, victim); err != nil {
				logger.Error(err, "Eviction failed",
					"tier", curTier, "pod", victim.Name)
				pts.EvictTried = true
			} else {
				logger.V(0).Info("Stage 2: Eviction succeeded",
					"tier", curTier, "pod", victim.Name)
				pts.EvictTried = true
			}
		}
	}

	// 처리 완료 후 더 이상 actionable한 Pod가 없으면 다음 tier로 에스컬레이션
	if pts.DegradeCount > 0 && pts.EvictTried {
		actionable := HasActionableInTier(nodePods, rtData, curTier, minMilli)
		if !actionable {
			if nextTier, ok := eh.nextHigherTier(curTier, state.Tiers); ok {
				logger.V(0).Info("Stage 3: Escalating to next criticality tier",
					"fromTier", curTier, "toTier", nextTier)
				state.CurrentTier = nextTier

				for i, t := range state.Tiers {
					if t == nextTier {
						state.CurrentTierIdx = i
						break
					}
				}
				if _, ok := state.PerTier[nextTier]; !ok {
					state.PerTier[nextTier] = &perTierState{}
				}
			}
		}
	}
}

// FindObjectsForNode handles node annotation changes for CPU pressure detection
func (eh *EventHandler) FindObjectsForNode(ctx context.Context, node client.Object) []reconcile.Request {
	nodeObj := node.(*corev1.Node)

	ann := nodeObj.GetAnnotations()
	if ann == nil {
		return []reconcile.Request{}
	}

	cpuUsageStr := strings.TrimSpace(ann[annUsageKey])
	if cpuUsageStr == "" {
		return []reconcile.Request{}
	}
	cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
	if err != nil {
		return []reconcile.Request{}
	}

	// isCpuBusy 상태 확인
	isCpuBusyStr := strings.TrimSpace(ann[annCpuBusyKey])
	isCpuBusy := true // 기본값 true
	if isCpuBusyStr != "" {
		if busy, err := strconv.ParseBool(isCpuBusyStr); err == nil {
			isCpuBusy = busy
		}
	}

	// 이미 진행중인 노드라면 스킵
	processingMutex.Lock()
	if processingNodes[nodeObj.Name] {
		processingMutex.Unlock()
		return []reconcile.Request{}
	}
	processingNodes[nodeObj.Name] = true
	processingMutex.Unlock()

	logger := log.Log.WithValues("McKube/rt.NodeEvent", "CPU-Event")
	logger.V(0).Info("CPU annotation change detected",
		"node", nodeObj.Name,
		"cpu(%)", fmt.Sprintf("%.1f", cpuUsage),
		"isCpuBusy", isCpuBusy)

	// CPU pressure 처리를 별도 Go Routine에서 비동기로 진행
	go func() {
		defer func() {
			// 처리 플래그 초기화
			processingMutex.Lock()
			delete(processingNodes, nodeObj.Name)
			processingMutex.Unlock()
		}()

		// 백그라운드 처리를 위한 30초 타임아웃
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// isCpuBusy=false인 경우: 모든 Pod를 runtime_low로 복귀
		if !isCpuBusy {
			eh.HandleCPURecovery(bgCtx, nodeObj.Name)
		} else {
			// isCpuBusy=true인 경우: 기존 CPU pressure 처리
			eh.HandleNodeCPUPressure(bgCtx, nodeObj.Name)
		}
	}()

	return []reconcile.Request{}
}

// ===================== Helper functions =====================

func (eh *EventHandler) collectSortedTiers(pods []*corev1.Pod, rtData map[string]RealTimeData) []string {
	seen := map[string]bool{}
	for _, p := range pods {
		app := p.Labels["sdv.com"]
		if app == "" {
			continue
		}
		if rt, ok := rtData[app]; ok {
			if _, ok2 := criticalityRank[rt.Criticality]; ok2 {
				seen[rt.Criticality] = true
			}
		}
	}
	var tiers []string
	for c := range seen {
		tiers = append(tiers, c)
	}
	// ascending by rank
	for i := 0; i < len(tiers); i++ {
		for j := i + 1; j < len(tiers); j++ {
			if criticalityRank[tiers[j]] < criticalityRank[tiers[i]] {
				tiers[i], tiers[j] = tiers[j], tiers[i]
			}
		}
	}
	return tiers
}

func (eh *EventHandler) evictPod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.Log.WithValues("McKube/rt.evictPod", "Eviction")

	// Step 1: Add label "evicted" to the pod before eviction
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels["evicted"] = "true"

	logger.V(0).Info("Adding evicted label to pod",
		"pod", pod.Name,
		"namespace", pod.Namespace)

	// Update the pod with the new label
	if err := eh.Update(ctx, pod); err != nil {
		logger.Error(err, "Failed to add evicted label to pod",
			"pod", pod.Name,
			"namespace", pod.Namespace)
		// Continue with eviction even if labeling fails
	}

	// Step 2: Perform the eviction
	logger.V(0).Info("Evicting pod",
		"pod", pod.Name,
		"namespace", pod.Namespace)

	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	return eh.SubResource("eviction").Create(ctx, pod, ev)
}

// reindexCurrentTier re-synchronizes CurrentTier/Idx with the re-sorted tier list
func (eh *EventHandler) reindexCurrentTier(state *NodePressureState) {
	if len(state.Tiers) == 0 {
		state.CurrentTier = ""
		state.CurrentTierIdx = 0
		return
	}
	// 현재 티어가 목록에 있으면 그 위치로 동기화
	for i, t := range state.Tiers {
		if t == state.CurrentTier {
			state.CurrentTierIdx = i
			return
		}
	}
	// 없으면 최하위(랭크가 가장 낮은) 티어로 리셋
	state.CurrentTierIdx = 0
	state.CurrentTier = state.Tiers[0]
}

// nextHigherTier returns the next higher tier among available tiers
func (eh *EventHandler) nextHigherTier(cur string, tiers []string) (string, bool) {
	curRank, ok := criticalityRank[cur]
	if !ok {
		return "", false
	}
	bestRank := 1 << 30
	best := ""
	for _, t := range tiers {
		if r, ok := criticalityRank[t]; ok && r > curRank && r < bestRank {
			bestRank = r
			best = t
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// GetPodRuntimeState returns the current runtime state of a pod
func GetPodRuntimeState(podName string) string {
	runtimeStateMutex.RLock()
	defer runtimeStateMutex.RUnlock()
	return podRuntimeState[podName]
}

// SetPodRuntimeState sets the runtime state of a pod
func SetPodRuntimeState(podName, state string) {
	runtimeStateMutex.Lock()
	defer runtimeStateMutex.Unlock()
	podRuntimeState[podName] = state
}
