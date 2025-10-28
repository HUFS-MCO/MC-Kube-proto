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

// Track pod runtime state (low or hi)
var podRuntimeState = make(map[string]string) // podName -> "low" or "hi"
var runtimeStateMutex sync.RWMutex

// CgroupRequest는 Webhook에서 사용하므로 컨트롤러에서는 제거

// Timers = (노드 이름 : taint 제거까지 남은 틱 수)
var Timers = make(map[string]int)

// Taint monitoring thread에 사용되는 polling rate (초)
const polling_rate = 10

// Criticality 순서: A < B < C
// Criticality 순서: Low < Middle < High
var criticalityRank = map[string]int{
	"Low":    0,
	"Middle": 1,
	"High":   2,
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

// ===================== CPU Pool 관리를 위한 데이터 구조 =====================
// CPUCoreInfo: 각 CPU 코어의 사용 정보
type CPUCoreInfo struct {
	CoreID      int                // CPU 코어 번호
	UsageMillis int64              // 현재 할당된 CPU 사용량 (밀리코어)
	Pods        map[string]PodInfo // 이 코어에 할당된 Pod 정보 (podName -> PodInfo)
}

// PodInfo: Pod의 할당 정보
type PodInfo struct {
	Name        string
	Namespace   string
	Criticality string // "Low", "Middle", "High"
	CPUMillis   int64  // 할당된 CPU 양 (밀리코어, RT 설정의 runtime/period 기반)
	CoreSet     []int  // 할당된 코어 번호들
}

// CPUPool: 노드별 CPU 코어 풀 관리
type CPUPool struct {
	NodeName string
	Cores    map[int]*CPUCoreInfo // coreID -> CPUCoreInfo
	mu       sync.RWMutex
}

// CPU Pool 저장소 (nodeName -> CPUPool)
var cpuPools = make(map[string]*CPUPool)
var cpuPoolsMutex sync.RWMutex

const coreUtilizationThreshold = 0.9 // 90% 임계값

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

	// Pod가 스케줄링되고 RT 설정이 있으면 선점 체크 및 CPU Pool 업데이트
	if rt.Spec.RTSettings != nil {
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.PodName}, pod); err == nil {
			// Pod가 노드에 할당되었고 Pending 또는 Running 상태면 처리
			if pod.Spec.NodeName != "" && (pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning) {
				
				if rt.Spec.RTSettings.Core != nil {
					targetCores := parseCoreSet(*rt.Spec.RTSettings.Core)
					
					// RT 설정 기반 CPU 사용량 계산
					runtimeStateMutex.RLock()
					currentRuntimeState := podRuntimeState[pod.Name]
					runtimeStateMutex.RUnlock()

					var effectiveRuntime int
					if currentRuntimeState == "hi" {
						effectiveRuntime = rt.Spec.RTSettings.RuntimeHi
					} else {
						effectiveRuntime = rt.Spec.RTSettings.RuntimeLow
					}

					cpuMillis := int64(0)
					if rt.Spec.RTSettings.Period > 0 {
						cpuMillis = int64(float64(effectiveRuntime) / float64(rt.Spec.RTSettings.Period) * 1000.0)
					}
					if cpuMillis == 0 {
						cpuMillis = 100 // 기본값
					}

					criticality := rt.Spec.Criticality

					// 선점 체크 먼저 수행 (CPU Pool에 추가하기 전)
					loggerHighPrio.Info("Checking preemption for pod",
						"pod", pod.Name,
						"cores", targetCores,
						"cpuMillis", cpuMillis,
						"criticality", criticality)

					if err := r.checkAndPreemptForPod(ctx, pod, targetCores, cpuMillis, criticality); err != nil {
						logger.Error(err, "Failed to check preemption for pod", "pod", pod.Name)
					}
				}

				// 선점 체크 후 CPU Pool 업데이트
				if err := r.updateCPUPoolForPod(ctx, pod, rt); err != nil {
					logger.Error(err, "Failed to update CPU pool for pod", "pod", pod.Name)
				}

				// RT 설정을 컨테이너에 적용
				if pod.Status.Phase == corev1.PodRunning {
					if err := r.applyRTSettingsToContainers(ctx, pod, rt); err != nil {
						logger.Error(err, "Failed to apply RT settings to containers", "pod", pod.Name)
					}
				}
			}
		}
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

