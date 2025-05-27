/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	errorsGo "errors"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mcoperatorv1 "mc-kube/api/v1"
	metrics "k8s.io/metrics/pkg/client/clientset/versioned"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// McKubeReconciler reconciles a McKube object
type McKubeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	DynamicClient dynamic.Interface
}

// RealTimeWCET is part of RealTimeData Struct
type RealTimeWCET struct {
	Node   string
	RTWcet int
}

// RealTimeData is a struct to extract data from the RealTime scheduling CRD
type RealTimeData struct {
	Criticality string
	RTDeadline  int
	RTPeriod    int
	RTWcets     []RealTimeWCET
}

// Map that contains as key the name of the node, and as value the time left before removing the taint
// The value is encoded as "value * polling_rate" seconds
var Timers = make(map[string]int)

// The polling rate to remove the taint
const polling_rate = 10

// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckubes/finalizers,verbs=update

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=nodes/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=mcoperator.sdv.com,resources=mckuberealtime,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the McKube object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile

// sendReniceRequest sends an HTTP POST request to the renicer daemon on the target node
func (r *McKubeReconciler) sendReniceRequest(ctx context.Context, pod *corev1.Pod, nodeIP string, niceValue int) error {
	logger := log.Log.WithValues("McKube/rt", pod.Namespace, "pod", pod.Name)
	logger.V(0).Info("Sending renice request", "pod", pod.Name, "nodeIP", nodeIP, "niceValue", niceValue)

	// Find the container ID. Assuming the first container for simplicity.
	if len(pod.Status.ContainerStatuses) == 0 {
		return errorsGo.New("pod has no container statuses")
	}
	containerID := pod.Status.ContainerStatuses[0].ContainerID
	if containerID == "" {
		return errorsGo.New("container ID is empty")
	}

	// Prepare the request payload
	payload := map[string]interface{}{
		"container_id": containerID,
		"nice":         niceValue,
	}
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		logger.Error(err, "Failed to marshal renice request payload")
		return err
	}

	// Construct the URL for the renicer daemon
	url := fmt.Sprintf("http://%s:8080/renice", nodeIP)

	// Create and send the HTTP POST request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		logger.Error(err, "Failed to create HTTP request for renice")
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		logger.Error(err, "Failed to send HTTP request to renicer daemon")
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("renicer daemon returned non-OK status: %s, body: %s", resp.Status, string(bodyBytes))
	}

	logger.V(0).Info("Successfully sent renice request")
	return nil
}

