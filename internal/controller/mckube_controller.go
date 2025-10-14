package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/dynamic"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcoperatorv1 "mc-kube/api/v1"
)

type McKubeReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	DynamicClient dynamic.Interface
}

type RealTimeWCET struct {
	Node   string
	RTWcet int
}

type RealTimeData struct {
	Criticality string
	RTDeadline  int
	RTPeriod    int
	RTWcets     []RealTimeWCET
}

// CgroupRequest for RT daemon communication
type CgroupRequest struct {
	ContainerID string  `json:"container_id"`
	Period      int     `json:"period"`
	Runtime     int     `json:"runtime"`
	Core        *string `json:"core,omitempty"`
}

// CgroupRequest는 Webhook에서 사용하므로 컨트롤러에서는 제거

// Timers = (노드 이름 : taint 제거까지 남은 틱 수)
var Timers = make(map[string]int)

// Taint monitoring thread에 사용되는 polling rate (초)
const polling_rate = 10

// Criticality 순서: A < B < C
var criticalityRank = map[string]int{
	"A": 0,
	"B": 1,
	"C": 2,
}

const targetNamespace = "default"

// Annotations (CPU util sender의 main.go에 명세된 목적지 annotation과 일치해야 함)
const (
	annUsageKey   = "mckube.sdv.com/cpu-usage"
	annDurKey     = "mckube.sdv.com/cpu-over90-duration-s"
	annCpuBusyKey = "mckube.sdv.com/isCpuBusy"
)

// Taint key (kept for compatibility, not required for eviction fast-path)
const rtPressureTaintKey = "McKubeRTDeadlinePressure"

// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckuberealtimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=update;patch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckuberealtime,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=pods/eviction,verbs=create

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
	LastDegradeTime int // 마지막 Degradation 시도 타임 스탬프
}

// ===================== Overrun 이벤트 로깅 용 데이터 =====================
type OverrunData struct {
	NodeName    string `json:"node_name,omitempty"` // optional
	ContainerID string `json:"container_id"`        // required
	Timestamp   int64  `json:"timestamp,omitempty"` // optional
}

var pressureState = make(map[string]*NodePressureState)

// 중복 처리 방지를 위해 처리 중인 노드 추적
var processingNodes = make(map[string]bool)
var processingMutex sync.RWMutex

const minMilli = int64(10)
const tierMissingTolerance = 2

// ===================== Reconcile =====================

func (r *McKubeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	defer duration(track("Reconcile"))
	logger := log.Log.WithValues("McKube/rt", req.NamespacedName)

	// logger.V() 안에 작성되는 숫자가 낮을수록 높은 우선 순위
	// V : Verbosity level (상세도)
	loggerLowPrio := logger.V(1)
	loggerHighPrio := logger.V(0)
	loggerLowPrio.Info("Mc-Kube/rt Reconcile method started")

	rt := &mcoperatorv1.McKube{}

	loggerLowPrio.Info("Fetching McKube resource")
	if err := r.Get(ctx, req.NamespacedName, rt); err != nil {
		if client.IgnoreNotFound(err) == nil {
			loggerLowPrio.Info("McKube/rt resource not found. Likely deleted.")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get McKube/rt instance")
		return ctrl.Result{}, err
	}
	loggerLowPrio.Info("McKube resource fetched successfully")

	if rt.Spec.PodName == "" {
		loggerHighPrio.Info("McKube resource has empty spec.PodName. Ignoring...")
		return ctrl.Result{}, nil
	}

	if rt.Spec.Node == "" {
		loggerLowPrio.Info("spec.Node is empty. Attempting to find the Pod and update Node.")

		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.PodName}, pod); err != nil {
			if client.IgnoreNotFound(err) == nil {
				loggerLowPrio.Info("Target pod not found. Requeuing...")
				return ctrl.Result{RequeueAfter: time.Second * 5}, nil
			}
			logger.Error(err, "Failed to get target pod")
			return ctrl.Result{}, err
		}

		if pod.Spec.NodeName == "" || (pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending) {
			loggerLowPrio.Info("Target pod not scheduled or not in Running/Pending phase. Requeuing...", "podPhase", pod.Status.Phase)
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil
		}

		rt.Spec.Node = pod.Spec.NodeName
		loggerHighPrio.Info("Updating McKube resource with Node name", "nodeName", rt.Spec.Node)
		if err := r.Update(ctx, rt); err != nil {
			logger.Error(err, "Failed to update McKube resource with node name")
			return ctrl.Result{}, err
		}
		loggerHighPrio.Info("McKube resource updated with Node name. Requeuing to process...")
		return ctrl.Result{RequeueAfter: time.Second * 1}, nil
	}


	loggerLowPrio.Info("Reconcile method finished")
	return ctrl.Result{}, nil
}