// handleCPURecovery() : CPU가 정상화되었을 때 (isCpuBusy=false) 모든 RT Pod를 runtime_low로 복귀시키는 함수
func (r *McKubeReconciler) handleCPURecovery(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPURecovery", "Recovery", "node", nodeName)
	logger.V(0).Info("CPU recovered (isCpuBusy=false), reverting pods to runtime_low")

	// 노드의 pressure state 리셋
	if _, exists := pressureState[nodeName]; exists {
		logger.V(0).Info("Resetting node pressure state")
		delete(pressureState, nodeName)
	}

	// 해당 노드의 모든 Pod 조회
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
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
		if err := r.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
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

			if err := r.sendRTRequest(nodeIP, req); err != nil {
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
		if err := r.Status().Update(ctx, targetMcKube); err != nil {
			logger.Error(err, "Failed to update McKube status", "pod", pod.Name)
		}
	}

	logger.V(0).Info("CPU recovery completed", "podsReverted", len(nodePods))
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
			r.handleCPURecovery(bgCtx, nodeObj.Name)
		} else {
			// isCpuBusy=true인 경우: 기존 CPU pressure 처리
			r.handleNodeCPUPressure(bgCtx, nodeObj.Name)
		}
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
	if err := r.Update(ctx, pod); err != nil {
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

	// sdv.com 라벨 체크 - RT 워크로드가 아니면 처리하지 않음
	labels := pod.GetLabels()
	if labels == nil || labels["sdv.com"] == "" {
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

	// McKube CR 조회
	mckubeList := &mcoperatorv1.McKubeList{}
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		logger.Error(err, "Failed to list McKube resources")
		return
	}

	var targetMcKube *mcoperatorv1.McKube
	for i := range mckubeList.Items {
		if mckubeList.Items[i].Spec.PodName == pod.Name {
			targetMcKube = &mckubeList.Items[i]
			break
		}
	}

	if targetMcKube == nil {
		logger.V(0).Info("No McKube CR found for pod", "podName", pod.Name)
		return
	}

	if targetMcKube.Spec.RTSettings == nil {
		logger.V(0).Info("Pod has no RT settings configured", "podName", pod.Name)
		return
	}

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

	// 현재 runtime 상태 확인
	runtimeStateMutex.RLock()
	currentState := podRuntimeState[pod.Name]
	runtimeStateMutex.RUnlock()

	if currentState == "hi" {
		logger.V(0).Info("Pod already using runtime_hi, no action needed",
			"podName", pod.Name)
		return
	}

	// runtime_low에서 runtime_hi로 변경
	logger.V(0).Info("Escalating pod runtime from low to hi due to overrun",
		"podName", pod.Name,
		"runtime_low", targetMcKube.Spec.RTSettings.RuntimeLow,
		"runtime_hi", targetMcKube.Spec.RTSettings.RuntimeHi)

	// 컨테이너에 runtime_hi 적용
	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		logger.Error(fmt.Errorf("node IP not available"), "Failed to get node IP for pod", "podName", pod.Name)
		return
	}

	req := CgroupRequest{
		ContainerID: data.ContainerID,
		Period:      targetMcKube.Spec.RTSettings.Period,
		Runtime:     targetMcKube.Spec.RTSettings.RuntimeHi,
		Core:        targetMcKube.Spec.RTSettings.Core,
	}

	if err := r.sendRTRequest(nodeIP, req); err != nil {
		logger.Error(err, "Failed to apply runtime_hi to container",
			"containerID", data.ContainerID,
			"podName", pod.Name)
		return
	}

	// 상태 업데이트
	runtimeStateMutex.Lock()
	podRuntimeState[pod.Name] = "hi"
	runtimeStateMutex.Unlock()

	// McKube CR 상태 업데이트
	now := metav1.Now()
	targetMcKube.Status.CurrentRuntime = "hi"
	targetMcKube.Status.LastOverrunTime = &now
	if err := r.Status().Update(ctx, targetMcKube); err != nil {
		logger.Error(err, "Failed to update McKube status", "podName", pod.Name)
	}

	logger.V(0).Info("Successfully escalated pod runtime to hi",
		"podName", pod.Name,
		"newRuntime", targetMcKube.Spec.RTSettings.RuntimeHi)
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

