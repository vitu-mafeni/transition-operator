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

// NodeHealthSpec defines the desired state of NodeHealth
type NodeHealthSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// foo is an example field of NodeHealth. Edit nodehealth_types.go to remove/update
	// +optional
	NodeName string `json:"nodeName,omitempty"`
}

// NodeHealthStatus defines the observed state of NodeHealth.
type NodeHealthStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the NodeHealth resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	IP        string      `json:"ip,omitempty"`
	OS        string      `json:"os,omitempty"`
	Arch      string      `json:"arch,omitempty"`
	CPUs      int         `json:"cpus,omitempty"`
	CPUUsage  string      `json:"cpuUsage,omitempty"`
	MemTotal  uint64      `json:"memTotal,omitempty"`
	MemUsed   uint64      `json:"memUsed,omitempty"`
	LastSeen  metav1.Time `json:"lastSeen,omitempty"`
	Condition string      `json:"condition,omitempty"`

	// From node annotations
	ClusterName string `json:"cluster_name,omitempty"`
	ClusterNS   string `json:"cluster_namespace,omitempty"`
	Machine     string `json:"machine,omitempty"`
	OwnerKind   string `json:"owner_kind,omitempty"`
	OwnerName   string `json:"owner_name,omitempty"`
	ProvidedIP  string `json:"provided_node_ip,omitempty"`
	CRISocket   string `json:"cri_socket,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NodeHealth is the Schema for the nodehealths API
type NodeHealth struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of NodeHealth
	// +required
	Spec NodeHealthSpec `json:"spec"`

	// status defines the observed state of NodeHealth
	// +optional
	Status NodeHealthStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// NodeHealthList contains a list of NodeHealth
type NodeHealthList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeHealth `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NodeHealth{}, &NodeHealthList{})
}
