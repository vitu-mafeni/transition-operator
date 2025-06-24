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
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func LogRepositories(log logr.Logger, repos []*gitea.Repository) {
	for _, repo := range repos {
		log.Info("Found repository", "name", repo.Name, "full_name", repo.FullName, "url", repo.CloneURL)
	}
}

func DetermineTargetRepo(policy *transitionv1.ClusterPolicy, log logr.Logger) (string, bool) {
	highestWeight := -1
	var repo string

	for _, target := range policy.Spec.TargetClusterPolicy.PreferClusters {
		if target.RepoType != "git" {
			log.Info("Skipping non-git repository", "cluster", target.Name)
			continue
		}
		if target.Weight > highestWeight {
			highestWeight = target.Weight
			repo = target.Name + "-dr"
		}
	}

	return repo, repo != ""
}

func CreateAndPushArgoApp(
	ctx context.Context,
	client *gitea.Client,
	username, repoName, folder string,
	clusterPolicy *transitionv1.ClusterPolicy,
	transitionPackage transitionv1.PackageSelector,
	log logr.Logger,
) (error, string) {
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

	app.Spec.SyncPolicy.Automated.Prune = true
	app.Spec.SyncPolicy.Automated.SelfHeal = true
	app.Spec.SyncPolicy.Automated.AllowEmpty = true
	app.Spec.SyncPolicy.SyncOptions = []string{"CreateNamespace=true"}

	// Correct placement of IgnoreDifferences
	app.Spec.IgnoreDifferences = []struct {
		Group string `yaml:"group"`
		Kind  string `yaml:"kind"`
	}{
		{Group: "fn.kpt.dev", Kind: "ApplyReplacements"},
		{Group: "fn.kpt.dev", Kind: "StarlarkRun"},
	}

	yamlData, err := yaml.Marshal(app)
	if err != nil {
		return fmt.Errorf("failed to marshal Argo application YAML: %w", err), ""
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
		return fmt.Errorf("failed to create file in Gitea: %w", err), ""
	}

	log.Info("Successfully pushed Argo application", "repo", repoName, "file", filename)
	return nil, filename
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
		return "", fmt.Errorf("failed to create file in Gitea: %w", err)
	}

	log.Info("Successfully pushed Velero restore", "repo", repoName, "file", filename)
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
	updated := app.DeepCopy()
	// now := time.Now().UTC()

	// Update Operation field to trigger sync
	updated.Operation = &argov1alpha1.Operation{
		Sync: &argov1alpha1.SyncOperation{
			Revision: "HEAD",
		},
		InitiatedBy: argov1alpha1.OperationInitiator{
			Username: "gitea-client",
		},
		// StartedAt: &now,
	}

	// Create a patch
	origData, err := json.Marshal(app)
	if err != nil {
		return err
	}
	modifiedData, err := json.Marshal(updated)
	if err != nil {
		return err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(origData, modifiedData, argov1alpha1.Application{})
	if err != nil {
		return err
	}

	// Apply patch
	return k8sClient.Patch(ctx, updated, client.RawPatch(types.MergePatchType, patchBytes))
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