// ===================== CPU Pool 관리 함수들 =====================

// getOrCreateCPUPool: 노드에 대한 CPU Pool을 가져오거나 생성
func getOrCreateCPUPool(nodeName string, numCores int) *CPUPool {
	cpuPoolsMutex.Lock()
	defer cpuPoolsMutex.Unlock()

	if pool, exists := cpuPools[nodeName]; exists {
		return pool
	}

	pool := &CPUPool{
		NodeName: nodeName,
		Cores:    make(map[int]*CPUCoreInfo),
	}

	// 초기화: 모든 코어 생성
	for i := 0; i < numCores; i++ {
		pool.Cores[i] = &CPUCoreInfo{
			CoreID:      i,
			UsageMillis: 0,
			Pods:        make(map[string]PodInfo),
		}
	}

	cpuPools[nodeName] = pool
	return pool
}

// getCoreUtilization: 특정 코어의 사용률 계산 (0.0 ~ 1.0)
func (p *CPUPool) getCoreUtilization(coreID int) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()

	core, exists := p.Cores[coreID]
	if !exists {
		return 0.0
	}

	// 1000 millis = 1 core = 100%
	return float64(core.UsageMillis) / 1000.0
}

// addPodToCore: 코어에 Pod 할당 정보 추가
func (p *CPUPool) addPodToCore(coreID int, pod PodInfo) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if core, exists := p.Cores[coreID]; exists {
		core.Pods[pod.Name] = pod
		core.UsageMillis += pod.CPUMillis
	}
}

// removePodFromCore: 코어에서 Pod 할당 정보 제거
func (p *CPUPool) removePodFromCore(coreID int, podName string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if core, exists := p.Cores[coreID]; exists {
		if pod, found := core.Pods[podName]; found {
			core.UsageMillis -= pod.CPUMillis
			delete(core.Pods, podName)
		}
	}
}

// getPodsOnCore: 특정 코어에 할당된 Pod 목록 반환
func (p *CPUPool) getPodsOnCore(coreID int) []PodInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	core, exists := p.Cores[coreID]
	if !exists {
		return nil
	}

	pods := make([]PodInfo, 0, len(core.Pods))
	for _, pod := range core.Pods {
		pods = append(pods, pod)
	}
	return pods
}

// findLeastLoadedCore: 가장 사용률이 낮은 코어 찾기
func (p *CPUPool) findLeastLoadedCore() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	minCore := -1
	minUsage := int64(1<<63 - 1)

	for coreID, core := range p.Cores {
		if core.UsageMillis < minUsage {
			minUsage = core.UsageMillis
			minCore = coreID
		}
	}

	return minCore
}

// ===================== 선점(Preemption) 로직 =====================