func (r *McKubeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	defer duration(track("Reconcile")) // This call measures the Reconcile run-time
	logger := log.Log.WithValues("McKube/rt", req.NamespacedName)
	loggerLowPrio := logger.V(1)  // Debug level
	loggerHighPrio := logger.V(0) // Info level
	loggerLowPrio.Info("Mc-Kube/rt Reconcile method started")

	rt := &mcoperatorv1.McKube{}

	// Verify if monitoring object still exists
	loggerLowPrio.Info("Fetching McKube resource")
	err := r.Get(ctx, req.NamespacedName, rt)
	if err != nil {
		if errors.IsNotFound(err) {
			loggerLowPrio.Info("McKube/rt resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get McKube/rt instance")
		return ctrl.Result{}, err
	}
	loggerLowPrio.Info("McKube resource fetched successfully")

	// Check if the PodName is specified in the McKube resource
	if rt.Spec.PodName == "" {
		loggerHighPrio.Info("McKube resource has empty PodName. Ignoring...")
		return ctrl.Result{}, nil
	}

	// If node is not specified in the McKube resource, get the corresponding Pod and update the node
	if rt.Spec.Node == "" {
		loggerLowPrio.Info("McKube resource has empty Node field. Attempting to find Pod and update Node.")
		// Get the Pod based on PodName
		podList := &corev1.PodList{}
		opts := []client.ListOption{
			client.InNamespace(rt.Namespace),
			client.MatchingFields{".metadata.name": rt.Spec.PodName},
		}
		err = r.List(ctx, podList, opts...)
		if err != nil {
			logger.Error(err, "Failed to list pods to find target pod")
			return ctrl.Result{}, err
		}

		if len(podList.Items) == 0 {
			loggerLowPrio.Info("Target pod not found. Requeuing...")
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil // Requeue if pod not found yet
		}

		targetPod := &podList.Items[0] // Assuming Pod names are unique within a namespace

		// Check if the pod is scheduled and node information is available
		if targetPod.Spec.NodeName == "" || (targetPod.Status.Phase != corev1.PodRunning && targetPod.Status.Phase != corev1.PodPending) {
			loggerLowPrio.Info("Target pod not yet scheduled or not in Running/Pending phase. Requeuing...", "podPhase", targetPod.Status.Phase)
			return ctrl.Result{RequeueAfter: time.Second * 5}, nil // Requeue if pod not scheduled yet
		}

		// Update the McKube resource with the node name
		rt.Spec.Node = targetPod.Spec.NodeName
		loggerHighPrio.Info("Updating McKube resource with Node name", "nodeName", rt.Spec.Node)
		err = r.Update(ctx, rt)
		if err != nil {
			logger.Error(err, "Failed to update McKube resource with node name")
			return ctrl.Result{}, err
		}

		loggerHighPrio.Info("McKube resource updated with Node name. Requeuing to process...")
		return ctrl.Result{RequeueAfter: time.Second * 1}, nil // Requeue immediately to process with updated node
	}

	// Check if node specified in monitoring object exists - This check is now redundant if rt.Spec.Node is guaranteed to be filled
	// However, keeping it for safety or if there are other ways spec.Node can be set initially
	foundNode := &corev1.Node{}
	loggerLowPrio.Info("Checking if node exists:", "Node", rt.Spec.Node)
	err = r.Get(ctx, types.NamespacedName{Name: rt.Spec.Node}, foundNode)
	if err != nil {
		if errors.IsNotFound(err) {
			loggerLowPrio.Info("Checking if node exists: Node not found. Ignoring..")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get node instance for comparison with RT")
		return ctrl.Result{}, err
	}
	loggerLowPrio.Info("Node exists")

	// Check if pod specified in monitoring object exists
	podList := &corev1.PodList{}
	loggerLowPrio.Info("Listing pods in namespace", "namespace", "default")
	opts := []client.ListOption{
		client.InNamespace("default"),
	}
	err = r.List(ctx, podList, opts...)
	if err != nil {
		if podList.Size() == 0 {
			logger.Error(err, "Checking if pod exists: error listing PodList")
			return ctrl.Result{}, err
		}
		logger.Error(err, "Failed to get PodList instance for comparison with RT")
		return ctrl.Result{}, err
	}
	loggerLowPrio.Info("Pod list obtained", "podCount", len(podList.Items))

	foundPod := -1
	loggerLowPrio.Info("Searching for target pod in list", "targetPodName", rt.Spec.PodName)
	for i, pod := range podList.Items {
		if pod.Name == rt.Spec.PodName {
			foundPod = i
			break
		}
	}

	if foundPod == -1 || podList.Items[foundPod].Name != rt.Spec.PodName {
		loggerLowPrio.Info("Checking if pod exists: Pod not found in list. Ignoring...")
		return ctrl.Result{}, nil
	}
	loggerLowPrio.Info("Target pod found in list")

	// Get the target pod
	targetPod := &podList.Items[foundPod]

	// Get node IP for renicer daemon
	var nodeIP string
	loggerLowPrio.Info("Fetching Node IP")
	for _, addr := range foundNode.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			nodeIP = addr.Address
			break
		}
	}

	// Check if pod is newly created and set initial nice value
	loggerLowPrio.Info("Checking pod phase and labels for renice eligibility")
	if targetPod.Status.Phase == corev1.PodPending || targetPod.Status.Phase == corev1.PodRunning {
		loggerHighPrio.Info("Checking pod for renice", "pod", targetPod.Name, "phase", targetPod.Status.Phase)
		if appName, ok := targetPod.Labels["sdv.com"]; ok && appName != "" {
			loggerHighPrio.Info("Found app label", "app", appName)
			loggerLowPrio.Info("Fetching realtime data")
			realTimeData, err := r.GetRealTimeData(ctx)
			if err != nil {
				logger.Error(err, "Failed to get realtime data")
			} else {
				if rtItem, ok := realTimeData[appName]; ok {
					loggerHighPrio.Info("Found realtime data", "app", appName, "criticality", rtItem.Criticality)
					var desiredNiceValue int
					switch rtItem.Criticality {
					case "A":
						desiredNiceValue = 0
					case "B":
						desiredNiceValue = -10
					case "C":
						desiredNiceValue = -15
					default:
						desiredNiceValue = 0
					}

					loggerLowPrio.Info("Determined desired nice value", "niceValue", desiredNiceValue)

					if nodeIP != "" {
						loggerHighPrio.Info("Sending renice request", "pod", targetPod.Name, "nodeIP", nodeIP, "niceValue", desiredNiceValue)
						if err := r.sendReniceRequest(ctx, targetPod, nodeIP, desiredNiceValue); err != nil {
							logger.Error(err, "Failed to send initial renice request", "pod", targetPod.Name, "nodeIP", nodeIP)
						} else {
							loggerHighPrio.Info("Successfully sent renice request", "pod", targetPod.Name)
						}
					} else {
						logger.Error(nil, "Node IP is empty", "pod", targetPod.Name)
					}
				} else {
					loggerHighPrio.Info("No realtime data found for app", "app", appName)
				}
			}
		} else {
			loggerHighPrio.Info("No app label found on pod", "pod", targetPod.Name)
		}
	} else {
		loggerLowPrio.Info("Pod not in Pending or Running phase, skipping renice check", "pod", targetPod.Name, "phase", targetPod.Status.Phase)
	}

	// The pod and node exist, check if req missedDeadlinesPeriod are higher than VALUE
	loggerLowPrio.Info("Checking pressured deadlines period", "PressuredDeadlinesPeriod", rt.Spec.PressuredDeadlinesPeriod)
	if rt.Spec.PressuredDeadlinesPeriod > 10 {
		loggerLowPrio.Info("Deleting pod: too many pressured RT deadlines", "PressuredDeadlinesPeriod", rt.Spec.PressuredDeadlinesPeriod)

		// Taint the node so that no other pod can be scheduled on it
		loggerLowPrio.Info("Checking node for taint McKubeRTDeadlinePressure")
		taintExists := false
		for _, taint := range foundNode.Spec.Taints {
			if taint.Key == "McKubeRTDeadlinePressure" {
				taintExists = true
				break
			}
		}
		if taintExists {
			loggerLowPrio.Info("Node already tainted with McKubeRTDeadlinePressure:noSchedule, updating timer")
			Timers[foundNode.Name]++
		} else {
			foundNode.Spec.Taints = append(foundNode.Spec.Taints, corev1.Taint{
				Key:    "McKubeRTDeadlinePressure",
				Value:  "True",
				Effect: corev1.TaintEffectNoSchedule,
			})
			loggerLowPrio.Info("Tainting node with McKubeRTDeadlinePressure:noSchedule")
			err = r.Update(ctx, foundNode)
			if err != nil {
				logger.Error(err, "Error while tainting the node")
				return ctrl.Result{}, err
			}
			Timers[foundNode.Name] = 1
		}

		// Delete the victim pod with some policy # selectPodVictimForDeletion(rt, podList)
		// Delete the current pod
		loggerLowPrio.Info("Selecting pod victim for deletion")
		victimPod := r.selectPodVictimForDeletion(rt, podList)
		if victimPod == nil {
			loggerHighPrio.Info("No pod can be evicted")
			return ctrl.Result{}, nil
		}
		loggerHighPrio.Info("Deleting Pod", "Pod", victimPod.Name)
		err = r.Delete(ctx, victimPod)
		if err != nil {
			if errors.IsNotFound(err) {
				loggerHighPrio.Info("Pod not found. Ignoring since pod must be deleted")
				return ctrl.Result{}, nil
			}
			logger.Error(err, "Error while deleting pod", "Pod", victimPod.Name)
			return ctrl.Result{}, err
		}
		loggerLowPrio.Info("Pod deletion initiated", "Pod", victimPod.Name)
	}
	loggerLowPrio.Info("Reconcile method finished")
	return ctrl.Result{}, nil
}

