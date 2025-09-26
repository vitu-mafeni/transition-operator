package helpers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gitea "code.gitea.io/sdk/gitea"
	argov1alpha1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ArgoAppSkipResourcesIgnoreDifferences struct {
	Group        string   `json:"group,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Name         string   `json:"name,omitempty"`
	JsonPointers []string `json:"jsonPointers,omitempty"`
}

func LogRepositories(log logr.Logger, repos []*gitea.Repository) {
	for _, repo := range repos {
		log.Info("Found repository", "name", repo.Name, "full_name", repo.FullName, "url", repo.CloneURL)
	}
}

func DetermineTargetRepo(policy *transitionv1.ClusterPolicy, log logr.Logger) (string, string, bool) {
	highestWeight := -1
	var repo string
	var targetClusterName string

	for _, target := range policy.Spec.TargetClusterPolicy.PreferClusters {
		if target.RepoType != "git" {
			log.Info("Skipping non-git repository", "cluster", target.Name)
			continue
		}
		if target.Weight > highestWeight {
			highestWeight = target.Weight
			repo = target.Name + "-dr"
			targetClusterName = target.Name
		}
	}

	return repo, targetClusterName, repo != ""
}

func CreateAndPushArgoApp(
	ctx context.Context,
	client *gitea.Client,
	username, repoName, folder string,
	clusterPolicy *transitionv1.ClusterPolicy,
	transitionPackage transitionv1.PackageSelector,
	ignoreDifferences []ArgoAppSkipResourcesIgnoreDifferences,
	log logr.Logger,
) (string, error) {
	app := ArgoAppSpec{
		APIVersion: "argoproj.io/v1alpha1",
		Kind:       "Application",
	}

	app.Metadata.Name = fmt.Sprintf("argocd-%s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)
	app.Metadata.Namespace = "argocd"
	app.Metadata.Finalizers = []string{"resources-finalizer.argocd.argoproj.io"}

	app.Spec.Project = "default"
	app.Spec.Source.RepoURL = clusterPolicy.Spec.ClusterSelector.Repo
	app.Spec.Source.TargetRevision = "HEAD"
	app.Spec.Source.Path = transitionPackage.PackagePath
	app.Spec.Source.Directory.Recurse = true

	app.Spec.Destination.Server = "https://kubernetes.default.svc"
	app.Spec.Destination.Namespace = "default"

	// app.Spec.SyncPolicy.Automated.Prune = true
	app.Spec.SyncPolicy.Automated.SelfHeal = true
	app.Spec.SyncPolicy.Automated.AllowEmpty = true
	app.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=true"}

	// Correct placement of IgnoreDifferences
	app.Spec.IgnoreDifferences = []IgnoreDifference{
		{Group: "fn.kpt.dev", Kind: "ApplyReplacements"},
		{Group: "fn.kpt.dev", Kind: "StarlarkRun"},
		{Group: "infra.nephio.org", Kind: "WorkloadCluster"}, // <-- Add this
	}

	//add more ignore differences if provided
	// Add more ignore differences if provided
	if len(ignoreDifferences) > 0 {
		for _, diff := range ignoreDifferences {
			app.Spec.IgnoreDifferences = append(app.Spec.IgnoreDifferences, IgnoreDifference{
				Group:        diff.Group,
				Kind:         diff.Kind,
				Name:         diff.Name,
				JSONPointers: diff.JsonPointers,
			})
		}
	}
	yamlData, err := yaml.Marshal(app)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Argo application YAML: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s/argo-app-%s.yaml", folder, timestamp)
	message := fmt.Sprintf("ArgoCD app: %s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)

	encodedContent := base64.StdEncoding.EncodeToString(yamlData)

	fileOpts := gitea.CreateFileOptions{
		Content: encodedContent,
		FileOptions: gitea.FileOptions{
			Message:    message,
			BranchName: "main",
		},
	}

	_, _, err = client.CreateFile(username, repoName, filename, fileOpts)
	if err != nil {
		return "", fmt.Errorf("failed to create file in Gitea: %w", err)
	}

	log.Info("Successfully pushed Argo application", "repo", repoName, "file", filename)
	return filename, nil
}

func GetMostRecentNodeCondition(node corev1.Node) *corev1.NodeCondition {
	var latest *corev1.NodeCondition
	for i := range node.Status.Conditions {
		cond := &node.Status.Conditions[i]
		if latest == nil || cond.LastTransitionTime.After(latest.LastTransitionTime.Time) {
			latest = cond
		}
	}
	return latest
}

func GetNodeStatusSummary(node corev1.Node) string {
	var readyCond *corev1.NodeCondition
	conditionMap := make(map[corev1.NodeConditionType]corev1.ConditionStatus)

	for i := range node.Status.Conditions {
		cond := node.Status.Conditions[i]
		conditionMap[cond.Type] = cond.Status
		if cond.Type == corev1.NodeReady {
			if readyCond == nil || cond.LastTransitionTime.After(readyCond.LastTransitionTime.Time) {
				readyCond = &cond
			}
		}
	}

	if readyCond == nil {
		return "Node status unknown (no Ready condition)"
	}

	status := "Unknown"
	switch readyCond.Status {
	case corev1.ConditionTrue:
		status = "Ready"
	case corev1.ConditionFalse:
		status = fmt.Sprintf("NotReady (%s)", readyCond.Reason)
	case corev1.ConditionUnknown:
		status = "Unknown"
	}

	// Add more context
	var problems []string
	if conditionMap[corev1.NodeMemoryPressure] == corev1.ConditionTrue {
		problems = append(problems, "MemoryPressure")
	}
	if conditionMap[corev1.NodeDiskPressure] == corev1.ConditionTrue {
		problems = append(problems, "DiskPressure")
	}
	if conditionMap[corev1.NodePIDPressure] == corev1.ConditionTrue {
		problems = append(problems, "PIDPressure")
	}
	if conditionMap[corev1.NodeNetworkUnavailable] == corev1.ConditionTrue {
		problems = append(problems, "NetworkUnavailable")
	}

	if len(problems) > 0 {
		status += " (Issues: " + strings.Join(problems, ", ") + ")"
	}

	return status
}

// IsPackageTransitioned returns true if the given package is already in the TransitionedPackages list.
func IsPackageTransitioned(clusterPolicy *transitionv1.ClusterPolicy, pkg transitionv1.PackageSelector) bool {

	var latestMatch *transitionv1.TransitionedPackages
	for i, transitioned := range clusterPolicy.Status.TransitionedPackages {
		for _, sel := range transitioned.PackageSelectors {
			if sel.Name == pkg.Name {
				if latestMatch == nil || transitioned.LastTransitionTime.After(latestMatch.LastTransitionTime.Time) {
					latestMatch = &clusterPolicy.Status.TransitionedPackages[i]
				}
			}
		}
	}

	if latestMatch != nil {
		switch latestMatch.PackageTransitionCondition {
		case transitionv1.PackageTransitionConditionCompleted:
			return true
		case transitionv1.PackageTransitionConditionInProgress:
			return false
		}
	}

	// for _, transitioned := range clusterPolicy.Status.TransitionedPackages {
	// 	for _, sel := range transitioned.PackageSelectors {
	// 		if sel.Name == pkg.Name {
	// 			if transitioned.PackageTransitionCondition == transitionv1.PackageTransitionConditionCompleted {
	// 				return true

	// 			}
	// 			if transitioned.PackageTransitionCondition == transitionv1.PackageTransitionConditionInProgress {
	// 				return false
	// 			}
	// 		}
	// 	}
	// }
	return false
}

func CreateAndPushVeleroRestore(
	ctx context.Context,
	client *gitea.Client,
	username, repoName, folder string,
	clusterPolicy *transitionv1.ClusterPolicy,
	transitionPackage transitionv1.PackageSelector,
	log logr.Logger,
	backupInfo transitionv1.BackupInformation,
	TargetClusterk8sClient client.Client, appName string,
) (string, error) {

	// excludedResources := []string{"events.k8s.io", "nodes"}
	includedNamespaces := []string{"*"}
	itemOperationTimeout := "4h0m0s"

	app := VeleroRestore{
		APIVersion: "velero.io/v1",
		Kind:       "Restore",
	}

	app.Metadata.Name = fmt.Sprintf("velero-%s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)
	app.Metadata.Namespace = "velero"

	app.Spec.BackupName = backupInfo.Name
	// app.Spec.ExcludedResources = excludedResources
	app.Spec.IncludedNamespaces = includedNamespaces
	// app.Spec.IncludedResources = []string{"persistentvolumeclaims", "secrets", "configmaps"}
	// app.Spec.ExcludedResources = []string{"deployments", "replicasets", "statefulsets", "daemonsets", "pods", "services", "ingresses", "networkpolicies", "horizontalpodautoscalers"}
	app.Spec.PreserveNodePorts = true
	// app.Spec.RestorePVs = true
	app.Spec.ItemOperationTimeout = itemOperationTimeout

	yamlData, err := yaml.Marshal(app)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Velero restore YAML: %w", err)
	}

	timestamp := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("%s/velero-restore-%s.yaml", folder, timestamp)
	message := fmt.Sprintf("Velero restore: %s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)

	encodedContent := base64.StdEncoding.EncodeToString(yamlData)

	fileOpts := gitea.CreateFileOptions{
		Content: encodedContent,
		FileOptions: gitea.FileOptions{
			Message:    message,
			BranchName: "main",
		},
	}

	_, _, err = client.CreateFile(username, repoName, filename, fileOpts)
	if err != nil {
		return "", fmt.Errorf("failed to create velero restore file in Gitea: %w", err)
	}

	log.Info("Successfully pushed Velero restore", "repo", repoName, "file", filename)

	err = TriggerArgoCDSyncWithKubeClient(TargetClusterk8sClient, appName, "argocd")
	if err != nil {
		log.Error(err, "Failed to trigger ArgoCD sync with kube client")
		message += "; but the ArgoCD sync was not triggered successfully"
	}

	log.Info("DELAYING FOR 5 SECONDS", "repo", repoName, "file", filename)

	//delay 5s and push argocd app to avoid overriding the configs from velero
	// time.Sleep(5 * time.Second)

	// Wait for restore completion instead of fixed delay
	if err := waitForVeleroRestoreCompletion(TargetClusterk8sClient, app.Metadata.Name, app.Metadata.Namespace, 30*time.Minute, log); err != nil {
		return "", err
	}

	log.Info("Velero restore completed successfully, time to push argo app", "argoapp", repoName)

	// we have to push argo app to the same folder
	ignoreDifferences := []ArgoAppSkipResourcesIgnoreDifferences{}

	_, err = CreateAndPushArgoApp(ctx, client, username, repoName, folder, clusterPolicy, transitionPackage, ignoreDifferences, log)
	if err != nil {
		return "", fmt.Errorf("stateful workload final restore file - failed to create argocd application restore file in gitea: %w", err)
	}
	return filename, nil
}

func TriggerArgoCDSyncWithKubeClient(k8sClient client.Client, appName, namespace string) error {
	ctx := context.TODO()

	// Get the current Application object
	var app argov1alpha1.Application
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      appName,
		Namespace: namespace,
	}, &app)
	if err != nil {
		return fmt.Errorf("failed to get Argo CD application: %w", err)
	}

	// Deep copy to modify
	// updated := app.DeepCopy()
	// now := time.Now().UTC()

	// Update Operation field to trigger sync
	app.Operation = &argov1alpha1.Operation{
		Sync: &argov1alpha1.SyncOperation{
			Revision: "HEAD",
			SyncStrategy: &argov1alpha1.SyncStrategy{
				Apply: &argov1alpha1.SyncStrategyApply{
					Force: true, // This enables kubectl apply --force
				},
			},
		},
		InitiatedBy: argov1alpha1.OperationInitiator{
			Username: "gitea-client",
		},
		// StartedAt: &now,
	}

	//doing operations for this argocd application
	// fmt.Printf("Triggering sync for application %v", app)
	// fmt.Printf("Triggering sync for application %s in namespace %s\n", appName, namespace)

	if err := k8sClient.Update(ctx, &app); err != nil {
		return fmt.Errorf("failed to update application with sync operation: %w", err)
	}

	return nil
}

// TriggerArgoCDSync patches an Argo CD Application to trigger a sync operation
func TriggerArgoCDSync(k8sClient client.Client, appName, namespace string) error {
	ctx := context.TODO()

	// Get the current Application as unstructured
	app := &unstructured.Unstructured{}
	app.SetGroupVersionKind(argoAppGVR)

	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      appName,
		Namespace: namespace,
	}, app); err != nil {
		return fmt.Errorf("failed to get Argo CD application: %w", err)
	}

	// Make a deep copy for modification
	modified := app.DeepCopy()

	operation := map[string]interface{}{
		"sync": map[string]interface{}{
			"revision": "HEAD",
		},
		"initiatedBy": map[string]interface{}{
			"username": "gitea-client",
		},
	}

	if err := unstructured.SetNestedMap(modified.Object, operation, "operation"); err != nil {
		return fmt.Errorf("failed to set sync operation: %w", err)
	}

	// Create a JSON Merge Patch
	modifiedJSON, err := json.Marshal(modified)
	if err != nil {
		return err
	}

	patch := client.RawPatch(types.MergePatchType, modifiedJSON)
	return k8sClient.Patch(ctx, modified, patch)
}

// check and wait for velero resources to indicate restore complete
// Waits for Velero restore CR to be Completed or Failed
func waitForVeleroRestoreCompletion(k8sClient client.Client, restoreName, namespace string, timeout time.Duration, log logr.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Velero restore %s to complete", restoreName)
		case <-ticker.C:
			var restore velerov1.Restore
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: namespace}, &restore); err != nil {
				return fmt.Errorf("failed to get Velero restore %s: %w", restoreName, err)
			}

			phase := restore.Status.Phase
			log.Info("Velero restore status check", "restore", restoreName, "phase", phase)

			switch phase {
			case velerov1.RestorePhaseCompleted:
				log.Info("Velero restore completed successfully", "restore", restoreName)
				return nil
			case velerov1.RestorePhaseFailed:
				return fmt.Errorf("velero restore %s failed", restoreName)
			}
		}
	}
}

func CreateAndPushLiveStateBackupRestore(
	ctx context.Context,
	client *gitea.Client,
	username, repoName, folder string,
	clusterPolicy *transitionv1.ClusterPolicy,
	transitionPackage transitionv1.PackageSelector,
	log logr.Logger,
	backupInfo transitionv1.BackupInformation,
	checkpoint transitionv1.Checkpoint,
	TargetClusterk8sClient client.Client,
	appName string,
) (string, error) {
	/*
		//create pod resource
		workloadPod := buildCleanPodFromCheckpoint(&checkpoint)

		yamlData, err := yaml.Marshal(workloadPod)
		if err != nil {
			return "", fmt.Errorf("failed to marshal pod recovery restore YAML: %w", err)
		}

		timestamp := time.Now().Format("20060102-150405")
		filename := fmt.Sprintf("%s/pod-restore-%s.yaml", folder, timestamp)
		message := fmt.Sprintf("Pod restore: %s-%s", clusterPolicy.Spec.ClusterSelector.Name, transitionPackage.Name)

		encodedContent := base64.StdEncoding.EncodeToString(yamlData)

		fileOpts := gitea.CreateFileOptions{
			Content: encodedContent,
			FileOptions: gitea.FileOptions{
				Message:    message,
				BranchName: "main",
			},
		}

		_, _, err = client.CreateFile(username, repoName, filename, fileOpts)
		if err != nil {
			return "", fmt.Errorf("failed to create live pod restore file in Gitea: %w", err)
		}

		log.Info("Successfully pushed live pod restore", "repo", repoName, "file", filename)

		err = TriggerArgoCDSyncWithKubeClient(TargetClusterk8sClient, appName, "argocd")
		if err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync with kube client")
			message += "; but the ArgoCD sync was not triggered successfully"
		}

		log.Info("DELAYING FOR 5 SECONDS", "repo", repoName, "file", filename)

		//delay 5s and push argocd app to avoid overriding the configs from velero
		// time.Sleep(5 * time.Second)

		// Wait for restore completion instead of fixed delay
		// if err := waitForVeleroRestoreCompletion(TargetClusterk8sClient, app.Metadata.Name, app.Metadata.Namespace, 30*time.Minute, log); err != nil {
		// 	return "", err
		// }

		//jsonpointers

	*/

	ignoreDifferences := []ArgoAppSkipResourcesIgnoreDifferences{
		{
			Group: "",
			Kind:  checkpoint.Spec.ResourceRef.Kind,
			Name:  checkpoint.Spec.ResourceRef.Name,
			JsonPointers: []string{
				"/spec/containers",
			},
		},
	}

	log.Info("Live state restore completed successfully, time to push argo app", "argoapp", repoName)

	// we have to push argo app to the same folder
	_, err := CreateAndPushArgoApp(ctx, client, username, repoName, folder, clusterPolicy, transitionPackage, ignoreDifferences, log)
	if err != nil {
		return "", fmt.Errorf("stateful workload final restore file - failed to create argocd application restore file in gitea: %w", err)
	}
	return "filename", nil
}

type SimplePod struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   metav1.ObjectMeta `yaml:"metadata"`
	Spec       corev1.PodSpec    `yaml:"spec"`
}

func buildCleanPodFromCheckpoint(cp *transitionv1.Checkpoint) map[string]interface{} {
	// Build minimal containers slice
	containers := []map[string]interface{}{}
	for _, c := range cp.Spec.PodRef.ContainerRef.Containers {
		cont := map[string]interface{}{
			"name":  c.Name,
			"image": cp.Status.LastCheckpointImage,
		}
		if len(c.Args) > 0 {
			cont["args"] = c.Args
		}
		if len(c.Ports) > 0 {
			// Only include containerPort and protocol
			ports := []map[string]interface{}{}
			for _, p := range c.Ports {
				port := map[string]interface{}{
					"containerPort": p.ContainerPort,
				}
				if p.Protocol != "" {
					port["protocol"] = string(p.Protocol)
				}
				ports = append(ports, port)
			}
			cont["ports"] = ports
		}
		containers = append(containers, cont)
	}

	// Build minimal Pod manifest
	pod := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]interface{}{
			"name":      cp.Spec.PodRef.Name,
			"namespace": cp.Spec.PodRef.Namespace,
			"labels": map[string]string{
				"checkpoint": cp.Name,
			},
		},
		"spec": map[string]interface{}{
			"containers": containers,
		},
	}

	return pod
}
