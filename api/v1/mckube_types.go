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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// McKubeSpec defines the desired state of McKube
type McKubeSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of McKube. Edit mckube_types.go to remove/update
	Node                  string `json:"node,omitempty"`
	PodName               string `json:"podname,omitempty"`
	PressuredDeadlinesTotal  int    `json:"pressuredDeadlinesTotal,omitempty"`
	PressuredDeadlinesPeriod int    `json:"pressuredDeadlinesPeriod,omitempty"`
}

// McKubeStatus defines the observed state of McKube
type McKubeStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// McKube is the Schema for the mckubes API
type McKube struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   McKubeSpec   `json:"spec,omitempty"`
	Status McKubeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// McKubeList contains a list of McKube
type McKubeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []McKube `json:"items"`
}

func init() {
	SchemeBuilder.Register(&McKube{}, &McKubeList{})
}