func (r *McKubeReconciler) selectPodVictimForDeletion(rt *mcoperatorv1.McKube, podList *corev1.PodList) *corev1.Pod {
	log.Log.V(1).Info("Inside selectPodVictimForDeletion")
	listMetrics, err := listMetrics()
	if err != nil {
		log.Log.Error(err, "selectPodVictimForDeletion: error retrieving pods metrics")
		return &corev1.Pod{}
	}
	log.Log.V(1).Info("Pods metrics retrieved")
	realTimeData, err := r.GetRealTimeData(context.TODO())
	if err != nil {
		log.Log.Error(err, "selectPodVictimForDeletion: could not obtain RT data")
		return &corev1.Pod{}
	} else {
		log.Log.V(1).Info("RealTime data obtained")
		max_nonRT := *resource.NewQuantity(0, "DecimalSI")
		max_RT := *resource.NewQuantity(0, "DecimalSI")
		res_nonRT := &corev1.Pod{}
		res_RT := &corev1.Pod{}

		log.Log.V(1).Info("Iterating through pod list to find victim")
		for i, pod := range podList.Items {
			var usagePod resource.Quantity
			if metricsItem, ok := listMetrics[pod.Name]; ok {
				usagePod = metricsItem["cpu"]
				log.Log.V(1).Info("Pod metrics found", "pod", pod.Name, "cpu", usagePod.String())
			} else {
				log.Log.V(1).Info("Pod metrics not found", "pod", pod.Name)
				continue
			}

			if rtItem, ok := realTimeData[pod.Labels["sdv.com"]]; ok {
				log.Log.V(1).Info("Pod has RT label", "pod", pod.Name, "criticality", rtItem.Criticality)
				if rtItem.Criticality != "C" {
					if usagePod.AsDec().Cmp(max_RT.AsDec()) > 0 {
						max_RT = usagePod
						res_RT = &podList.Items[i]
						log.Log.V(1).Info("Found potential RT victim", "pod", res_RT.Name, "cpu", max_RT.String())
					}
				}
			} else {
				log.Log.V(1).Info("Pod does not have RT label", "pod", pod.Name)
				if usagePod.AsDec().Cmp(max_nonRT.AsDec()) > 0 {
					max_nonRT = usagePod
					res_nonRT = &podList.Items[i]
					log.Log.V(1).Info("Found potential non-RT victim", "pod", res_nonRT.Name, "cpu", max_nonRT.String())
				}
			}
		}
		log.Log.V(1).Info("Finished iterating through pod list")

		if max_nonRT.AsDec().Cmp(resource.NewQuantity(0, "DecimalSI").AsDec()) > 0 && res_nonRT != nil {
			log.Log.V(1).Info("Returning non-RT victim", "pod", res_nonRT.Name)
			return res_nonRT
		} else if max_RT.AsDec().Cmp(resource.NewQuantity(0, "DecimalSI").AsDec()) > 0 && res_RT != nil {
			log.Log.V(1).Info("Returning RT victim", "pod", res_RT.Name)
			return res_RT
		}
	}
	log.Log.V(1).Info("No victim found, returning nil")
	return nil
}

