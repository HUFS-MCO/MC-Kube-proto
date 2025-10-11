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
	"time"

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

// ===================== Shared types / clients =====================

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

// Timers map: key=node name, value=remaining ticks before removing the taint.
var Timers = make(map[string]int)

// Polling rate (seconds) used by the taint monitoring thread
const polling_rate = 10

// Criticality order: A(lowest) < B < C(highest)
var criticalityRank = map[string]int{
	"A": 0,
	"B": 1,
	"C": 2,
}

const cpuThresholdPercent = 90.0
const controlTickSeconds = 1
const targetNamespace = "default"

// ===== Annotations (must match main.go) =====
const (
	annUsageKey = "mckube.sdv.com/cpu-usage"
	// NOTE: main.go publishes this exact key:
	annDurKey = "mckube.sdv.com/cpu-over90-duration-s"
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

// ===================== Internal state =====================

type NodePressureState struct {
	AboveSec       int
	Tiers          []string
	CurrentTierIdx int
	CurrentTier    string
	PerTier        map[string]*perTierState
}

type perTierState struct {
	ElapsedSec   int
	DegradeTried bool
	EvictTried   bool
	MissingTicks int
}

// ===================== OverrunData for logging overrun events =====================
type OverrunData struct {
	NodeName    string `json:"node_name,omitempty"`    // optional
    ContainerID string `json:"container_id"`           // required
    Timestamp   int64  `json:"timestamp,omitempty"`    // optional
}

var pressureState = make(map[string]*NodePressureState)

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

	// If spec.node is empty, find the Pod and update the node field
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

	// Apply RT settings if specified
	if rt.Spec.RTSettings != nil {
		loggerHighPrio.Info("Checking if RT settings need to be applied", "podName", rt.Spec.PodName)

		// Get the pod first to check its current state
		pod := &corev1.Pod{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.PodName}, pod); err != nil {
			if client.IgnoreNotFound(err) == nil {
				loggerLowPrio.Info("Target pod not found yet. Will apply RT settings when pod is created.")
				return ctrl.Result{RequeueAfter: time.Second * 5}, nil
			}
			logger.Error(err, "Failed to get target pod for RT settings")
			return ctrl.Result{}, err
		}

		// Apply RT settings based on pod phase
		switch pod.Status.Phase {
		case corev1.PodPending:
			// Check if containers are created but not started yet
			if len(pod.Status.ContainerStatuses) > 0 {
				allHaveIDs := true
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.ContainerID == "" {
						allHaveIDs = false
						break
					}
				}
				if allHaveIDs {
					loggerHighPrio.Info("Containers created but not running - applying RT settings", "podName", rt.Spec.PodName)
					if err := r.applyRTSettings(ctx, rt); err != nil {
						logger.Error(err, "Failed to apply RT settings during pending phase")
						return ctrl.Result{RequeueAfter: time.Second * 5}, err
					}
					loggerHighPrio.Info("RT settings applied successfully during pending phase")
				} else {
					loggerLowPrio.Info("Containers not yet created. Requeuing...", "podName", rt.Spec.PodName)
					return ctrl.Result{RequeueAfter: time.Second * 2}, nil
				}
			} else {
				loggerLowPrio.Info("Pod pending but no container statuses yet. Requeuing...", "podName", rt.Spec.PodName)
				return ctrl.Result{RequeueAfter: time.Second * 2}, nil
			}
		case corev1.PodRunning:
			// For running pods, check if RT settings have been applied
			// We could add an annotation to track this
			if pod.Annotations == nil || pod.Annotations["mckube.io/rt-applied"] != "true" {
				loggerHighPrio.Info("Pod running but RT settings not applied yet", "podName", rt.Spec.PodName)
				if err := r.applyRTSettings(ctx, rt); err != nil {
					logger.Error(err, "Failed to apply RT settings to running pod")
					return ctrl.Result{RequeueAfter: time.Second * 10}, err
				}
				loggerHighPrio.Info("RT settings applied to running pod")

				// Mark as applied
				if err := r.markRTSettingsApplied(ctx, pod); err != nil {
					logger.Error(err, "Failed to mark RT settings as applied")
				}
			}
		default:
			loggerLowPrio.Info("Pod in non-applicable phase for RT settings", "phase", pod.Status.Phase)
		}
	}

	loggerLowPrio.Info("Reconcile method finished")
	return ctrl.Result{}, nil
}