// ===================== 데이터 수집 함수들 (CPU 사용량, 90% 이상 지속 시간, CPU Requests) =====================
// 이하 모든 함수에서 annotation을 조회할 때는 반드시 etcd에 저장된 annotation 이름과 일치해야 함

// listPodMilliCPUByNode() : 노드에 스케줄된 Pod들의 CPU 요청량(milli CPU 단위) 맵 반환
// Ex) podMilli := map[string]int64{"test-pod-a": 600}
func (r *McKubeReconciler) listPodMilliCPUByNode(ctx context.Context, namespace string, node *corev1.Node) (map[string]int64, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make(map[string]int64)
	for i := range podList.Items {
		p := &podList.Items[i]
		if p.Spec.NodeName != node.Name {
			continue
		}
		var sumMilli int64
		for _, c := range p.Spec.Containers {
			if c.Resources.Requests == nil || c.Resources.Requests.Cpu() == nil {
				continue
			}
			sumMilli += c.Resources.Requests.Cpu().MilliValue()
		}
		out[p.Name] = sumMilli
	}
	return out, nil
}

// GetRealTimeData() : 기존 RT KUBE에서의 CRD 기반 RT 데이터 수집 함수 (Criticality, deadline, period, WCET)
func (r *McKubeReconciler) GetRealTimeData(ctx context.Context) (map[string]RealTimeData, error) {
	result := make(map[string]RealTimeData)

	items, err := r.GetResourcesDynamically(ctx, "mcoperator.sdv.com", "v1", "mckuberealtimes", "default")
	if err != nil {
		log.Log.Error(err, "Failed to get McKubeRealtime resources")
		return nil, err
	}
	for _, item := range items {
		typedData := RealTimeData{}
		appName, appNameFound, appNameErr := unstructured.NestedString(item.Object, "metadata", "name")
		criticality, criticalityFound, criticalityErr := unstructured.NestedString(item.Object, "spec", "criticality")
		rtDeadline, rtDeadlineFound, rtDeadlineErr := unstructured.NestedInt64(item.Object, "spec", "rtDeadline")
		rtPeriod, rtPeriodFound, rtPeriodErr := unstructured.NestedInt64(item.Object, "spec", "rtPeriod")

		if !appNameFound || appNameErr != nil {
			return nil, appNameErr
		}
		if !criticalityFound || criticalityErr != nil {
			return nil, criticalityErr
		}
		if !rtDeadlineFound || rtDeadlineErr != nil {
			return nil, rtDeadlineErr
		}
		if !rtPeriodFound || rtPeriodErr != nil {
			return nil, rtPeriodErr
		}

		typedData.Criticality = criticality
		typedData.RTDeadline = int(rtDeadline)
		typedData.RTPeriod = int(rtPeriod)

		rtWcets, rtWcetsFound, rtWcetsErr := unstructured.NestedSlice(item.Object, "spec", "rtWcets")
		if !rtWcetsFound || rtWcetsErr != nil {
			return nil, rtWcetsErr
		}
		for _, rtWcet := range rtWcets {
			m, ok := rtWcet.(map[string]interface{})
			if !ok {
				return nil, errors.New("unable to obtain map from rtWcet object")
			}
			var wcetInt int
			switch v := m["rtWcet"].(type) {
			case int64:
				wcetInt = int(v)
			case float64:
				wcetInt = int(v)
			default:
				return nil, errors.New("rtWcet: unsupported number type")
			}
			typedData.RTWcets = append(typedData.RTWcets, RealTimeWCET{
				Node:   fmt.Sprintf("%v", m["node"]),
				RTWcet: wcetInt,
			})
		}
		result[appName] = typedData
	}
	return result, nil
}

