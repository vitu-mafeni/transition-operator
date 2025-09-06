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

// ResourceRef defines a reference to a Kubernetes resource
type ResourceRef struct {
	// APIVersion of the referenced resource
	// +required
	APIVersion string `json:"apiVersion"`

	// Kind of the referenced resource
	// +required
	Kind string `json:"kind"`

	// Namespace of the referenced resource
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name of the referenced resource
	// +required
	Name string `json:"name"`
}

// PodRef defines a reference to a Pod
type PodRef struct {
	// Namespace of the referenced pod
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name of the referenced pod
	// +required
	Name string `json:"name"`
}

// BackupRef defines a reference to a backup
type BackupRef struct {
	// Name of the referenced backup
	// +required
	Name string `json:"name"`
}

// SecretRef defines a reference to a Secret
type SecretRef struct {
	// Name of the referenced secret
	// +required
	Name string `json:"name"`

	// Namespace of the referenced secret
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// Registry defines registry configuration
type Registry struct {
	// URL of the registry
	// +required
	URL string `json:"url"`

	// Repository path in the registry
	// +required
	Repository string `json:"repository"`

	// SecretRef contains credentials for the registry
	// +optional
	SecretRef *SecretRef `json:"secretRef,omitempty"`
}

// Container defines a container configuration for checkpoints
type Container struct {
	// Name of the container
	// +required
	Name string `json:"name"`

	// Image of the container in the registry
	// +required
	Image string `json:"image"`
}

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CheckpointSpec defines the desired state of Checkpoint
type CheckpointSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// foo is an example field of Checkpoint. Edit checkpoint_types.go to remove/update
	// +optional
	// Schedule specifies the backup schedule in cron format
	// +required
	Schedule string `json:"schedule"`

	ClusterRef *ClusterRef `json:"clusterRef,omitempty"`

	// PodRef specifies the pod to checkpoint
	// +required
	PodRef PodRef `json:"podRef"`

	// ResourceRef specifies the workload to migrate
	// +required
	ResourceRef ResourceRef `json:"resourceRef"`

	// Registry specifies the registry configuration for storing checkpoints
	// +required
	Registry Registry `json:"registry"`

	// Containers specifies the container configurations for checkpoints
	// +optional
	Containers []Container `json:"containers,omitempty"`
}
type ClusterRef struct {
	// Name of the referenced cluster
	// +required
	Name string `json:"name"`
}

// CheckpointStatus defines the observed state of Checkpoint.
type CheckpointStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Phase represents the current phase of the checkpoint backup operation
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastCheckpointTime represents the last time a checkpoint was successfully created
	// +optional
	LastCheckpointTime *metav1.Time `json:"lastCheckpointTime,omitempty"`

	// Message provides additional information about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// ObservedGeneration reflects the generation of the most recently observed CheckpointBackup
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Checkpoint resource.
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Checkpoint is the Schema for the checkpoints API
type Checkpoint struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of Checkpoint
	// +required
	Spec CheckpointSpec `json:"spec"`

	// status defines the observed state of Checkpoint
	// +optional
	Status CheckpointStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// CheckpointList contains a list of Checkpoint
type CheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Checkpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Checkpoint{}, &CheckpointList{})
}
