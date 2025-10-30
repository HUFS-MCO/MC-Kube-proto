package ipvs

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
)

// EventHandler handles CPU pressure and recovery events
type EventHandler struct {
	client.Client
	DataCollector  *DataCollector
	PodSpecHandler *PodSpecHandler
}

// NodePressureState represents the pressure state of a node
type NodePressureState struct {
	AboveSec       int
	Tiers          []string
	CurrentTierIdx int
	CurrentTier    string
	PerTier        map[string]*PerTierState
}

// PerTierState represents the state for each criticality tier
type PerTierState struct {
	ElapsedSec      int
	DegradeCount    int // Degradation 시도 횟수
	EvictTried      bool
	MissingTicks    int
	LastDegradeTime int // 마지막 Degradation 시도 타임 스탬프
}

// Constants for event handling
const (
	AnnUsageKey   = "mckube.sdv.com/cpu-usage"
	AnnDurKey     = "mckube.sdv.com/cpu-over90-duration-s"
	AnnCpuBusyKey = "mckube.sdv.com/isCpuBusy"

	TargetNamespace      = "default"
	TierMissingTolerance = 2
)

// Global state management
var (
	PressureState   = make(map[string]*NodePressureState)
	ProcessingNodes = make(map[string]bool)
	ProcessingMutex sync.RWMutex

	// Runtime state tracking
	PodRuntimeState   = make(map[string]string) // podName -> "low" or "hi"
	RuntimeStateMutex sync.RWMutex

	// Criticality ranking
	CriticalityRank = map[string]int{
		"Low":    0,
		"Middle": 1,
		"High":   2,
	}
)

// NewEventHandler creates a new EventHandler instance
func NewEventHandler(client client.Client, dataCollector *DataCollector, podSpecHandler *PodSpecHandler) *EventHandler {
	return &EventHandler{
		Client:         client,
		DataCollector:  dataCollector,
		PodSpecHandler: podSpecHandler,
	}
}

// ===================== 이벤트 기반 처리 함수들 =====================

// HandleNodeCPUPressure :  노드의 CPU 사용률이 90% 이상일 때, annotation 및 CR 정보를 파싱하여 processCurrentTier()에 전달하는 함수
func (e *EventHandler) HandleNodeCPUPressure(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPUPressureHandler", "EventDriven", "node", nodeName)

	node := &corev1.Node{}
	if err := e.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		logger.Error(err, "Failed to get node")
		return
	}

	ann := node.GetAnnotations()
	if ann == nil {
		return
	}

	cpuUsageStr := strings.TrimSpace(ann[AnnUsageKey])
	if cpuUsageStr == "" {
		logger.Error(fmt.Errorf("missing annotation %q", AnnUsageKey), "Failed to get CPU usage from node")
		return
	}

	cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
	if err != nil {
		logger.Error(err, "Failed to parse CPU usage from node annotation", "value", cpuUsageStr)
		return
	}

	overSecStr := strings.TrimSpace(ann[AnnDurKey])
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

	rtData, err := e.DataCollector.GetRealTimeData(ctx)
	if err != nil {
		logger.Error(err, "Failed to get RT data")
		return
	}

	podList := &corev1.PodList{}
	if err := e.List(ctx, podList, client.InNamespace(TargetNamespace)); err != nil {
		logger.Error(err, "Failed to list pods", "namespace", TargetNamespace)
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
		return
	}

	podMilli, err := e.DataCollector.ListPodMilliCPUByNode(ctx, TargetNamespace, node)
	if err != nil {
		logger.Error(err, "Failed to list pod milliCPU (requests-based)")
		return
	}

	state := PressureState[nodeName]
	if state == nil {
		state = &NodePressureState{
			AboveSec:       0,
			Tiers:          nil,
			CurrentTierIdx: 0,
			CurrentTier:    "",
			PerTier:        map[string]*PerTierState{},
		}
		PressureState[nodeName] = state
	}
	state.Tiers = CollectSortedTiers(nodePods, rtData)
	state.AboveSec = overSec

	ReindexCurrentTier(state)

	// Tier가 없으면 스킵
	if len(state.Tiers) == 0 {
		return
	}

	// Current tier 설정 (여전히 비어있다면 안전 초기화)
	if state.CurrentTier == "" || state.CurrentTierIdx >= len(state.Tiers) {
		state.CurrentTierIdx = 0
		state.CurrentTier = state.Tiers[0]
		if _, ok := state.PerTier[state.CurrentTier]; !ok {
			state.PerTier[state.CurrentTier] = &PerTierState{}
		}
	}

	// 현 Criticality에 대해 조치 가능한 Pod에 대한 처리 진행
	e.ProcessCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
}