func listMetrics() (map[string]corev1.ResourceList, error) {
	log.Log.V(1).Info("Inside listMetrics")
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Log.Error(err, "listMetrics: failed to get in-cluster config")
		return nil, err
	}
	log.Log.V(1).Info("In-cluster config obtained")
	mc, err := metrics.NewForConfig(config)
	if err != nil {
		log.Log.Error(err, "listMetrics: failed to create metrics client")
		return nil, err
	}
	log.Log.V(1).Info("Metrics client created")

	podMetricses, err := mc.MetricsV1beta1().PodMetricses(metav1.NamespaceDefault).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Log.Error(err, "listMetrics: failed to list pod metrics")
		return nil, err
	}
	log.Log.V(1).Info("Pod metrics listed", "metricCount", len(podMetricses.Items))

	result := make(map[string]corev1.ResourceList)
	log.Log.V(1).Info("Processing pod metrics")
	for _, pod := range podMetricses.Items {
		for _, container := range pod.Containers {
			result[pod.Name] = container.Usage
			log.Log.V(1).Info("Processed metrics for pod", "pod", pod.Name, "cpu", container.Usage.Cpu().String())
		}
	}
	log.Log.V(1).Info("Finished processing pod metrics")
	return result, nil
}

// Uses the function "GetResourcesDynamically" to obtain the RT objects used for scheduling
// These objects are obtained for the eviction policy
func (r *McKubeReconciler) GetRealTimeData(ctx context.Context) (map[string]RealTimeData, error) {
	resultErr := make(map[string]RealTimeData)
	result := make(map[string]RealTimeData)

	items, err := r.GetResourcesDynamically(ctx, "mcoperator.sdv.com", "v1", "mckuberealtimes", "default")
	if err != nil {
		log.Log.Error(err, "Failed to get McKubeRealtime resources")
		return resultErr, err
	} else {
		// For each unstructured item in the list, we get the fields and compile an ad-hoc data strcture manually
		for _, item := range items {
			typedData := RealTimeData{}
			appName, appNameFound, appNameErr := unstructured.NestedString(item.Object, "metadata", "name")
			criticality, criticalityFound, criticalityErr := unstructured.NestedString(item.Object, "spec", "criticality")
			rtDeadline, rtDeadlineFound, rtDeadlineErr := unstructured.NestedInt64(item.Object, "spec", "rtDeadline")
			rtPeriod, rtPeriodFound, rtPeriodErr := unstructured.NestedInt64(item.Object, "spec", "rtPeriod")

			if criticalityFound && criticalityErr == nil {
				typedData.Criticality = criticality
			} else {
				return resultErr, criticalityErr
			}

			if rtDeadlineFound && rtDeadlineErr == nil {
				typedData.RTDeadline = int(rtDeadline)
			} else {
				return resultErr, rtDeadlineErr
			}

			if rtPeriodFound && rtPeriodErr == nil {
				typedData.RTPeriod = int(rtPeriod)
			} else {
				return resultErr, rtPeriodErr
			}
			// As there may be more than one WCET listed in the object, we have to iterate on a list
			rtWcets, rtWcetsFound, rtWcetsErr := unstructured.NestedSlice(item.Object, "spec", "rtWcets")
			if rtWcetsFound && rtWcetsErr == nil {
				rtWcetsArray := []RealTimeWCET{}
				for _, rtWcet := range rtWcets {
					mapRTWcet, ok := rtWcet.(map[string]interface{})
					if !ok {
						return resultErr, errorsGo.New("unable to obtain map from rtWcet object")
					}
					rtWcetsArray = append(rtWcetsArray, RealTimeWCET{Node: mapRTWcet["node"].(string), RTWcet: int(mapRTWcet["rtWcet"].(int64))})
				}
				typedData.RTWcets = append(typedData.RTWcets, rtWcetsArray...)
			} else {
				return resultErr, rtWcetsErr
			}
			if appNameFound && appNameErr == nil {
				result[appName] = typedData
			} else {
				return resultErr, appNameErr
			}
		}
	}
	return result, nil
}