// ===================== Data collection (CPU utilization, over90 duration) =====================

func (r *McKubeReconciler) getNodeCPUPercent(ctx context.Context, node *corev1.Node) (float64, error) {
	_ = ctx
	ann := node.GetAnnotations()
	if ann == nil {
		return 0, fmt.Errorf("node %s has no annotations", node.Name)
	}
	v := strings.TrimSpace(ann[annUsageKey])
	if v == "" {
		return 0, fmt.Errorf("node %s missing annotation %q", node.Name, annUsageKey)
	}
	pct, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value on node %s: %v", annUsageKey, node.Name, err)
	}
	return pct, nil
}

func (r *McKubeReconciler) getNodeOver90Seconds(ctx context.Context, node *corev1.Node) (int, error) {
	_ = ctx
	ann := node.GetAnnotations()
	if ann == nil {
		return 0, fmt.Errorf("node %s has no annotations", node.Name)
	}
	v := strings.TrimSpace(ann[annDurKey]) // main.go와 정합!
	if v == "" {
		return 0, nil
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value on node %s: %v", annDurKey, node.Name, err)
	}
	if sec < 0 {
		sec = 0
	}
	return int(sec), nil
}

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

// ===================== Adaptive control loop =====================

var ErrResizeUnsupported = errors.New("in-place resize unsupported or forbidden")

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
		// Reduce requests.cpu
		entry.Resources.Requests = map[string]string{
			string(corev1.ResourceCPU): fmt.Sprintf("%dm", newMilli),
		}
		// Copy over existing limits unchanged to avoid "limits cannot be removed"
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
		if k8serrors.IsForbidden(err) ||
			k8serrors.IsNotFound(err) ||
			strings.Contains(strings.ToLower(err.Error()), "pods/resize") {
			return ErrResizeUnsupported
		}
		return err
	}
	return nil
}

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
	return true
}

