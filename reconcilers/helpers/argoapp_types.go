package helpers

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
		IgnoreDifferences []struct {
			Group string `yaml:"group"`
			Kind  string `yaml:"kind"`
		} `yaml:"ignoreDifferences"`
	} `yaml:"spec"`
}
