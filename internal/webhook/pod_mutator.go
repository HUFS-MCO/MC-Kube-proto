package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	mcoperatorv1 "mc-kube/api/v1"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=fail,sideEffects=NoneOnDryRun,groups="",resources=pods,verbs=create,versions=v1,name=mpod.kb.io,admissionReviewVersions=v1

type PodMutator struct {
	client client.Client
}

func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log.Log.V(1).Info("=== Webhook Handle called ===", "pod.name", req.Name, "pod.namespace", req.Namespace)

	// Check if receiver m is nil
	if m == nil {
		log.Log.Error(fmt.Errorf("PodMutator receiver is nil"), "PodMutator receiver (m) is nil")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("internal error: PodMutator receiver is nil"))
	}
	log.Log.V(1).Info("PodMutator receiver is not nil", "mutator", m)

	// Nil check for client
	if m.client == nil {
		log.Log.Error(fmt.Errorf("client is nil"), "PodMutator client is nil")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("internal error: client is nil"))
	}
	log.Log.V(1).Info("Client is not nil", "client", m.client)

	pod := &corev1.Pod{}

	err := json.Unmarshal(req.Object.Raw, pod)
	if err != nil {
		log.Log.Error(err, "Failed to unmarshal pod")
		return admission.Errored(http.StatusBadRequest, err)
	}

	log.Log.V(1).Info("Pod unmarshaled successfully", "pod.name", pod.Name, "pod.namespace", pod.Namespace)

	// Check if RT annotations are already applied or in progress
	if pod.Annotations != nil {
		if applied, exists := pod.Annotations["mckube.io/rt-applied"]; exists && applied == "true" {
			log.Log.V(1).Info("RT annotations already applied, skipping", "pod", pod.Name)
			return admission.Allowed("RT annotations already applied")
		}
		if pending, exists := pod.Annotations["mckube.io/rt-pending"]; exists && pending == "true" {
			log.Log.V(1).Info("RT configuration already in progress, skipping", "pod", pod.Name)
			return admission.Allowed("RT configuration already in progress")
		}
	}

	// Check if there's a matching McKube resource with RT settings
	rtSettings, err := m.findRTSettingsForPod(ctx, pod)
	if err != nil {
		log.Log.Error(err, "Failed to find RT settings for pod")
		return admission.Allowed("No RT settings found")
	}

	log.Log.V(1).Info("RT settings found", "rtSettings", rtSettings)

	if rtSettings == nil {
		log.Log.V(1).Info("No RT settings configured")
		return admission.Allowed("No RT settings configured")
	}

	// Add annotations to track RT configuration request
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	// Check if RT configuration is already pending or configured
	if pod.Annotations["mckube.io/rt-pending"] == "true" || pod.Annotations["mckube.io/rt-configured"] == "true" {
		log.Log.V(1).Info("RT configuration already in progress or completed", "pod.name", pod.Name)
		return admission.Allowed("RT configuration already handled")
	}

	pod.Annotations["mckube.io/rt-pending"] = "true"
	pod.Annotations["mckube.io/rt-period"] = fmt.Sprintf("%d", rtSettings.Period)
	pod.Annotations["mckube.io/rt-runtime-low"] = fmt.Sprintf("%d", rtSettings.RuntimeLow)
	pod.Annotations["mckube.io/rt-runtime-hi"] = fmt.Sprintf("%d", rtSettings.RuntimeHi)
	pod.Annotations["mckube.io/rt-current"] = "low" // 초기에는 runtime_low 사용
	if rtSettings.Core != nil {
		pod.Annotations["mckube.io/rt-core"] = *rtSettings.Core
	}

	// Schedule RT configuration asynchronously after pod is scheduled
	go m.scheduleRTConfiguration(ctx, pod, rtSettings)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

func (m *PodMutator) findRTSettingsForPod(ctx context.Context, pod *corev1.Pod) (*mcoperatorv1.RTSettings, error) {
	if m.client == nil {
		return nil, fmt.Errorf("client is nil")
	}

	if pod == nil {
		return nil, fmt.Errorf("pod is nil")
	}

	log.Log.V(1).Info("Finding RT settings for pod", "pod.name", pod.Name, "pod.namespace", pod.Namespace)

	mckubeList := &mcoperatorv1.McKubeList{}
	if err := m.client.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		log.Log.Error(err, "Failed to list McKube resources")
		return nil, err
	}

	log.Log.V(1).Info("Found McKube resources", "count", len(mckubeList.Items))

	for i, mckube := range mckubeList.Items {
		log.Log.V(1).Info("Checking McKube resource", "index", i, "name", mckube.Name, "podName", mckube.Spec.PodName, "targetPod", pod.Name)
		if mckube.Spec.PodName == pod.Name && mckube.Spec.RTSettings != nil {
			log.Log.Info("Found matching RT settings", "mckube.name", mckube.Name)
			return mckube.Spec.RTSettings, nil
		}
	}

	log.Log.V(1).Info("No matching RT settings found for pod", "pod.name", pod.Name)
	return nil, nil
}