// checkAndPreemptForPod: Pod 할당 전 선점 필요 여부 확인 및 실행
// High criticality: Middle/Low를 선점 가능
// Middle criticality: Low를 선점 가능
func (r *McKubeReconciler) checkAndPreemptForPod(ctx context.Context, pod *corev1.Pod, targetCores []int, cpuMillis int64, criticality string) error {
	logger := log.Log.WithValues("McKube/rt.Preemption", "Check")

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod has no assigned node")
	}

	// 노드의 CPU Pool 가져오기
	cpuPoolsMutex.RLock()
	pool, exists := cpuPools[nodeName]
	cpuPoolsMutex.RUnlock()

	if !exists {
		logger.V(1).Info("No CPU pool for node, skipping preemption check", "node", nodeName)
		return nil
	}

	// 각 타겟 코어에 대해 선점 필요 여부 확인
	for _, coreID := range targetCores {
		// 할당 후 예상 사용률 계산
		currentUsage := pool.getCoreUtilization(coreID)
		afterUsage := currentUsage + float64(cpuMillis)/1000.0

		logger.V(0).Info("Core utilization check",
			"core", coreID,
			"currentUsage", fmt.Sprintf("%.2f%%", currentUsage*100),
			"afterUsage", fmt.Sprintf("%.2f%%", afterUsage*100),
			"threshold", fmt.Sprintf("%.2f%%", coreUtilizationThreshold*100))

		// 90% 임계값 초과 시 선점 시도
		if afterUsage > coreUtilizationThreshold {
			logger.V(0).Info("Core utilization will exceed threshold, attempting preemption",
				"core", coreID,
				"pod", pod.Name,
				"criticality", criticality)

			victims := r.findPreemptionVictims(pool, coreID, criticality)
			if len(victims) > 0 {
				logger.V(0).Info("Found preemption victims",
					"core", coreID,
					"victimCount", len(victims))

				for _, victim := range victims {
					if err := r.preemptPod(ctx, victim, pool, coreID); err != nil {
						logger.Error(err, "Failed to preempt victim pod",
							"victim", victim.Name,
							"core", coreID)
					} else {
						logger.V(0).Info("Successfully preempted victim pod",
							"victim", victim.Name,
							"victimCriticality", victim.Criticality,
							"preemptor", pod.Name,
							"preemptorCriticality", criticality,
							"core", coreID)
					}
				}
			} else {
				logger.V(0).Info("No preemptable victims found on core",
					"core", coreID,
					"criticality", criticality)
			}
		}
	}

	return nil
}

// findPreemptionVictims: 선점 가능한 Pod들을 찾음
// High는 Middle, Low를 선점 가능
// Middle은 Low를 선점 가능
func (r *McKubeReconciler) findPreemptionVictims(pool *CPUPool, coreID int, preemptorCriticality string) []PodInfo {
	pods := pool.getPodsOnCore(coreID)
	victims := make([]PodInfo, 0)

	preemptorRank := criticalityRank[preemptorCriticality]

	for _, pod := range pods {
		victimRank := criticalityRank[pod.Criticality]

		// 선점자의 우선순위가 피해자보다 높으면(rank가 크면) 선점 가능
		if preemptorRank > victimRank {
			victims = append(victims, pod)
		}
	}

	return victims
}

// preemptPod: Pod를 선점하여 다른 코어로 이동 또는 evict
func (r *McKubeReconciler) preemptPod(ctx context.Context, victim PodInfo, pool *CPUPool, currentCore int) error {
	logger := log.Log.WithValues("McKube/rt.Preemption", "Evict")

	logger.V(0).Info("Preempting pod from core",
		"pod", victim.Name,
		"namespace", victim.Namespace,
		"criticality", victim.Criticality,
		"core", currentCore)

	// Pod 객체 가져오기
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      victim.Name,
		Namespace: victim.Namespace,
	}, pod); err != nil {
		return fmt.Errorf("failed to get victim pod: %v", err)
	}

	// Step 1: CPU Pool에서 현재 코어에서 제거
	pool.removePodFromCore(currentCore, victim.Name)

	// Step 2: 다른 코어로 이동 시도
	newCore := pool.findLeastLoadedCore()
	newCoreUsage := pool.getCoreUtilization(newCore)

	logger.V(0).Info("Attempting to migrate pod to different core",
		"pod", victim.Name,
		"fromCore", currentCore,
		"toCore", newCore,
		"newCoreUsage", fmt.Sprintf("%.2f%%", newCoreUsage*100))

	// 새 코어에 공간이 있으면 이동
	if newCoreUsage+float64(victim.CPUMillis)/1000.0 <= coreUtilizationThreshold {
		pool.addPodToCore(newCore, victim)

		// Pod의 RT 설정 업데이트 (코어 변경)
		if err := r.updatePodCoreAffinity(ctx, pod, newCore); err != nil {
			logger.Error(err, "Failed to update pod core affinity",
				"pod", victim.Name,
				"newCore", newCore)
			return err
		}

		logger.V(0).Info("Successfully migrated pod to different core",
			"pod", victim.Name,
			"fromCore", currentCore,
			"toCore", newCore)

		return nil
	}

	// Step 3: 이동할 공간이 없으면 evict
	logger.V(0).Info("No available core for migration, evicting pod",
		"pod", victim.Name,
		"criticality", victim.Criticality)

	return r.evictPod(ctx, pod)
}