// GetResourcesDynamically() : {Group, Version, Resource} 기반 K8s 리소스 조회 함수
func (r *McKubeReconciler) GetResourcesDynamically(ctx context.Context, group, version, resource, namespace string) ([]unstructured.Unstructured, error) {
	log.Log.V(1).Info("Inside GetResourcesDynamically", "group", group, "version", version, "resource", resource, "namespace", namespace)
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	list, err := r.DynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Log.Error(err, "GetResourcesDynamically: failed to list", "gvr", gvr)
		return nil, err
	}
	return list.Items, nil
}

// ===================== Pod spec 조정 관련 함수들 =====================

// degradePodRequests() : Pod의 CPU 요청량을 임의의 비율 만큼 감소시킴
func (r *McKubeReconciler) degradePodRequests(ctx context.Context, pod *corev1.Pod, ratio float64) error {
	type containerPatch struct {
		Name      string `json:"name"`
		Resources struct {
			Requests map[string]string `json:"requests,omitempty"`
			Limits   map[string]string `json:"limits,omitempty"`
		} `json:"resources"`
	}
	type patchSpec struct {
		Containers []containerPatch `json:"containers"`
	}
	type patchRoot struct {
		Spec patchSpec `json:"spec"`
	}

	var pr patchRoot
	for _, c := range pod.Spec.Containers {
		// Only patch containers that have CPU requests
		if c.Resources.Requests == nil {
			continue
		}
		reqCPU := c.Resources.Requests.Cpu()
		if reqCPU == nil || reqCPU.IsZero() {
			continue
		}

		oldMilli := reqCPU.MilliValue()
		newMilli := int64(float64(oldMilli) * (1.0 - ratio))
		if newMilli < minMilli {
			newMilli = minMilli
		}

		entry := containerPatch{Name: c.Name}

		entry.Resources.Requests = map[string]string{
			string(corev1.ResourceCPU): fmt.Sprintf("%dm", newMilli),
		}

		if len(c.Resources.Limits) > 0 {
			entry.Resources.Limits = make(map[string]string)
			for resName, qty := range c.Resources.Limits {
				entry.Resources.Limits[string(resName)] = qty.String()
			}
		}
		pr.Spec.Containers = append(pr.Spec.Containers, entry)
	}

	if len(pr.Spec.Containers) == 0 {
		return nil
	}

	patchBytes, err := json.Marshal(pr)
	if err != nil {
		return err
	}

	if err := r.SubResource("resize").Patch(ctx, pod, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		return err
	}
	return nil
}

// pickHighestCPUFromMilli() : 가장 높은 CPU 요청량을 가진 Pod 선택 함수
func pickHighestCPUFromMilli(pods []*corev1.Pod, podMilli map[string]int64) *corev1.Pod {
	var best *corev1.Pod
	var bestMilli int64 = -1
	for _, p := range pods {
		if m, ok := podMilli[p.Name]; ok {
			if m > bestMilli {
				bestMilli = m
				best = p
			}
		}
	}
	return best
}

// filterPodsByCriticality() : 특정 Criticality 값을 가진 Pod들을 필터링하는 함수
// → 추후 동일 Criticality를 가진 Pod들을 하나의 그룹으로 묶어 processCurrentTier()에서 처리할 때 사용됨
func filterPodsByCriticality(pods []*corev1.Pod, rtData map[string]RealTimeData, crit string) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range pods {
		app := p.Labels["sdv.com"]
		if app == "" {
			continue
		}
		if rt, ok := rtData[app]; ok {
			if rt.Criticality == crit {
				out = append(out, p)
			}
		}
	}
	return out
}

// hasActionableInTier() : 특정 Criticality 티어에 대해 조치 가능한 Pod가 있는지 확인
func hasActionableInTier(pods []*corev1.Pod, rtData map[string]RealTimeData, tier string) bool {
	targets := filterPodsByCriticality(pods, rtData, tier)
	if len(targets) == 0 {
		return false
	}
	for _, p := range targets {
		for _, c := range p.Spec.Containers {
			if c.Resources.Requests == nil || c.Resources.Requests.Cpu() == nil {
				continue
			}
			if c.Resources.Requests.Cpu().MilliValue() > minMilli {
				return true
			}
		}
	}
	return false
}

// ===================== 이벤트 기반 처리 함수들 =====================

