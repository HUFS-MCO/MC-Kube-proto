package ipvspodmanager

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// RealTimeWCET represents real-time WCET data for a specific node
type RealTimeWCET struct {
	Node   string
	RTWcet int
}

// RealTimeData represents complete real-time data for an application
type RealTimeData struct {
	Criticality string
	RTDeadline  int
	RTPeriod    int
	RTWcets     []RealTimeWCET
}

// DataCollector handles data collection operations for IPVS pod management
type DataCollector struct {
	Client        client.Client
	DynamicClient dynamic.Interface
}

// NewDataCollector creates a new DataCollector instance
func NewDataCollector(client client.Client, dynamicClient dynamic.Interface) *DataCollector {
	return &DataCollector{
		Client:        client,
		DynamicClient: dynamicClient,
	}
}

// ===================== 데이터 수집 함수들 (CPU 사용량, 90% 이상 지속 시간, CPU Requests) =====================
// 이하 모든 함수에서 annotation을 조회할 때는 반드시 etcd에 저장된 annotation 이름과 일치해야 함

// ListPodMilliCPUByNode : 노드에 스케줄된 Pod들의 CPU 요청량(milli CPU 단위) 맵 반환
// Ex) podMilli := map[string]int64{"test-pod-a": 600}
func (dc *DataCollector) ListPodMilliCPUByNode(ctx context.Context, namespace string, node *corev1.Node) (map[string]int64, error) {
	podList := &corev1.PodList{}
	if err := dc.Client.List(ctx, podList, client.InNamespace(namespace)); err != nil {
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

// GetRealTimeData : 기존 RT KUBE에서의 CRD 기반 RT 데이터 수집 함수 (Criticality, deadline, period, WCET)
func (dc *DataCollector) GetRealTimeData(ctx context.Context) (map[string]RealTimeData, error) {
	result := make(map[string]RealTimeData)

	items, err := dc.GetResourcesDynamically(ctx, "mcoperator.sdv.com", "v1", "mckuberealtimes", "default")
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

// GetResourcesDynamically : {Group, Version, Resource} 기반 K8s 리소스 조회 함수
func (dc *DataCollector) GetResourcesDynamically(ctx context.Context, group, version, resource, namespace string) ([]unstructured.Unstructured, error) {
	log.Log.V(1).Info("Inside GetResourcesDynamically", "group", group, "version", version, "resource", resource, "namespace", namespace)
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	list, err := dc.DynamicClient.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Log.Error(err, "GetResourcesDynamically: failed to list", "gvr", gvr)
		return nil, err
	}
	return list.Items, nil
}
