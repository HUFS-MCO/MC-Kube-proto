package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	mcoperatorv1 "mc-kube/api/v1"
)

// +kubebuilder:webhook:path=/validate-rt-pod,mutating=false,failurePolicy=fail,sideEffects=NoneOnDryRun,groups="",resources=pods,verbs=create,versions=v1,name=vrtpod.kb.io,admissionReviewVersions=v1

type RTValidator struct {
	Client client.Client
}

type CgroupRequest struct {
	ContainerID string  `json:"container_id"`
	Period      int     `json:"period"`
	Runtime     int     `json:"runtime"`
	Core        *string `json:"core,omitempty"`
}

func (v *RTValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}

	err := json.Unmarshal(req.Object.Raw, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if there's a matching McKube resource with RT settings
	rtSettings, mckube, err := v.findRTSettingsForPod(ctx, pod)
	if err != nil {
		log.Log.Error(err, "Failed to find RT settings for pod")
		return admission.Allowed("No RT settings found")
	}

	if rtSettings == nil {
		return admission.Allowed("No RT settings configured")
	}

	// For validation webhook, we can't modify the pod directly
	// But we can trigger RT configuration asynchronously
	go v.applyRTSettingsAsync(ctx, pod, rtSettings, mckube)

	return admission.Allowed("RT configuration will be applied")
}

func (v *RTValidator) findRTSettingsForPod(ctx context.Context, pod *corev1.Pod) (*mcoperatorv1.RTSettings, *mcoperatorv1.McKube, error) {
	mckubeList := &mcoperatorv1.McKubeList{}
	if err := v.Client.List(ctx, mckubeList, client.InNamespace(pod.Namespace)); err != nil {
		return nil, nil, err
	}

	for _, mckube := range mckubeList.Items {
		if mckube.Spec.PodName == pod.Name && mckube.Spec.RTSettings != nil {
			return mckube.Spec.RTSettings, &mckube, nil
		}
	}

	return nil, nil, nil
}

func (v *RTValidator) applyRTSettingsAsync(ctx context.Context, pod *corev1.Pod, rtSettings *mcoperatorv1.RTSettings, mckube *mcoperatorv1.McKube) {
	logger := log.Log.WithValues("pod", pod.Name, "namespace", pod.Namespace)

	// Wait for pod to be scheduled and containers to be created
	for i := 0; i < 60; i++ { // Wait up to 60 seconds
		currentPod := &corev1.Pod{}
		if err := v.Client.Get(ctx, client.ObjectKeyFromObject(pod), currentPod); err != nil {
			logger.Error(err, "Failed to get pod for RT configuration")
			return
		}

		// Check if pod is scheduled and has container IDs
		if currentPod.Spec.NodeName != "" && len(currentPod.Status.ContainerStatuses) > 0 {
			allHaveIDs := true
			for _, cs := range currentPod.Status.ContainerStatuses {
				if cs.ContainerID == "" {
					allHaveIDs = false
					break
				}
			}

			if allHaveIDs {
				logger.Info("Applying RT settings to pod containers")
				if err := v.applyRTToPod(ctx, currentPod, rtSettings); err != nil {
					logger.Error(err, "Failed to apply RT settings")
				} else {
					logger.Info("Successfully applied RT settings")
					// Update McKube status or add annotation
					v.markRTApplied(ctx, mckube)
				}
				return
			}
		}

		time.Sleep(1 * time.Second)
	}

	logger.Error(fmt.Errorf("timeout waiting for containers"), "Failed to apply RT settings due to timeout")
}

func (v *RTValidator) applyRTToPod(ctx context.Context, pod *corev1.Pod, rtSettings *mcoperatorv1.RTSettings) error {
	nodeIP := pod.Status.HostIP
	if nodeIP == "" {
		return fmt.Errorf("node IP not available for pod %s", pod.Name)
	}

	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.ContainerID == "" {
			continue
		}

		req := CgroupRequest{
			ContainerID: containerStatus.ContainerID,
			Period:      rtSettings.Period,
			Runtime:     rtSettings.Runtime,
			Core:        rtSettings.Core,
		}

		if err := v.sendRTRequest(nodeIP, req); err != nil {
			return fmt.Errorf("failed to apply RT to container %s: %v", containerStatus.ContainerID, err)
		}
	}

	return nil
}

func (v *RTValidator) sendRTRequest(nodeIP string, req CgroupRequest) error {
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

func (v *RTValidator) markRTApplied(ctx context.Context, mckube *mcoperatorv1.McKube) error {
	if mckube.Annotations == nil {
		mckube.Annotations = make(map[string]string)
	}
	mckube.Annotations["mckube.io/rt-applied"] = "true"

	return v.Client.Update(ctx, mckube)
}

