package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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

func (v *RTValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}

	err := json.Unmarshal(req.Object.Raw, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if there's a matching McKube resource with RT settings
	rtSettings, _, err := v.findRTSettingsForPod(ctx, pod)
	if err != nil {
		log.Log.Error(err, "Failed to find RT settings for pod")
		return admission.Allowed("No RT settings found, allowing pod creation")
	}

	if rtSettings == nil {
		return admission.Allowed("No RT settings configured, allowing pod creation")
	}

	// Validate RT settings
	if err := v.validateRTSettings(rtSettings); err != nil {
		return admission.Denied(fmt.Sprintf("Invalid RT settings: %v", err))
	}

	return admission.Allowed("RT settings are valid, pod creation allowed")
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

// validateRTSettings validates RT configuration parameters
func (v *RTValidator) validateRTSettings(rtSettings *mcoperatorv1.RTSettings) error {
	// Validate RT period (must be > 0)
	if rtSettings.Period <= 0 {
		return fmt.Errorf("RT period must be positive, got: %d", rtSettings.Period)
	}

	// Validate RT runtime_low (must be > 0 and <= period)
	if rtSettings.RuntimeLow <= 0 {
		return fmt.Errorf("RT runtime_low must be positive, got: %d", rtSettings.RuntimeLow)
	}

	if rtSettings.RuntimeLow > rtSettings.Period {
		return fmt.Errorf("RT runtime_low (%d) cannot be greater than period (%d)", rtSettings.RuntimeLow, rtSettings.Period)
	}

	// Validate RT runtime_hi (must be > 0 and <= period)
	if rtSettings.RuntimeHi <= 0 {
		return fmt.Errorf("RT runtime_hi must be positive, got: %d", rtSettings.RuntimeHi)
	}

	if rtSettings.RuntimeHi > rtSettings.Period {
		return fmt.Errorf("RT runtime_hi (%d) cannot be greater than period (%d)", rtSettings.RuntimeHi, rtSettings.Period)
	}

	// Validate that runtime_low <= runtime_hi
	if rtSettings.RuntimeLow > rtSettings.RuntimeHi {
		return fmt.Errorf("RT runtime_low (%d) cannot be greater than runtime_hi (%d)", rtSettings.RuntimeLow, rtSettings.RuntimeHi)
	}

	// Validate RT runtime_hi ratio (should not exceed 95% of period for safety)
	maxRuntime := int(float64(rtSettings.Period) * 0.95)
	if rtSettings.RuntimeHi > maxRuntime {
		return fmt.Errorf("RT runtime_hi (%d) exceeds 95%% of period (%d), max allowed: %d", rtSettings.RuntimeHi, rtSettings.Period, maxRuntime)
	}

	// Core setting is optional, but if provided, should be valid format
	if rtSettings.Core != nil && *rtSettings.Core != "" {
		// Basic validation for core format (e.g., "0-3", "1,3", "0")
		// This is a simple check, more sophisticated validation could be added
		if len(*rtSettings.Core) == 0 {
			return fmt.Errorf("RT core setting cannot be empty string")
		}
	}

	return nil
}
