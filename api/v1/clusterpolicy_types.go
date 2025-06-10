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

const (
	// specific mode means user has to specify which packages to transition.
	SelectSpecific SelectMode = "Specific"
	// All means all packages will be transitioned.
	// This is the default mode.
	SelectAll SelectMode = "All"
)

// PackageType defines the type of package (stateless or stateful)
type PackageType string

const (
	PackageTypeStateless PackageType = "Stateless"
	// PackageTypeStateful means stateful packages will be transitioned.
	PackageTypeStateful PackageType = "Stateful"
)

type PackageTransitionCondition string

const (
	PackageTransitionConditionInProgress PackageTransitionCondition = "InProgress"
	PackageTransitionConditionCompleted  PackageTransitionCondition = "Completed"
	PackageTransitionConditionFailed     PackageTransitionCondition = "Failed"
)

// BackupType defines the type of backup in Velero backups
type BackupType string

const (
	BackupTypeSchedule BackupType = "Schedule"
	// PackageTypeStateful means stateful packages will be transitioned.
	BackupTypeManual BackupType = "Manual"
)

// +enum
type SelectMode string

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ClusterPolicySpec defines the desired state of ClusterPolicy.
type ClusterPolicySpec struct {
	ClusterSelector        ClusterSelector        `json:"clusterSelector"`
	SelectMode             SelectMode             `json:"selectMode"`     // specific, all, or none
	TransitionMode         string                 `json:"transitionMode"` // manual, automatic, or none
	PackageSelectors       []PackageSelector      `json:"packageSelectors,omitempty"`
	PackageRetentionPolicy PackageRetentionPolicy `json:"packageRetentionPolicy,omitempty"`
	TargetClusterPolicy    TargetClusterPolicy    `json:"targetClusterPolicy,omitempty"`
}

// ClusterSelector specifies the source cluster
type ClusterSelector struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	RepoType string `json:"repoType"` // e.g., git, helm
}

// PackageSelector defines individual package selection criteria
type PackageSelector struct {
	Name              string              `json:"name"`
	PackagePath       string              `json:"packagePath"`
	PackageType       PackageType         `json:"packageType"` // e.g., stateful, stateless
	Selected          bool                `json:"selected"`
	BackupInformation []BackupInformation `json:"backupInformation"`
}

type BackupInformation struct {
	Name       string     `json:"name"`
	BackupType BackupType `json:"backupType"`
}

// PackageRetentionPolicy defines rules for source cleanup after transition
type PackageRetentionPolicy struct {
	RetainOnSource        bool `json:"retainOnSource"`
	DeleteAfterTransition bool `json:"deleteAfterTransition"`
}

// TargetClusterPolicy defines preferences and avoid rules for target clusters
type TargetClusterPolicy struct {
	PreferClusters []PreferredCluster `json:"preferClusters,omitempty"`
	AvoidClusters  []PreferredCluster `json:"avoidClusters,omitempty"`
}

type PreferredCluster struct {
	Name string `json:"name"`
	// Repo string `json:"repo"`
	// RepoType is the type of repository (e.g., git, helm)
	RepoType string `json:"repoType"`
	// Weight is used to prioritize clusters, higher values indicate higher preference
	Weight int `json:"weight,omitempty"`
}

type TransitionedPackages struct {
	PackageSelectors           []PackageSelector          `json:"packageSelectors,omitempty"`
	LastTransitionTime         metav1.Time                `json:"lastTransitionTime,omitempty"`
	PackageTransitionCondition PackageTransitionCondition `json:"packageTransitionCondition,omitempty"` // e.g., "InProgress", "Completed", "Failed"
	PackageTransitionMessage   string                     `json:"packageTransitionMessage,omitempty"`   // message describing the transition status
}

type ClusterPolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	TransitionedPackages []TransitionedPackages `json:"transitionedPackages,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ClusterPolicy is the Schema for the clusterpolicies API.
type ClusterPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterPolicySpec   `json:"spec,omitempty"`
	Status ClusterPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterPolicyList contains a list of ClusterPolicy.
type ClusterPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterPolicy{}, &ClusterPolicyList{})
}
