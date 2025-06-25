package helpers

type VeleroRestore struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
		// Finalizers []string `yaml:"finalizers"`
	} `yaml:"metadata"`
	Spec struct {
		BackupName           string            `yaml:"backupName" json:"backupName"`
		ExcludedResources    []string          `yaml:"excludedResources,omitempty" json:"excludedResources,omitempty"`
		Hooks                map[string]string `yaml:"hooks,omitempty" json:"hooks,omitempty"` // Update type if hooks are used
		IncludedNamespaces   []string          `yaml:"includedNamespaces,omitempty" json:"includedNamespaces,omitempty"`
		ItemOperationTimeout string            `yaml:"itemOperationTimeout,omitempty" json:"itemOperationTimeout,omitempty"`
	} `yaml:"spec"`
}

// apiVersion: velero.io/v1
// kind: Restore
// metadata:
//   name: edge-iot-state-restore
//   namespace: velero
// spec:
//   backupName: edge-iot-backup
//   includedNamespaces:
//     - edge-iot
//   includedResources:
//     - persistentvolumeclaims
//     - secrets
//   restorePVs: true
//   preserveNodePorts: true
