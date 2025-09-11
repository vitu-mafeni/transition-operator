package helpers

import "k8s.io/apimachinery/pkg/runtime/schema"

type ArgoAppSpec struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name       string   `yaml:"name"`
		Namespace  string   `yaml:"namespace"`
		Finalizers []string `yaml:"finalizers"`
	} `yaml:"metadata"`
	Spec struct {
		Project string `yaml:"project"`
		Source  struct {
			RepoURL        string `yaml:"repoURL"`
			TargetRevision string `yaml:"targetRevision"`
			Path           string `yaml:"path"`
			Directory      struct {
				Recurse bool `yaml:"recurse"`
			} `yaml:"directory"`
		} `yaml:"source"`
		Destination struct {
			Server    string `yaml:"server"`
			Namespace string `yaml:"namespace"`
		} `yaml:"destination"`
		SyncPolicy struct {
			Automated struct {
				Prune      bool `yaml:"prune"`
				SelfHeal   bool `yaml:"selfHeal"`
				AllowEmpty bool `yaml:"allowEmpty"`
			} `yaml:"automated"`

			SyncOptions []string `yaml:"syncOptions"`
		} `yaml:"syncPolicy"`
		IgnoreDifferences []IgnoreDifference `yaml:"ignoreDifferences"`
	} `yaml:"spec"`
}

type IgnoreDifference struct {
	Group        string   `yaml:"group"`
	Kind         string   `yaml:"kind"`
	Name         string   `yaml:"name,omitempty"`
	Namespace    string   `yaml:"namespace,omitempty"`
	JSONPointers []string `yaml:"jsonPointers,omitempty"`
}

var argoAppGVR = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Application",
}
