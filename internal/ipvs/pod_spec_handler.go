package ipvs

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// PodSpecHandler handles Pod specification adjustments
type PodSpecHandler struct {
	client.Client
}

// NewPodSpecHandler creates a new PodSpecHandler instance
func NewPodSpecHandler(client client.Client) *PodSpecHandler {
	return &PodSpecHandler{
		Client: client,
	}
}

// Constants for minimum resource values
const MinMilli = int64(10)

// ===================== Pod spec 조정 관련 함수들 =====================

// DegradePodRequests : Pod의 CPU 요청량을 임의의 비율 만큼 감소시킴
func (h *PodSpecHandler) DegradePodRequests(ctx context.Context, pod *corev1.Pod, ratio float64) error {
	logger := log.Log.WithValues("McKube/rt.degradation", "DegradePodRequests")
	logger.V(0).Info("Starting degradation",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"ratio", ratio)
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
		if newMilli < MinMilli {
			newMilli = MinMilli
		}

		logger.V(0).Info("Processing container for degradation",
			"container", c.Name,
			"oldMilli", oldMilli,
			"newMilli", newMilli,
			"ratio", ratio)

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
		logger.V(0).Info("No containers to degrade, skipping")
		return nil
	}

	patchBytes, err := json.Marshal(pr)
	if err != nil {
		logger.Error(err, "Failed to marshal patch JSON")
		return err
	}

	if err := h.SubResource("resize").Patch(ctx, pod, client.RawPatch(types.MergePatchType, patchBytes)); err != nil {
		logger.Error(err, "Resize patch failed")
		return err
	}

	logger.V(0).Info("Degradation completed successfully")
	return nil
}

// PickHighestCPUFromMilli : 가장 높은 CPU 요청량을 가진 Pod 선택 함수
func PickHighestCPUFromMilli(pods []*corev1.Pod, podMilli map[string]int64) *corev1.Pod {
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

// FilterPodsByCriticality : 특정 Criticality 값을 가진 Pod들을 필터링하는 함수
// → 추후 동일 Criticality를 가진 Pod들을 하나의 그룹으로 묶어 processCurrentTier()에서 처리할 때 사용됨
func FilterPodsByCriticality(pods []*corev1.Pod, rtData map[string]RealTimeData, crit string) []*corev1.Pod {
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

// HasActionableInTier : 특정 Criticality 티어에 대해 조치 가능한 Pod가 있는지 확인
func HasActionableInTier(pods []*corev1.Pod, rtData map[string]RealTimeData, tier string) bool {
	targets := FilterPodsByCriticality(pods, rtData, tier)
	if len(targets) == 0 {
		return false
	}
	for _, p := range targets {
		for _, c := range p.Spec.Containers {
			if c.Resources.Requests == nil || c.Resources.Requests.Cpu() == nil {
				continue
			}
			if c.Resources.Requests.Cpu().MilliValue() > MinMilli {
				return true
			}
		}
	}
	return false
}
