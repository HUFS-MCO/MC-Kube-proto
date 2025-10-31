package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"golang.org/x/sys/unix"

	"k8s.io/client-go/dynamic"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcoperatorv1 "mc-kube/api/v1"
	"mc-kube/internal/ipvs"
)

// Type aliases for ipvs package types
type RealTimeData = ipvs.RealTimeData
type RealTimeWCET = ipvs.RealTimeWCET

type McKubeReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	DynamicClient  dynamic.Interface
	DataCollector  *ipvs.DataCollector
	PodSpecHandler *ipvs.PodSpecHandler
	EventHandler   *ipvs.EventHandler
}

// Track pod runtime state (low or hi)
var podRuntimeState = make(map[string]string) // podName -> "low" or "hi"
var runtimeStateMutex sync.RWMutex

// CgroupRequest for RT daemon communication
type CgroupRequest struct {
	ContainerID string  `json:"container_id"`
	Period      int     `json:"period"`
	Runtime     int     `json:"runtime"`
	Core        *string `json:"core,omitempty"`
}

// RTRequestSender interface for sending RT requests to nodes
type RTRequestSender interface {
	SendRTRequest(nodeIP string, req CgroupRequest) error
}

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
	NodeName    string `json:"node_name,omitempty"`    // optional
	ContainerID string `json:"container_id"`           // required
	Timestamp   uint64 `json:"timestamp,omitempty"`    // eBPF monotonic timestamp (ns)
	RecvTime    int64  `json:"recv_time,omitempty"`    // Controller receive time (monotonic ns)
	Latency     int64  `json:"latency_ns,omitempty"`   // Calculated latency (recv - ts)
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
			// McKube CR이 삭제되었을 때 해당 Pod 상태도 정리
			r.cleanupPodStateByMcKubeName(ctx, req.NamespacedName)
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

				// Pod 처리 시작 시 runtime 상태 초기화 (신규 Pod인 경우)
				r.ensurePodRuntimeStateInitialized(pod.Name)

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

	// 노드 CPU 압박 상황 체크 및 처리
	if rt.Spec.Node != "" {
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: rt.Spec.Node}, node); err == nil {
			ann := node.GetAnnotations()
			if ann != nil {
				// CPU 압박 상황 체크
				if isCpuBusyStr, exists := ann[ipvs.AnnCpuBusyKey]; exists {
					isCpuBusy := strings.TrimSpace(isCpuBusyStr) == "true"

					if isCpuBusy {
						// CPU 압박 상황 처리
						loggerHighPrio.Info("CPU pressure detected, handling with EventHandler", "node", rt.Spec.Node)
						r.EventHandler.HandleNodeCPUPressure(ctx, rt.Spec.Node)
					} else {
						// CPU 복구 상황 처리
						loggerHighPrio.Info("CPU recovered, handling with controller", "node", rt.Spec.Node)
						r.handleCPURecovery(ctx, rt.Spec.Node)
					}
				}
			}
		}
	}

	loggerLowPrio.Info("Reconcile method finished")
	return ctrl.Result{}, nil
}

// 기존 IPVS 관련 로직을 모두 ipvs 패키지로 이원화