// This function obtains untyped resources, such as CRDs defined thrugh a yaml
func (r *McKubeReconciler) GetResourcesDynamically(ctx context.Context, group string, version string, resource string, namespace string) ([]unstructured.Unstructured, error) {
	log.Log.V(1).Info("Inside GetResourcesDynamically", "group", group, "version", version, "resource", resource, "namespace", namespace)
	resourceId := schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}
	log.Log.V(1).Info("Fetching dynamic resource list", "resourceId", resourceId)
	list, err := r.DynamicClient.Resource(resourceId).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Log.Error(err, "GetResourcesDynamically: failed to list dynamic resource", "resourceId", resourceId)
		return nil, err
	}
	log.Log.V(1).Info("Dynamic resource list obtained", "itemCount", len(list.Items))
	return list.Items, nil
}

// Timing function to measure performance, starts the timer
func track(msg string) (string, time.Time) {
	return msg, time.Now()
}

var max time.Duration = time.Duration(0) * time.Nanosecond
var counter int = 1

// Timing function to measure performance, calculates the delay since the timer started
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

// This thread uses the variable "Timers" to keep track of the nodes tainted with "RTDeadlinePressure"
// After the "polling_rate", if the timer for the node is zero and the taint is present, the taint is removed
func (r *McKubeReconciler) StartTaintThread() {
	go func() {
		logger := log.Log.WithValues("McKube/rt.TaintMonitoringThread", "Taint")
		logger.V(1).Info("Starting taint monitoring thread")
		for {
			// Sleeps for "polling_rate" seconds
			time.Sleep(time.Duration(polling_rate) * time.Second)
			logger.V(1).Info("Taint Thread: Waking up, working...", "len(Timers)", len(Timers))
			// Checks all timers
			for nodeName, timer := range Timers {
				// For each timer that has expired
				if timer <= 0 {
					node := &corev1.Node{}
					// Obtaines the node for the timer
					// Note: we cannot store the node in the data structure because it may change inside Kubernetes and we need the latest version
					err := r.Get(context.TODO(), types.NamespacedName{Name: nodeName}, node)
					if err != nil {
						if errors.IsNotFound(err) {
							logger.Error(err, "Taint Thread: node not found, ignoring...")
							continue
						}
						logger.Error(err, "Taint Thread: failed to get node instance")
						continue
					}
					// We check all the taints, if "RTDeadlinePressure" is present, we remove it and update the node
					for i, taint := range node.Spec.Taints {
						if taint.Key == "McKubeRTDeadlinePressure" {
							// To remove the taint from the array:
							// assign last element to RTDeadlinePressure position
							node.Spec.Taints[i] = node.Spec.Taints[len(node.Spec.Taints)-1]
							// Update array without last element
							node.Spec.Taints = node.Spec.Taints[:len(node.Spec.Taints)-1]
							log.Log.V(0).Info("Taint Thread: untaining node", "node", nodeName)
							err = r.Update(context.TODO(), node)
							if err != nil {
								logger.Error(err, "Taint Thread: error while un-tainting the node")
							}
							break
						}
					}
					// We remove the entry about the tainted node because we removed the taint
					delete(Timers, nodeName)
				} else {
					// If the timer is not zero, we decrement it
					logger.V(0).Info("Decrementing timer", nodeName, Timers[nodeName])
					Timers[nodeName]--
				}
			}
		}
	}()	
}