func (r *McKubeReconciler) StartAdaptiveControlLoop() {
	go func() {
		logger := log.Log.WithValues("McKube/rt.AdaptiveControlLoop", "CPU>90%")
		logger.V(1).Info("Starting adaptive control loop")

		ticker := time.NewTicker(time.Duration(controlTickSeconds) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx := context.TODO()

			nodeList := &corev1.NodeList{}
			if err := r.List(ctx, nodeList); err != nil {
				logger.Error(err, "Failed to list nodes")
				continue
			}

			rtData, err := r.GetRealTimeData(ctx)
			if err != nil {
				logger.Error(err, "Failed to get RT data")
				continue
			}

			podList := &corev1.PodList{}
			if err := r.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
				logger.Error(err, "Failed to list pods", "namespace", targetNamespace)
				continue
			}

			podsByNode := map[string][]*corev1.Pod{}
			for i := range podList.Items {
				p := &podList.Items[i]
				if p.Spec.NodeName == "" {
					continue
				}
				podsByNode[p.Spec.NodeName] = append(podsByNode[p.Spec.NodeName], p)
			}

			for i := range nodeList.Items {
				node := &nodeList.Items[i]
				nodePods := podsByNode[node.Name]
				if len(nodePods) == 0 {
					continue
				}

				pct, err := r.getNodeCPUPercent(ctx, node)
				if err != nil {
					logger.Error(err, "Failed to get node CPU percent (annotation)", "node", node.Name)
					continue
				}
				overSec, err := r.getNodeOver90Seconds(ctx, node)
				if err != nil {
					logger.V(1).Info("Failed to get over90 duration; treating as 0", "node", node.Name, "err", err.Error())
					overSec = 0
				}

				podMilli, err := r.listPodMilliCPUByNode(ctx, targetNamespace, node)
				if err != nil {
					logger.Error(err, "Failed to list pod milliCPU (requests-based)", "node", node.Name)
					continue
				}

				// Initialize or get existing state for this node
				state := pressureState[node.Name]
				if state == nil {
					state = &NodePressureState{
						AboveSec:       0,
						Tiers:          nil,
						CurrentTierIdx: 0,
						CurrentTier:    "",
						PerTier:        map[string]*perTierState{},
					}
					pressureState[node.Name] = state
				}
				state.Tiers = collectSortedTiers(nodePods, rtData)

				// -------- CPU가 90% 이하로 떨어지면 상태 리셋 --------
				if pct <= cpuThresholdPercent {
					logger.V(0).Info("Node CPU below threshold: resetting pressure state",
						"node", node.Name, "cpu(%)", fmt.Sprintf("%.1f", pct))
					delete(pressureState, node.Name)
					continue
				}

				// -------- CPU > 90% 지속 시 단계별 처리 --------
				if pct > cpuThresholdPercent {
					state.AboveSec = overSec

					// Tier가 없으면 스킵
					if len(state.Tiers) == 0 {
						logger.V(0).Info("No criticality tiers detected on node; skipping",
							"node", node.Name, "cpu(%)", fmt.Sprintf("%.1f", pct), "over90Sec", overSec)
						continue
					}

					// Current tier 설정
					if state.CurrentTier == "" || state.CurrentTierIdx >= len(state.Tiers) {
						state.CurrentTierIdx = 0
						state.CurrentTier = state.Tiers[0]
						if _, ok := state.PerTier[state.CurrentTier]; !ok {
							state.PerTier[state.CurrentTier] = &perTierState{}
						}
						logger.V(0).Info("Pinning current tier to lowest detected tier",
							"node", node.Name, "tier", state.CurrentTier)
					}

					curTier := state.CurrentTier
					pts := state.PerTier[curTier]
					if pts == nil {
						pts = &perTierState{}
						state.PerTier[curTier] = pts
					}

					targets := filterPodsByCriticality(nodePods, rtData, curTier)
					targetsCount := len(targets)

					logger.V(0).Info("Node pressure snapshot",
						"node", node.Name,
						"cpu(%)", fmt.Sprintf("%.1f", pct),
						"over90Sec", overSec,
						"currentTier", curTier,
						"targetsCount", targetsCount,
						"elapsedSec(tier)", pts.ElapsedSec,
						"degradeTried", pts.DegradeTried,
						"evictTried", pts.EvictTried,
					)

					// Targets가 없으면 다음 tier로 에스컬레이션
					if targetsCount == 0 {
						pts.MissingTicks++
						logger.V(0).Info("No targets in current tier; incrementing MissingTicks",
							"node", node.Name, "tier", curTier, "missingTicks", pts.MissingTicks)

						if pts.MissingTicks >= tierMissingTolerance && state.CurrentTierIdx+1 < len(state.Tiers) {
							nextIdx := state.CurrentTierIdx + 1
							nextTier := state.Tiers[nextIdx]
							logger.V(0).Info("Escalating to next criticality tier",
								"node", node.Name, "fromTier", curTier, "toTier", nextTier)
							state.CurrentTierIdx = nextIdx
							state.CurrentTier = nextTier
							if _, ok := state.PerTier[nextTier]; !ok {
								state.PerTier[nextTier] = &perTierState{}
							}
						}
						continue
					}

					pts.MissingTicks = 0
					pts.ElapsedSec += controlTickSeconds

					// -------- Stage 1: 1초 후부터 Stage2 전까지 매 틱 request 패치 (30% 감소) --------
					if pts.ElapsedSec >= 1 && !pts.EvictTried {
						top := pickHighestCPUFromMilli(targets, podMilli)
						if top != nil {
							logger.V(0).Info("Stage 1: Attempting graceful degradation (-30% requests) on heaviest pod",
								"node", node.Name, "tier", curTier, "pod", top.Name)
							if err := r.degradePodRequests(ctx, top, 0.3); err != nil {
								if errors.Is(err, ErrResizeUnsupported) {
									logger.V(0).Info("In-place resize unsupported; stop further degrade attempts",
										"node", node.Name, "tier", curTier, "pod", top.Name)
									pts.DegradeTried = true // mark to stop repeating degrade when resize unsupported
								} else {
									logger.Error(err, "Graceful degradation failed",
										"node", node.Name, "tier", curTier, "pod", top.Name)
									pts.DegradeTried = true // avoid spamming on repeated errors
								}
							} else {
								logger.V(0).Info("Stage 1: Graceful degradation applied",
									"node", node.Name, "tier", curTier, "pod", top.Name)
								// On success, keep DegradeTried=false so we continue degrading next ticks until eviction
							}
						}
					}

					// -------- Stage 2: 10초 후 eviction --------
					if pts.ElapsedSec >= 10 && !pts.EvictTried {
						victim := pickHighestCPUFromMilli(targets, podMilli)
						if victim != nil {
							logger.V(0).Info("Stage 2: Attempting eviction due to sustained high node CPU",
								"node", node.Name, "tier", curTier, "pod", victim.Name)
							if err := r.evictPod(ctx, victim); err != nil {
								logger.Error(err, "Eviction failed",
									"node", node.Name, "tier", curTier, "pod", victim.Name)
							} else {
								logger.V(0).Info("Stage 2: Eviction succeeded",
									"node", node.Name, "tier", curTier, "pod", victim.Name)
							}
							pts.EvictTried = true
						}
					}

					// -------- Stage 3: 20초 후 다음 tier로 에스컬레이션 --------
					if pts.ElapsedSec >= 20 && pts.DegradeTried && pts.EvictTried {
						actionable := hasActionableInTier(nodePods, rtData, curTier)
						logger.V(0).Info("Stage 3: Escalation gate check",
							"node", node.Name,
							"tier", curTier,
							"actionable", actionable,
							"elapsedSec(tier)", pts.ElapsedSec,
						)
						if !actionable && state.CurrentTierIdx+1 < len(state.Tiers) {
							nextIdx := state.CurrentTierIdx + 1
							nextTier := state.Tiers[nextIdx]
							logger.V(0).Info("Stage 3: Escalating to next criticality tier",
								"node", node.Name, "fromTier", curTier, "toTier", nextTier)
							state.CurrentTierIdx = nextIdx
							state.CurrentTier = nextTier
							if _, ok := state.PerTier[nextTier]; !ok {
								state.PerTier[nextTier] = &perTierState{}
							}
						}
					}
				}
			}
		}
	}()
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