func (m *PodMutator) scheduleRTConfiguration(ctx context.Context, pod *corev1.Pod, rtSettings *mcoperatorv1.RTSettings) {
	// Check if RT configuration is already applied or in progress
	if pod.Annotations != nil {
		if configured, exists := pod.Annotations["mckube.io/rt-configured"]; exists && configured == "true" {
			log.Log.V(1).Info("RT configuration already completed, skipping", "pod", pod.Name)
			return
		}
	}

	log.Log.V(1).Info("Starting RT configuration scheduling", "pod", pod.Name, "namespace", pod.Namespace)

	// Wait for pod to be scheduled and containers to be created
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	timeout := time.After(5 * time.Minute) // 5 minute timeout

	for {
		select {
		case <-timeout:
			log.Log.Error(fmt.Errorf("timeout waiting for pod to be ready"),
				"Pod RT configuration timeout", "pod", pod.Name, "namespace", pod.Namespace)
			return
		case <-ticker.C:
			// Get latest pod status
			updatedPod := &corev1.Pod{}
			err := m.client.Get(ctx, client.ObjectKey{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}, updatedPod)
			if err != nil {
				// If pod is not found, it was likely deleted - stop trying
				if strings.Contains(err.Error(), "not found") {
					log.Log.V(1).Info("Pod was deleted, stopping RT configuration attempts", "pod", pod.Name)
					return
				}
				log.Log.Error(err, "Failed to get pod status", "pod", pod.Name)
				continue
			}

			// Check if pod has been scheduled and containers are running
			if updatedPod.Spec.NodeName == "" {
				continue // Pod not scheduled yet
			}

			if updatedPod.Status.Phase != corev1.PodRunning {
				continue // Pod not running yet
			}

			// Check if RT settings are already configured
			if updatedPod.Annotations != nil {
				if configured, exists := updatedPod.Annotations["mckube.io/rt-configured"]; exists && configured == "true" {
					log.Log.V(1).Info("RT settings already configured for pod", "pod", updatedPod.Name)
					return // Already configured, no need to retry
				}
			}

			// Apply RT settings via daemon on the node
			if m.applyRTSettingsViaDaemon(ctx, updatedPod, rtSettings) {
				// Update annotation to mark as configured
				m.updatePodRTAnnotation(ctx, updatedPod, "mckube.io/rt-configured", "true")
				m.updatePodRTAnnotation(ctx, updatedPod, "mckube.io/rt-pending", "false")
				return
			}
		}
	}
}

func (m *PodMutator) applyRTSettingsViaDaemon(ctx context.Context, pod *corev1.Pod, rtSettings *mcoperatorv1.RTSettings) bool {
	// Get the node's IP where the pod is running
	node := &corev1.Node{}
	err := m.client.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node)
	if err != nil {
		log.Log.Error(err, "Failed to get node", "node", pod.Spec.NodeName)
		return false
	}

	var nodeIP string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}

	if nodeIP == "" {
		log.Log.Error(fmt.Errorf("no internal IP found"), "Node has no internal IP", "node", pod.Spec.NodeName)
		return false
	}

	// Call Resource-controller DaemonSet on the node
	daemonURL := fmt.Sprintf("http://%s:8080/cgroup", nodeIP)

	// Get container ID from pod status
	if len(pod.Status.ContainerStatuses) == 0 {
		log.Log.Error(fmt.Errorf("no container statuses"), "Pod has no container statuses", "pod", pod.Name)
		return false
	}

	containerID := pod.Status.ContainerStatuses[0].ContainerID
	if containerID == "" {
		log.Log.Error(fmt.Errorf("container ID is empty"), "Container ID not available", "pod", pod.Name)
		return false
	}

	// Extract container ID from the full URI (e.g., "containerd://abc123" -> "abc123")
	if idx := strings.LastIndex(containerID, "://"); idx != -1 {
		containerID = containerID[idx+3:]
	}

	requestBody := map[string]interface{}{
		"container_id": containerID,
		"period":       rtSettings.Period,
		"runtime":      rtSettings.RuntimeLow, // 초기에는 runtime_low 사용
	}

	if rtSettings.Core != nil {
		requestBody["core"] = *rtSettings.Core
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		log.Log.Error(err, "Failed to marshal request body")
		return false
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(daemonURL, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		log.Log.Error(err, "Failed to call resource-controller", "url", daemonURL)
		return false
	}
	defer resp.Body.Close()

	// Read response body for debugging
	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyString := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		log.Log.Error(fmt.Errorf("resource-controller returned non-200 status"),
			"Resource-controller error", "status", resp.StatusCode, "url", daemonURL, 
			"response", bodyString, "request", string(jsonBody))
		return false
	}

	log.Log.Info("RT settings applied successfully",
		"pod", pod.Name, "namespace", pod.Namespace, "node", pod.Spec.NodeName)
	return true
}

func (m *PodMutator) updatePodRTAnnotation(ctx context.Context, pod *corev1.Pod, key, value string) {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[key] = value

	err := m.client.Patch(ctx, pod, patch)
	if err != nil {
		// Rate limit 에러인 경우 경고 로그만 출력
		if strings.Contains(err.Error(), "rate limiter") || strings.Contains(err.Error(), "context canceled") {
			log.Log.V(1).Info("Rate limit hit while updating pod annotation - this is expected under load", "key", key, "value", value)
		} else {
			log.Log.Error(err, "Failed to update pod annotation", "key", key, "value", value)
		}
	}
}

func NewPodMutator(client client.Client) *PodMutator {
	return &PodMutator{
		client: client,
	}
}