// updatePodCoreAffinity: Pod의 CPU 코어 어피니티 업데이트
func (r *McKubeReconciler) updatePodCoreAffinity(ctx context.Context, pod *corev1.Pod, newCore int) error {
	logger := log.Log.WithValues("McKube/rt.CoreUpdate", "Affinity")

	// McKube CR 조회
	mckubeList := &mcoperatorv1.McKubeList{}
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		return fmt.Errorf("failed to list McKube resources: %v", err)
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
		return nil
	}

	// Core 설정 업데이트
	newCoreStr := fmt.Sprintf("%d", newCore)
	targetMcKube.Spec.RTSettings.Core = &newCoreStr

	if err := r.Update(ctx, targetMcKube); err != nil {
		return fmt.Errorf("failed to update McKube CR: %v", err)
	}

	logger.V(0).Info("Updated pod core affinity",
		"pod", pod.Name,
		"newCore", newCore)

	// RT 데몬에 새 코어 설정 전송
	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		return fmt.Errorf("node IP not available")
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == "" {
			continue
		}

		req := CgroupRequest{
			ContainerID: cs.ContainerID,
			Period:      targetMcKube.Spec.RTSettings.Period,
			Runtime:     targetMcKube.Spec.RTSettings.RuntimeLow,
			Core:        &newCoreStr,
		}

		if err := r.sendRTRequest(nodeIP, req); err != nil {
			logger.Error(err, "Failed to apply new core to container",
				"containerID", cs.ContainerID,
				"pod", pod.Name)
			continue
		}
	}

	return nil
}

// ===================== 헬퍼 함수들 =====================

// parseCoreSet: 코어 범위 문자열을 파싱하여 코어 번호 배열로 변환
// 예: "2-3" -> [2, 3], "1" -> [1], "0,2,4" -> [0, 2, 4]
func parseCoreSet(coreStr string) []int {
	cores := make([]int, 0)

	// 쉼표로 분리
	parts := strings.Split(coreStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)

		// 범위 체크 (예: "2-3")
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) == 2 {
				start := 0
				end := 0
				fmt.Sscanf(rangeParts[0], "%d", &start)
				fmt.Sscanf(rangeParts[1], "%d", &end)

				for i := start; i <= end; i++ {
					cores = append(cores, i)
				}
			}
		} else {
			// 단일 코어
			coreID := 0
			if _, err := fmt.Sscanf(part, "%d", &coreID); err == nil {
				cores = append(cores, coreID)
			}
		}
	}

	return cores
}

// getPodCPUMillis: Pod의 CPU 요청량을 밀리코어로 반환
func (r *McKubeReconciler) getPodCPUMillis(pod *corev1.Pod) int64 {
	totalMillis := int64(0)

	for _, container := range pod.Spec.Containers {
		if container.Resources.Requests != nil {
			if cpu := container.Resources.Requests.Cpu(); cpu != nil {
				totalMillis += cpu.MilliValue()
			}
		}
	}

	// 최소값 보장
	if totalMillis == 0 {
		totalMillis = 100 // 기본값 100m
	}

	return totalMillis
}