// ===================== Utils / timing =====================

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

func (r *McKubeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Pods by their name
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, ".metadata.name", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Name}
	}); err != nil {
		return err
	}

	// Start loops
	r.StartAdaptiveControlLoop()
	r.StartTaintThread() // 남겨둠
	r.StartOverrunListener(8090)  // Overrun 수신 포트 선언

	return ctrl.NewControllerManagedBy(mgr).
		For(&mcoperatorv1.McKube{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.findObjectsForPod)),
		).
		Complete(r)
}

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

// ===================== RT Settings Functions =====================

func (r *McKubeReconciler) applyRTSettings(ctx context.Context, rt *mcoperatorv1.McKube) error {
	logger := log.Log.WithValues("McKube/rt", rt.Name)

	// Get the target pod
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rt.Namespace, Name: rt.Spec.PodName}, pod); err != nil {
		return fmt.Errorf("failed to get target pod %s: %v", rt.Spec.PodName, err)
	}

	// Check if pod has containers created (but may not be running yet)
	if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
		return fmt.Errorf("pod %s is not in a valid state for RT configuration: %s", rt.Spec.PodName, pod.Status.Phase)
	}

	// For pending pods, wait until containers are created
	if pod.Status.Phase == corev1.PodPending {
		// Check if containers are created but not yet running
		if len(pod.Status.ContainerStatuses) == 0 {
			return fmt.Errorf("pod %s containers not yet created", rt.Spec.PodName)
		}

		// Check if any container doesn't have an ID yet
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.ContainerID == "" {
				return fmt.Errorf("pod %s containers not fully created yet", rt.Spec.PodName)
			}
		}
	}

	// Get node IP
	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		return fmt.Errorf("node IP not available for pod %s", rt.Spec.PodName)
	}

	// Apply RT settings to each container
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.ContainerID == "" {
			continue
		}

		req := CgroupRequest{
			ContainerID: containerStatus.ContainerID,
			Period:      rt.Spec.RTSettings.Period,
			Runtime:     rt.Spec.RTSettings.Runtime,
			Core:        rt.Spec.RTSettings.Core,
		}

		if err := r.sendRTRequest(nodeIP, req); err != nil {
			logger.Error(err, "Failed to apply RT settings to container", "containerID", containerStatus.ContainerID)
			return err
		}

		logger.Info("Successfully applied RT settings to container", "containerID", containerStatus.ContainerID)
	}

	return nil
}