// handleNodeCPUPressure() :  노드의 CPU 사용률이 90% 이상일 때, annotation 및 CR 정보를 파싱하여 processCurrentTier()에 전달하는 함수
func (r *McKubeReconciler) handleNodeCPUPressure(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPUPressureHandler", "EventDriven", "node", nodeName)

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
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

	rtData, err := r.GetRealTimeData(ctx)
	if err != nil {
		logger.Error(err, "Failed to get RT data")
		return
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
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

	podMilli, err := r.listPodMilliCPUByNode(ctx, targetNamespace, node)
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
	state.Tiers = collectSortedTiers(nodePods, rtData)
	state.AboveSec = overSec

	reindexCurrentTier(state)

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
	r.processCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
}

// processCurrentTier() : 현재 Criticality 티어에 대해 다음의 로직을 수행
//
//  1. 90% 이상 지속 시간이 1초 이상인 경우
//     - 현재 배포된 Pod 중 가장 낮은 Criticality를 가진 Pod의 CPU 요청량에 20% 감소 적용 (Graceful degradation)
//
//  2. 90% 이상 지속 시간이 10초 이상인 경우
//     - 이전 기준에서 Degradation을 적용했던 Pod에 대해 Eviction 처리
func (r *McKubeReconciler) processCurrentTier(ctx context.Context, logger logr.Logger, state *NodePressureState, nodePods []*corev1.Pod, rtData map[string]RealTimeData, podMilli map[string]int64, overSec int, cpuUsage float64) {
	curTier := state.CurrentTier
	pts := state.PerTier[curTier]
	if pts == nil {
		pts = &perTierState{}
		state.PerTier[curTier] = pts
	}

	targets := filterPodsByCriticality(nodePods, rtData, curTier)
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
			if nextTier, ok := nextHigherTier(curTier, state.Tiers); ok {
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
				r.processCurrentTier(ctx, logger, state, nodePods, rtData, podMilli, overSec, cpuUsage)
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
			top := pickHighestCPUFromMilli(targets, podMilli)
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

					if err := r.degradePodRequests(ctx, top, degradeRatio); err != nil {
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
		victim := pickHighestCPUFromMilli(targets, podMilli)
		if victim != nil {
			logger.V(0).Info("Stage 2: Immediate eviction due to sustained high CPU",
				"tier", curTier, "pod", victim.Name, "over90Sec", overSec)
			if err := r.evictPod(ctx, victim); err != nil {
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
		actionable := hasActionableInTier(nodePods, rtData, curTier)
		if !actionable {
			if nextTier, ok := nextHigherTier(curTier, state.Tiers); ok {
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

// findObjectsForNode() : 노드의 annotation 변경을 감지하는 이벤트 핸들러 함수
func (r *McKubeReconciler) findObjectsForNode(ctx context.Context, node client.Object) []reconcile.Request {
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

	// 이미 진행중인 노드라면 스킵
	processingMutex.Lock()
	if processingNodes[nodeObj.Name] {
		processingMutex.Unlock()
		return []reconcile.Request{}
	}
	processingNodes[nodeObj.Name] = true
	processingMutex.Unlock()

	logger := log.Log.WithValues("McKube/rt.NodeEvent", "CPU-Event")
	logger.V(0).Info("CPU annotation change detected, triggering adaptive control",
		"node", nodeObj.Name,
		"cpu(%)", fmt.Sprintf("%.1f", cpuUsage))

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
		r.handleNodeCPUPressure(bgCtx, nodeObj.Name)
	}()

	return []reconcile.Request{}
}

func collectSortedTiers(pods []*corev1.Pod, rtData map[string]RealTimeData) []string {
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

func (r *McKubeReconciler) evictPod(ctx context.Context, pod *corev1.Pod) error {
	ev := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}
	return r.SubResource("eviction").Create(ctx, pod, ev)
}

// ===================== Tier helpers =====================

// 현재 state.Tiers가 동적으로 바뀐 뒤, CurrentTier/Idx를 재정렬된 목록에 맞춰 재동기화
func reindexCurrentTier(state *NodePressureState) {
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

// 현재 티어보다 높은(랭크가 큰) 티어 중 가장 낮은 랭크를 반환
func nextHigherTier(cur string, tiers []string) (string, bool) {
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

// ===================== Utils / timing =====================

// track() & duration() : 함수 실행 시간 측정용 함수
func track(msg string) (string, time.Time) {
	return msg, time.Now()
}

var max time.Duration = 0
var counter int = 1

func duration(msg string, start time.Time) {
	elapsed := time.Since(start)
	if counter > 1 {
		if elapsed > max {
			max = elapsed
		}
	}
	if counter%50 == 0 {
		log.Log.V(0).Info("Time", msg, elapsed, "Max", max)
		counter = 1
	}
	counter++
}

// ===================== Setup =====================

// StartTaintThread() : 노드에 대한 taint 모니터링 및 해제 스레드 함수
func (r *McKubeReconciler) StartTaintThread() {
	go func() {
		logger := log.Log.WithValues("McKube/rt.TaintMonitoringThread", "Taint")
		logger.V(1).Info("Starting taint monitoring thread")
		for {
			time.Sleep(time.Duration(polling_rate) * time.Second)
			logger.V(1).Info("Taint Thread: Waking up, working...", "len(Timers)", len(Timers))
			for nodeName, timer := range Timers {
				if timer <= 0 {
					node := &corev1.Node{}
					err := r.Get(context.TODO(), types.NamespacedName{Name: nodeName}, node)
					if err != nil {
						if k8serrors.IsNotFound(err) {
							logger.Error(err, "Taint Thread: node not found, ignoring...")
							continue
						}
						logger.Error(err, "Taint Thread: failed to get node instance")
						continue
					}
					for i, taint := range node.Spec.Taints {
						if taint.Key == rtPressureTaintKey {
							node.Spec.Taints[i] = node.Spec.Taints[len(node.Spec.Taints)-1]
							node.Spec.Taints = node.Spec.Taints[:len(node.Spec.Taints)-1]
							log.Log.V(0).Info("Taint Thread: untainting node", "node", nodeName)
							err = r.Update(context.TODO(), node)
							if err != nil {
								logger.Error(err, "Taint Thread: error while un-tainting the node")
							}
							break
						}
					}
					delete(Timers, nodeName)
				} else {
					logger.V(0).Info("Decrementing timer", nodeName, Timers[nodeName])
					Timers[nodeName]--
				}
			}
		}
	}()
}

// SetupWithManager() : Reconciler를 매니저에 등록하는 함수
func (r *McKubeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Pods by their name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, ".metadata.name", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Name}
	}); err != nil {
		return err
	}

	r.StartOverrunListener(8090) // Overrun 수신 포트 선언

	return ctrl.NewControllerManagedBy(mgr).
		For(&mcoperatorv1.McKube{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.findObjectsForPod)),
		).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.findObjectsForNode)),
		).
		Complete(r)
}

// findObjectsForPod() : 파드가 생성된 네임스페이스의 McKube 관련 CR을 찾아 Reconcile 요청을 생성하는 함수
func (r *McKubeReconciler) findObjectsForPod(ctx context.Context, pod client.Object) []reconcile.Request {
	if pod.GetNamespace() != targetNamespace {
		return []reconcile.Request{}
	}

	mckubeList := &mcoperatorv1.McKubeList{}
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.GetNamespace())); err != nil {
		log.Log.Error(err, "Failed to list McKube resources in findObjectsForPod")
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	for _, mckube := range mckubeList.Items {
		if mckube.Spec.PodName == pod.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      mckube.Name,
					Namespace: mckube.Namespace,
				},
			})
			return requests
		}
	}
	return []reconcile.Request{}
}