// ProcessCurrentTier : 현재 Criticality 티어에 대해 다음의 로직을 수행
//
//  1. 90% 이상 지속 시간이 1초 이상인 경우
//     - 현재 배포된 Pod 중 가장 낮은 Criticality를 가진 Pod의 CPU 요청량에 20% 감소 적용 (Graceful degradation)
//
//  2. 90% 이상 지속 시간이 10초 이상인 경우
//     - 이전 기준에서 Degradation을 적용했던 Pod에 대해 Eviction 처리
func (e *EventHandler) ProcessCurrentTier(ctx context.Context, logger logr.Logger, state *NodePressureState, nodePods []*corev1.Pod, rtData map[string]RealTimeData, podMilli map[string]int64, overSec int, cpuUsage float64) {
	curTier := state.CurrentTier
	pts := state.PerTier[curTier]
	if pts == nil {
		pts = &PerTierState{}
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

		if pts.MissingTicks >= TierMissingTolerance {
			if nextTier, ok := NextHigherTier(curTier, state.Tiers); ok {
				state.CurrentTier = nextTier

				for i, t := range state.Tiers {
					if t == nextTier {
						state.CurrentTierIdx = i
						break
					}
				}
				if _, ok := state.PerTier[nextTier]; !ok {
					state.PerTier[nextTier] = &PerTierState{}
				}
				// 재귀적으로 다음 티어 처리
				e.ProcessCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
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
				if currentMilli > MinMilli {
					degradeRatio := 0.2 // 20% (Graceful degradation 비율)
					if err := e.PodSpecHandler.DegradePodRequests(ctx, top, degradeRatio); err != nil {
						logger.Error(err, "Graceful degradation failed",
							"tier", curTier, "pod", top.Name, "attempt", pts.DegradeCount+1)
					} else {
						pts.DegradeCount++
						pts.LastDegradeTime = overSec
					}
				} else {
					pts.DegradeCount = 999 // 더 이상 degradation 불가
				}
			}
		}
	}

	// 90% 지속 시간이 10초 이상이라면, eviction
	if overSec >= 10 && pts.DegradeCount > 0 && !pts.EvictTried {
		victim := PickHighestCPUFromMilli(targets, podMilli)
		if victim != nil {
			if err := e.EvictPod(ctx, victim); err != nil {
				logger.Error(err, "Eviction failed",
					"tier", curTier, "pod", victim.Name)
				pts.EvictTried = true
			} else {
				pts.EvictTried = true
			}
		}
	}

	// 처리 완료 후 더 이상 actionable한 Pod가 없으면 다음 tier로 에스컬레이션
	if pts.DegradeCount > 0 && pts.EvictTried {
		actionable := HasActionableInTier(nodePods, rtData, curTier)
		if !actionable {
			if nextTier, ok := NextHigherTier(curTier, state.Tiers); ok {
				state.CurrentTier = nextTier

				for i, t := range state.Tiers {
					if t == nextTier {
						state.CurrentTierIdx = i
						break
					}
				}
				if _, ok := state.PerTier[nextTier]; !ok {
					state.PerTier[nextTier] = &PerTierState{}
				}
			}
		}
	}
}