func (r *McKubeReconciler) markRTSettingsApplied(ctx context.Context, pod *corev1.Pod) error {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations["mckube.io/rt-applied"] = "true"

	return r.Update(ctx, pod)
}

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

// ===================== Overrun Listening Thread =====================

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

			// overrun 이벤트 처리 로직 호출
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

func (r *McKubeReconciler) handleOverrunEvent(data OverrunData) {
	logger := log.Log.WithValues("McKube/rt.OverrunHandler", "Overrun")
	ctx := context.TODO()
	
	logger.V(0).Info("=== Overrun Event Detected ===",
		"nodeName", data.NodeName,
		"containerID", data.ContainerID,
		"timestamp", data.Timestamp)

	// Find the pod associated with this container ID
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

	// Find the specific container name within the pod
	containerName := ""
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == data.ContainerID {
			containerName = cs.Name
			break
		}
	}

	// Log the pod information
	logger.V(0).Info("=== Overrun Pod Identified ===",
		"podName", pod.Name,
		"namespace", pod.Namespace,
		"nodeName", pod.Spec.NodeName,
		"containerName", containerName,
		"containerID", data.ContainerID,
		"podPhase", pod.Status.Phase,
		"timestamp", data.Timestamp)

	// Get criticality if available
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

// findPodByContainerID searches for a pod with the given container ID on the specified node
func (r *McKubeReconciler) findPodByContainerID(ctx context.Context, nodeName string, containerID string) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	// Normalize container ID (remove runtime prefix if present)
	// Container IDs can be in format: containerd://abc123 or docker://abc123
	normalizedID := containerID
	if idx := strings.Index(containerID, "://"); idx != -1 {
		normalizedID = containerID[idx+3:]
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		
		// If nodeName is provided, filter by node; otherwise search all nodes.
        if nodeName != "" && pod.Spec.NodeName != nodeName {
            continue
        }

		// Check all container statuses
		for _, cs := range pod.Status.ContainerStatuses {
			// Normalize the container status ID as well
			statusID := cs.ContainerID
			if idx := strings.Index(statusID, "://"); idx != -1 {
				statusID = statusID[idx+3:]
			}

			// Match either full or normalized IDs
			if cs.ContainerID == containerID || 
			   statusID == normalizedID || 
			   strings.Contains(cs.ContainerID, normalizedID) {
				return pod, nil
			}
		}

		// Also check init container statuses
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