// ===================== RT 설정 함수들은 Webhook에서 처리 =====================
// 중복 처리 방지를 위해 컨트롤러에서는 RT 설정 관련 함수들을 제거

// ===================== Overrun Listening Thread =====================

// StartOverrunListener() : Overrun 이벤트 수신용 HTTP 서버 시작 함수
func (r *McKubeReconciler) StartOverrunListener(port int) {
	go func() {
		logger := log.Log.WithValues("McKube/rt.OverrunListener", "HTTP")
		logger.V(0).Info("Starting overrun listener", "port", port)

		http.HandleFunc("/overrun", func(w http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodPost {
				logger.V(1).Info("Invalid method for /overrun", "method", req.Method)
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			var data OverrunData
			decoder := json.NewDecoder(req.Body)
			if err := decoder.Decode(&data); err != nil {
				logger.Error(err, "Failed to decode overrun data")
				http.Error(w, "Invalid JSON", http.StatusBadRequest)
				return
			}

			logger.V(0).Info("Received overrun event",
				"node", data.NodeName,
				"containerID", data.ContainerID,
				"timestamp", data.Timestamp)

			r.handleOverrunEvent(data)

			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})

		addr := fmt.Sprintf(":%d", port)
		logger.V(0).Info("Overrun listener ready", "address", addr)

		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.Error(err, "Overrun listener failed to start")
		}
	}()
}