// FindObjectsForNode : 노드의 annotation 변경을 감지하는 이벤트 핸들러 함수
func (e *EventHandler) FindObjectsForNode(ctx context.Context, node client.Object) []reconcile.Request {
	nodeObj := node.(*corev1.Node)

	ann := nodeObj.GetAnnotations()
	if ann == nil {
		return []reconcile.Request{}
	}

	cpuUsageStr := strings.TrimSpace(ann[AnnUsageKey])
	if cpuUsageStr == "" {
		return []reconcile.Request{}
	}
	cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
	if err != nil {
		return []reconcile.Request{}
	}

	// isCpuBusy 상태 확인
	isCpuBusyStr := strings.TrimSpace(ann[AnnCpuBusyKey])
	isCpuBusy := true // 기본값 true
	if isCpuBusyStr != "" {
		if busy, err := strconv.ParseBool(isCpuBusyStr); err == nil {
			isCpuBusy = busy
		}
	}

	// 이미 진행중인 노드라면 스킵
	ProcessingMutex.Lock()
	if ProcessingNodes[nodeObj.Name] {
		ProcessingMutex.Unlock()
		return []reconcile.Request{}
	}
	ProcessingNodes[nodeObj.Name] = true
	ProcessingMutex.Unlock()

	logger := log.Log.WithValues("McKube/rt.NodeEvent", "CPU-Event")
	logger.V(0).Info("CPU annotation change detected",
		"node", nodeObj.Name,
		"cpu(%)", fmt.Sprintf("%.1f", cpuUsage),
		"isCpuBusy", isCpuBusy)

	// CPU pressure 처리를 별도 Go Routine에서 비동기로 진행
	go func() {
		defer func() {
			// 처리 플래그 초기화
			ProcessingMutex.Lock()
			delete(ProcessingNodes, nodeObj.Name)
			ProcessingMutex.Unlock()
		}()

		// 백그라운드 처리를 위한 30초 타임아웃
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// isCpuBusy=true인 경우에만 CPU pressure 처리
		// isCpuBusy=false인 경우의 CPU 복구는 controller에서 직접 처리
		if isCpuBusy {
			e.HandleNodeCPUPressure(bgCtx, nodeObj.Name)
		}
	}()

	return []reconcile.Request{}
}

// EvictPod evicts a pod with proper labeling
func (e *EventHandler) EvictPod(ctx context.Context, pod *corev1.Pod) error {
	logger := log.Log.WithValues("McKube/rt.evictPod", "Eviction")

	// Step 1: Add label "evicted" to the pod before eviction
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels["evicted"] = "true"

	// Update the pod with the new label
	if err := e.Update(ctx, pod); err != nil {
		logger.Error(err, "Failed to add evicted label to pod",
			"pod", pod.Name,
			"namespace", pod.Namespace)
		// Continue with eviction even if labeling fails
	}

	// Step 2: Perform the eviction
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	return e.SubResource("eviction").Create(ctx, pod, ev)
}

// ===================== Helper functions =====================

// CollectSortedTiers collects and sorts criticality tiers from pods
func CollectSortedTiers(pods []*corev1.Pod, rtData map[string]RealTimeData) []string {
	seen := map[string]bool{}
	for _, p := range pods {
		app := p.Labels["sdv.com"]
		if app == "" {
			continue
		}
		if rt, ok := rtData[app]; ok {
			if _, ok2 := CriticalityRank[rt.Criticality]; ok2 {
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
			if CriticalityRank[tiers[j]] < CriticalityRank[tiers[i]] {
				tiers[i], tiers[j] = tiers[j], tiers[i]
			}
		}
	}
	return tiers
}

// ReindexCurrentTier 현재 state.Tiers가 동적으로 바뀐 뒤, CurrentTier/Idx를 재정렬된 목록에 맞춰 재동기화
func ReindexCurrentTier(state *NodePressureState) {
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

// NextHigherTier 현재 티어보다 높은(랭크가 큰) 티어 중 가장 낮은 랭크를 반환
func NextHigherTier(cur string, tiers []string) (string, bool) {
	curRank, ok := CriticalityRank[cur]
	if !ok {
		return "", false
	}
	bestRank := 1 << 30
	best := ""
	for _, t := range tiers {
		if r, ok := CriticalityRank[t]; ok && r > curRank && r < bestRank {
			bestRank = r
			best = t
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}
