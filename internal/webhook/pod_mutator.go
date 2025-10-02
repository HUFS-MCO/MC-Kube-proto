package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	mcoperatorv1 "mc-kube/api/v1"
)

// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=fail,sideEffects=NoneOnDryRun,groups="",resources=pods,verbs=create,versions=v1,name=mpod.kb.io,admissionReviewVersions=v1

type PodMutator struct {
	client  client.Client
	decoder *admission.Decoder
}

func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	
	err := m.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if there's a matching McKube resource with RT settings
	rtSettings, err := m.findRTSettingsForPod(ctx, pod)
	if err != nil {
		log.Log.Error(err, "Failed to find RT settings for pod")
		return admission.Allowed("No RT settings found")
	}

	if rtSettings == nil {
		return admission.Allowed("No RT settings configured")
	}

	// Add annotations to track RT configuration request
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations["mckube.io/rt-pending"] = "true"
	pod.Annotations["mckube.io/rt-period"] = fmt.Sprintf("%d", rtSettings.Period)
	pod.Annotations["mckube.io/rt-runtime"] = fmt.Sprintf("%d", rtSettings.Runtime)
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
	mckubeList := &mcoperatorv1.McKubeList{}
	if err := m.client.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		return nil, err
	}

	for _, mckube := range mckubeList.Items {
		if mckube.Spec.PodName == pod.Name && mckube.Spec.RTSettings != nil {
			return mckube.Spec.RTSettings, nil
		}
	}

	return nil, nil
}

func (m *PodMutator) scheduleRTConfiguration(ctx context.Context, pod *corev1.Pod, rtSettings *mcoperatorv1.RTSettings) {
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

	// Call rt-daemon on the node
	daemonURL := fmt.Sprintf("http://%s:8080/cgroup", nodeIP)
	
	requestBody := map[string]interface{}{
		"pod_name":   pod.Name,
		"namespace":  pod.Namespace,
		"period":     rtSettings.Period,
		"runtime":    rtSettings.Runtime,
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
		log.Log.Error(err, "Failed to call rt-daemon", "url", daemonURL)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Log.Error(fmt.Errorf("rt-daemon returned non-200 status"), 
			"RT daemon error", "status", resp.StatusCode, "url", daemonURL)
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
		log.Log.Error(err, "Failed to update pod annotation", "key", key, "value", value)
	}
}

func (m *PodMutator) InjectDecoder(d *admission.Decoder) error {
	m.decoder = d
	return nil
}

func NewPodMutator(client client.Client) *PodMutator {
	return &PodMutator{
		client: client,
	}
}