// handleCPURecovery() : CPU가 정상화되었을 때 (isCpuBusy=false) 모든 RT Pod를 runtime_low로 복귀시키는 함수
func (r *McKubeReconciler) handleCPURecovery(ctx context.Context, nodeName string) {
	logger := log.Log.WithValues("McKube/rt.CPURecovery", "Recovery", "node", nodeName)
	logger.V(0).Info("CPU recovered (isCpuBusy=false), reverting pods to runtime_low")

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

			if err := r.SendRTRequest(nodeIP, req); err != nil {
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

	// CPU 복구 완료 후 해당 노드의 pressure state 정리
	ipvs.ProcessingMutex.Lock()
	if _, exists := ipvs.PressureState[nodeName]; exists {
		delete(ipvs.PressureState, nodeName)
		logger.V(0).Info("Cleared node pressure state after CPU recovery")
	}
	if _, exists := ipvs.ProcessingNodes[nodeName]; exists {
		delete(ipvs.ProcessingNodes, nodeName)
		logger.V(0).Info("Cleared node processing state after CPU recovery")
	}
	ipvs.ProcessingMutex.Unlock()

	logger.V(0).Info("CPU recovery completed", "podsReverted", len(nodePods))
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
	// DataCollector 초기화
	r.DataCollector = ipvs.NewDataCollector(r.Client, r.DynamicClient)

	// PodSpecHandler 초기화
	r.PodSpecHandler = ipvs.NewPodSpecHandler(r.Client)

	// EventHandler 초기화
	r.EventHandler = ipvs.NewEventHandler(r.Client, r.DataCollector, r.PodSpecHandler)

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
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.EventHandler.FindObjectsForNode)),
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

	// Pod 삭제 시 내부 상태 정리
	if pod.GetDeletionTimestamp() != nil {
		r.cleanupPodState(pod.GetName(), pod.GetNamespace())
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

// cleanupPodState : Pod 삭제 시 모든 내부 상태를 정리하는 함수
func (r *McKubeReconciler) cleanupPodState(podName, namespace string) {
	logger := log.Log.WithValues("McKube/rt.Cleanup", "PodState", "pod", podName)
	logger.V(0).Info("Cleaning up internal state for deleted pod")

	// 1. Controller의 podRuntimeState 정리
	runtimeStateMutex.Lock()
	if _, exists := podRuntimeState[podName]; exists {
		delete(podRuntimeState, podName)
		logger.V(0).Info("Removed pod from runtime state tracking")
	}
	runtimeStateMutex.Unlock()

	// 2. IPVS 패키지의 PodRuntimeState 정리
	ipvs.RuntimeStateMutex.Lock()
	if _, exists := ipvs.PodRuntimeState[podName]; exists {
		delete(ipvs.PodRuntimeState, podName)
		logger.V(0).Info("Removed pod from IPVS runtime state tracking")
	}
	ipvs.RuntimeStateMutex.Unlock()

	// 3. CPU Pool에서 해당 Pod 제거
	cpuPoolsMutex.Lock()
	for nodeName, pool := range cpuPools {
		pool.mu.Lock()
		for coreID, core := range pool.Cores {
			if _, exists := core.Pods[podName]; exists {
				// Pod 정보 찾아서 CPU 사용량 차감
				podInfo := core.Pods[podName]
				core.UsageMillis -= podInfo.CPUMillis
				delete(core.Pods, podName)
				logger.V(0).Info("Removed pod from CPU pool",
					"node", nodeName,
					"core", coreID,
					"releasedMillis", podInfo.CPUMillis)
			}
		}
		pool.mu.Unlock()
	}
	cpuPoolsMutex.Unlock()

	logger.V(0).Info("Pod state cleanup completed")
}

// cleanupPodStateByMcKubeName : McKube CR 이름으로 Pod 상태를 정리하는 함수
func (r *McKubeReconciler) cleanupPodStateByMcKubeName(ctx context.Context, mcKubeName types.NamespacedName) {
	logger := log.Log.WithValues("McKube/rt.Cleanup", "McKubeDeleted", "mckube", mcKubeName.Name)

	// McKube CR이 삭제되었으므로 이름으로부터 podName을 추정하기 어려움
	// 대신 모든 상태를 정리하는 방식보다는, 해당 namespace의 Pod들을 확인
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(mcKubeName.Namespace)); err != nil {
		logger.Error(err, "Failed to list pods for cleanup")
		return
	}

	// 존재하지 않는 Pod들의 상태를 정리
	existingPods := make(map[string]bool)
	for _, pod := range podList.Items {
		if pod.Labels != nil && pod.Labels["sdv.com"] != "" {
			existingPods[pod.Name] = true
		}
	}

	// Controller podRuntimeState 정리
	runtimeStateMutex.Lock()
	for podName := range podRuntimeState {
		if !existingPods[podName] {
			delete(podRuntimeState, podName)
			logger.V(0).Info("Cleaned up runtime state for non-existent pod", "pod", podName)
		}
	}
	runtimeStateMutex.Unlock()

	// IPVS PodRuntimeState 정리
	ipvs.RuntimeStateMutex.Lock()
	for podName := range ipvs.PodRuntimeState {
		if !existingPods[podName] {
			delete(ipvs.PodRuntimeState, podName)
			logger.V(0).Info("Cleaned up IPVS runtime state for non-existent pod", "pod", podName)
		}
	}
	ipvs.RuntimeStateMutex.Unlock()

	// CPU Pool 정리
	cpuPoolsMutex.Lock()
	for nodeName, pool := range cpuPools {
		pool.mu.Lock()
		for coreID, core := range pool.Cores {
			for podName, podInfo := range core.Pods {
				if !existingPods[podName] {
					core.UsageMillis -= podInfo.CPUMillis
					delete(core.Pods, podName)
					logger.V(0).Info("Cleaned up CPU pool for non-existent pod",
						"pod", podName,
						"node", nodeName,
						"core", coreID)
				}
			}
		}
		pool.mu.Unlock()
	}
	cpuPoolsMutex.Unlock()

	logger.V(0).Info("McKube deletion cleanup completed")
}

// ensurePodRuntimeStateInitialized : Pod의 runtime 상태가 초기화되어 있는지 확인하고 초기화
func (r *McKubeReconciler) ensurePodRuntimeStateInitialized(podName string) {
	runtimeStateMutex.Lock()
	if _, exists := podRuntimeState[podName]; !exists {
		podRuntimeState[podName] = "low" // 기본값으로 low 설정
		log.Log.V(0).Info("Initialized pod runtime state to low", "pod", podName)
	}
	runtimeStateMutex.Unlock()

	ipvs.RuntimeStateMutex.Lock()
	if _, exists := ipvs.PodRuntimeState[podName]; !exists {
		ipvs.PodRuntimeState[podName] = "low" // 기본값으로 low 설정
		log.Log.V(0).Info("Initialized IPVS pod runtime state to low", "pod", podName)
	}
	ipvs.RuntimeStateMutex.Unlock()
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
			// Monotonic timestamp 즉시 기록 (수신 시점 - 가장 먼저!)
			var ts unix.Timespec
			if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
				logger.Error(err, "Failed to get monotonic time")
			}
			recvTimeNs := ts.Sec*1e9 + ts.Nsec

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

			// 수신 시간 기록
			data.RecvTime = recvTimeNs

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

	// Latency 계산 및 측정 로깅 (매 오버런 이벤트마다 기록)
	if data.Timestamp > 0 && data.RecvTime > 0 {
		data.Latency = data.RecvTime - int64(data.Timestamp)
	}
	
	logger.V(99).Info("LATENCY_MEASUREMENT",
		"sendTimeNs", data.Timestamp,
		"recvTimeNs", data.RecvTime,
		"latencyNs", data.Latency)
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

// SendRTRequest sends RT configuration request to daemon (implements RTRequestSender interface)
func (r *McKubeReconciler) SendRTRequest(nodeIP string, req CgroupRequest) error {
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

	return r.EventHandler.EvictPod(ctx, pod)
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

		if err := r.SendRTRequest(nodeIP, req); err != nil {
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

		if err := r.SendRTRequest(nodeIP, req); err != nil {
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