// SetupWithManager sets up the controller with the Manager.
func (r *McKubeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index Pods by their name to allow efficient lookup by name in Reconcile
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, ".metadata.name", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{pod.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mcoperatorv1.McKube{}).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(handler.MapFunc(r.findObjectsForPod)),
		).
		Complete(r)
}

// findObjectsForPod finds McKube objects for a given pod
func (r *McKubeReconciler) findObjectsForPod(ctx context.Context, pod client.Object) []reconcile.Request {
	// Pod가 default 네임스페이스인지 확인
	if pod.GetNamespace() != "default" {
		return []reconcile.Request{}
	}

	// Find McKube resources whose Spec.PodName matches the changed pod's name
	mckubeList := &mcoperatorv1.McKubeList{}
	// McKube 리소스는 Pod와 같은 네임스페이스(default)에 있다고 가정하고 해당 네임스페이스에서 조회
	if err := r.List(ctx, mckubeList, client.InNamespace(pod.GetNamespace())); err != nil {
		log.Log.Error(err, "Failed to list McKube resources in findObjectsForPod")
		return []reconcile.Request{}
	}

	var requests []reconcile.Request
	// McKube 리소스 목록을 순회하며 Spec.PodName이 현재 Pod의 이름과 일치하는지 확인
	for _, mckube := range mckubeList.Items {
		if mckube.Spec.PodName == pod.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      mckube.Name,
					Namespace: mckube.Namespace,
				},
			})
			// 하나의 Pod는 하나의 McKube 리소스에만 연결된다고 가정하고 바로 반환
			return requests
		}
	}

	// default 네임스페이스의 Pod이지만, 해당 Pod를 Spec.PodName으로 가지는 McKube 리소스를 찾지 못한 경우
	return []reconcile.Request{}
}