// updateCPUPoolForPod: Pod 정보를 CPU Pool에 업데이트
func (r *McKubeReconciler) updateCPUPoolForPod(ctx context.Context, pod *corev1.Pod, mckube *mcoperatorv1.McKube) error {
	if mckube.Spec.RTSettings == nil || mckube.Spec.RTSettings.Core == nil {
		return nil
	}

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod has no assigned node")
	}

	// CPU Pool 가져오기 또는 생성 (기본 8코어로 가정, 실제로는 노드 정보에서 가져와야 함)
	pool := getOrCreateCPUPool(nodeName, 8)

	// RT 설정을 기반으로 CPU 사용량 계산
	// Runtime과 Period를 사용하여 실제 CPU 사용률 추정
	// CPU 사용량 (millis) = (runtime / period) * 1000
	// 현재 runtime 상태 확인
	runtimeStateMutex.RLock()
	currentRuntimeState := podRuntimeState[pod.Name]
	runtimeStateMutex.RUnlock()

	var effectiveRuntime int
	if currentRuntimeState == "hi" {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeHi
	} else {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeLow
	}

	// CPU 사용량 계산: (runtime_us / period_us) * 1000 millis
	cpuMillis := int64(0)
	if mckube.Spec.RTSettings.Period > 0 {
		cpuMillis = int64(float64(effectiveRuntime) / float64(mckube.Spec.RTSettings.Period) * 1000.0)
	}

	// 최소값 보장
	if cpuMillis == 0 {
		cpuMillis = 100 // 기본값 100m
	}

	// Pod 정보 생성
	podInfo := PodInfo{
		Name:        pod.Name,
		Namespace:   pod.Namespace,
		Criticality: mckube.Spec.Criticality,
		CPUMillis:   cpuMillis,
		CoreSet:     parseCoreSet(*mckube.Spec.RTSettings.Core),
	}

	// 각 코어에 Pod 추가 (이미 있으면 업데이트)
	for _, coreID := range podInfo.CoreSet {
		// 기존에 있으면 제거 후 재추가 (업데이트)
		pool.removePodFromCore(coreID, pod.Name)
		pool.addPodToCore(coreID, podInfo)
	}

	log.Log.V(0).Info("Updated CPU pool for pod",
		"pod", pod.Name,
		"node", nodeName,
		"cores", podInfo.CoreSet,
		"cpuMillis", cpuMillis,
		"runtime", effectiveRuntime,
		"period", mckube.Spec.RTSettings.Period,
		"criticality", podInfo.Criticality)

	return nil
}

// applyRTSettingsToContainers applies RT cgroup settings to all containers in a pod
func (r *McKubeReconciler) applyRTSettingsToContainers(ctx context.Context, pod *corev1.Pod, mckube *mcoperatorv1.McKube) error {
	logger := log.Log.WithValues("McKube/rt.RTSettings", "Apply")

	if mckube.Spec.RTSettings == nil {
		return nil
	}

	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		return fmt.Errorf("node IP not available")
	}

	// 현재 runtime 상태 확인
	runtimeStateMutex.RLock()
	currentRuntimeState := podRuntimeState[pod.Name]
	runtimeStateMutex.RUnlock()

	var effectiveRuntime int
	if currentRuntimeState == "hi" {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeHi
	} else {
		effectiveRuntime = mckube.Spec.RTSettings.RuntimeLow
	}

	// 모든 컨테이너에 RT 설정 적용
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == "" {
			continue
		}

		req := CgroupRequest{
			ContainerID: cs.ContainerID,
			Period:      mckube.Spec.RTSettings.Period,
			Runtime:     effectiveRuntime,
			Core:        mckube.Spec.RTSettings.Core,
		}

		if err := r.sendRTRequest(nodeIP, req); err != nil {
			logger.Error(err, "Failed to apply RT settings to container",
				"containerID", cs.ContainerID,
				"pod", pod.Name,
				"runtime", effectiveRuntime,
				"period", mckube.Spec.RTSettings.Period,
				"core", *mckube.Spec.RTSettings.Core)
			return err
		}

		logger.V(0).Info("Applied RT settings to container",
			"pod", pod.Name,
			"container", cs.Name,
			"runtime", effectiveRuntime,
			"period", mckube.Spec.RTSettings.Period,
			"core", *mckube.Spec.RTSettings.Core)
	}

	return nil
}