// handleOverrunEvent() : Overrun 이벤트 처리 함수
func (r *McKubeReconciler) handleOverrunEvent(data OverrunData) {
	logger := log.Log.WithValues("McKube/rt.OverrunHandler", "Overrun")
	ctx := context.TODO()

	logger.V(0).Info("=== Overrun Event Detected ===",
		"nodeName", data.NodeName,
		"containerID", data.ContainerID,
		"timestamp", data.Timestamp)

	// 특정 컨테이너 ID를 가진 파드 조회
	pod, err := r.findPodByContainerID(ctx, data.NodeName, data.ContainerID)
	if err != nil {
		logger.Error(err, "Failed to find pod for container",
			"containerID", data.ContainerID,
			"nodeName", data.NodeName)
		return
	}

	if pod == nil {
		logger.V(0).Info("No pod found for container ID",
			"containerID", data.ContainerID,
			"nodeName", data.NodeName)
		return
	}

	// 파드 내 특정 컨테이너 이름 조회
	containerName := ""
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == data.ContainerID {
			containerName = cs.Name
			break
		}
	}

	// 파드 정보 로깅
	logger.V(0).Info("=== Overrun Pod Identified ===",
		"podName", pod.Name,
		"namespace", pod.Namespace,
		"nodeName", pod.Spec.NodeName,
		"containerName", containerName,
		"containerID", data.ContainerID,
		"podPhase", pod.Status.Phase,
		"timestamp", data.Timestamp)

	// Criticality 정보 로깅
	if app, ok := pod.Labels["sdv.com"]; ok {
		rtData, err := r.GetRealTimeData(ctx)
		if err == nil {
			if rt, found := rtData[app]; found {
				logger.V(0).Info("Pod RT Information",
					"podName", pod.Name,
					"criticality", rt.Criticality,
					"rtPeriod", rt.RTPeriod,
					"rtDeadline", rt.RTDeadline)
			}
		}
	}
}

// findPodByContainerID() : 특정 노드에서 컨테이너 ID로 파드를 찾는 함수
func (r *McKubeReconciler) findPodByContainerID(ctx context.Context, nodeName string, containerID string) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	// 컨테이너 ID 정규화
	//   - 컨테이너 ID 양식 예시 : containerd://abc123 or docker://abc123
	normalizedID := containerID
	if idx := strings.Index(containerID, "://"); idx != -1 {
		normalizedID = containerID[idx+3:]
	}

	for i := range podList.Items {
		pod := &podList.Items[i]

		// 노드 이름이 명시된 경우 해당 노드에서만 진행 → 그렇지 않은 경우 전체 노드에서 탐색
		if nodeName != "" && pod.Spec.NodeName != nodeName {
			continue
		}

		// 모든 컨테이너 상태 확인
		for _, cs := range pod.Status.ContainerStatuses {
			// 컨테이너 상태 ID 정규화
			statusID := cs.ContainerID
			if idx := strings.Index(statusID, "://"); idx != -1 {
				statusID = statusID[idx+3:]
			}

			// 원시 컨테이너 ID 혹은 정규화된 ID와의 일치 여부 확인
			if cs.ContainerID == containerID ||
				statusID == normalizedID ||
				strings.Contains(cs.ContainerID, normalizedID) {
				return pod, nil
			}
		}

		// Init 컨테이너 상태 확인
		for _, cs := range pod.Status.InitContainerStatuses {
			statusID := cs.ContainerID
			if idx := strings.Index(statusID, "://"); idx != -1 {
				statusID = statusID[idx+3:]
			}

			if cs.ContainerID == containerID ||
				statusID == normalizedID ||
				strings.Contains(cs.ContainerID, normalizedID) {
				return pod, nil
			}
		}
	}

	return nil, nil
}

// sendRTRequest sends RT configuration request to daemon
func (r *McKubeReconciler) sendRTRequest(nodeIP string, req CgroupRequest) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %v", err)
	}

	url := fmt.Sprintf("http://%s:8080/cgroup", nodeIP)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		return fmt.Errorf("failed to send request to %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("RT daemon request failed with status: %d", resp.StatusCode)
	}

	return nil
}
