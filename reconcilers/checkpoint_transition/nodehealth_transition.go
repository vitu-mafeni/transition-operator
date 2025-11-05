package checkpointtransition

import (
	"context"
	"fmt"
	"sync"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/nephio-project/nephio/controllers/pkg/resource"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capi "github.com/vitu1234/transition-operator/reconcilers/capi"
	giteaclient "github.com/vitu1234/transition-operator/reconcilers/gitaclient"
	"github.com/vitu1234/transition-operator/reconcilers/helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

//======NODE HEALTH BASED TRANSITION=====

// transition if nodehealth seen for a while
func TriggerTransitionOnMissedNodeHealth(
	ctx context.Context,
	mgmtClient client.Client,
	pkg transitionv1.PackageSelector,
	clusterPolicy *transitionv1.ClusterPolicy,
	workloadClusterName string,
) error {
	log := logf.FromContext(ctx)

	checkpoints := &transitionv1.CheckpointList{}
	if err := mgmtClient.List(ctx, checkpoints, &client.ListOptions{}); err != nil {
		log.Error(err, "Failed to list checkpoints")
		return err
	}

	processed := make(map[string]struct{}) // deduplication
	var wg sync.WaitGroup
	sem := make(chan struct{}, 2) // limit concurrency to 2 transitions

	for _, checkpoint := range checkpoints.Items {
		annotations := checkpoint.Spec.ResourceRef.Annotations
		if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] != "true" ||
			annotations["transition.dcnlab.ssu.ac.kr/packageName"] != pkg.Name {
			continue
		}

		if helpers.IsPackageTransitioned(clusterPolicy, pkg) {
			log.Info("Package already transitioned; skipping", "package", pkg.Name, "pod", checkpoint.Spec.PodRef.Name)
			continue
		}

		key := fmt.Sprintf("%s/%s", checkpoint.Spec.PodRef.Name, pkg.Name)
		if _, seen := processed[key]; seen {
			continue
		}
		processed[key] = struct{}{}

		wg.Add(1)
		sem <- struct{}{}
		go func(cp transitionv1.Checkpoint) {
			defer wg.Done()
			defer func() { <-sem }()
			TransitionOnMissedNodeHealth(ctx, mgmtClient, cp.Spec.PodRef.Name, pkg, clusterPolicy, cp.Spec.ClusterRef.Name)
		}(checkpoint)
	}

	wg.Wait()

	// --- If no checkpoints exist for this package, create new ones from pods ---
	hasCheckpoint := false
	for _, c := range checkpoints.Items {
		if c.Spec.ResourceRef.Annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {
			hasCheckpoint = true
			break
		}
	}

	if !hasCheckpoint {
		// get workload cluster client
		_, workloadClusterClient, _, err := capi.GetWorkloadClusterClient(ctx, mgmtClient, workloadClusterName)
		if err != nil {
			return err
		}
		if workloadClusterClient == nil {
			log.Info("Cluster client not available yet; skipping checkpoint creation", "cluster", workloadClusterName)
			return fmt.Errorf("workload cluster %q client not available yet", workloadClusterName)
		}

		podList := &corev1.PodList{}
		if err := workloadClusterClient.List(ctx, podList, &client.ListOptions{}); err != nil {
			log.Error(err, "Failed to list pods in workload cluster", "cluster", workloadClusterName)
			return err
		}

		processedPods := make(map[string]struct{})

		for _, pod := range podList.Items {
			namespace := pod.Namespace
			workloadKind, workloadName, hasOwner := helpers.GetWorkloadOwnerControllerInfo(pod)
			if !hasOwner {
				continue
			}

			parentAnnotations, parentKind, err := helpers.GetParentAnnotations(ctx, workloadClusterClient, workloadKind, workloadName, namespace)
			if err != nil {
				log.Error(err, "Error getting parent annotations")
				continue
			}

			if parentAnnotations == nil ||
				parentAnnotations.Annotations["transition.dcnlab.ssu.ac.kr/packageName"] != pkg.Name {
				continue
			}

			if helpers.IsPackageTransitioned(clusterPolicy, pkg) {
				continue
			}

			key := fmt.Sprintf("%s/%s/%s", namespace, workloadName, pkg.Name)
			if _, seen := processedPods[key]; seen {
				continue
			}
			processedPods[key] = struct{}{}

			parentObject := &metav1.PartialObjectMetadata{
				TypeMeta: metav1.TypeMeta{
					Kind: parentKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:        parentAnnotations.Name,
					Namespace:   parentAnnotations.Namespace,
					Annotations: parentAnnotations.Annotations,
				},
			}

			if err := CreateCheckpointCR(ctx, mgmtClient, &pod, pkg, clusterPolicy, parentObject); err != nil {
				log.Error(err, "Failed to create Checkpoint CR for pod", "pod", pod.Name, "package", pkg.Name)
				return err
			}
		}
	}

	return nil
}

func TransitionOnMissedNodeHealth(
	ctx context.Context,
	mgmtClient client.Client,
	podName string,
	transitionPackage transitionv1.PackageSelector,
	clusterPolicy *transitionv1.ClusterPolicy,
	workloadClusterName string,
) {
	log := logf.FromContext(ctx)
	apiClient := resource.NewAPIPatchingApplicator(mgmtClient)

	giteaClient, err := giteaclient.GetClient(ctx, apiClient)
	if err != nil {
		log.Error(err, "Failed to initialize Gitea client")
		return
	}
	if !giteaClient.IsInitialized() {
		log.Info("Gitea client not yet initialized, retrying later")
		return
	}

	user, resp, err := giteaClient.GetMyUserInfo()
	if err != nil {
		log.Error(err, "Failed to get Gitea user info", "response", resp)
		return
	}

	sourceRepo := clusterPolicy.Spec.ClusterSelector.Repo
	if sourceRepo == "" {
		log.Info("ClusterPolicy does not specify source repo; skipping transition")
		return
	}

	repos, resp, err := giteaClient.Get().ListMyRepos(gitea.ListReposOptions{})
	if err != nil {
		log.Error(err, "Failed to list Gitea repositories", "response", resp)
		return
	}
	if len(repos) == 0 {
		log.Info("No repositories found for user", "username", user.UserName)
		return
	}

	var drRepo *gitea.Repository
	for i := range repos {
		if repos[i].Name == "dr" {
			drRepo = repos[i]
			break
		}
	}
	if drRepo == nil {
		log.Info("Repository named 'dr' not found; skipping transition", clusterPolicy.Name)
		return
	}

	targetRepoName, targetClusterName, found := helpers.DetermineTargetRepo(clusterPolicy, log)
	if !found {
		log.Info("No suitable target repository found; canceling transition")
		return
	}

	_, targetClusterClient, _, err := capi.GetWorkloadClusterClient(ctx, mgmtClient, targetClusterName)
	if err != nil {
		log.Error(err, "Failed to get target workload cluster client", "cluster", targetClusterName)
		return
	}

	var associatedCheckpoint transitionv1.Checkpoint

	switch transitionPackage.PackageType {
	case transitionv1.PackageTypeStateful:
		var backupMatching transitionv1.BackupInformation

		for _, backup := range transitionPackage.BackupInformation {
			if backup.BackupType == transitionv1.BackupTypeSchedule {
				checkpointName := fmt.Sprintf("checkpoint-%s-%s-%s", clusterPolicy.Name, podName, transitionPackage.Name)
				checkpoint := &transitionv1.Checkpoint{}
				if err := mgmtClient.Get(ctx, types.NamespacedName{Name: checkpointName, Namespace: "default"}, checkpoint); err != nil {
					log.Error(err, "Failed to get checkpoint", "name", checkpointName)
					return
				}
				if checkpoint.Status.LastCheckpointImage == "" {
					log.Info("Checkpoint has no associated backup; cannot proceed with live state transition", "checkpoint", checkpointName)
					return
				}
				associatedCheckpoint = *checkpoint
			}
		}

		if associatedCheckpoint.Name == "" {
			log.Error(err, "Failed to find associated live backup - backup name cannot be empty")
			return
		}

		tmpDir, matches, err := giteaclient.CheckRepoForMatchingManifests(ctx, sourceRepo, "main", &associatedCheckpoint.Spec.ResourceRef)
		if err != nil {
			log.Error(err, "Failed to find matching manifests in source repo", "repo", sourceRepo)
			return
		}

		if len(matches) == 0 {
			log.Info("No matching manifests found", "repo", sourceRepo)
			return
		}

		log.Info("Found matching manifests", "count", len(matches), "repo", sourceRepo)

		// Parallelize manifest updates (limit concurrency to avoid CPU exhaustion)
		var wg sync.WaitGroup
		sem := make(chan struct{}, 2) // Limit to 2 concurrent file operations (good for 2 vCPUs)
		errChan := make(chan error, len(matches))

		for _, f := range matches {
			wg.Add(1)
			go func(file string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				if err := giteaclient.UpdateResourceContainers(file,
					associatedCheckpoint.Status.LastCheckpointImage,
					associatedCheckpoint.Status.OriginalImage); err != nil {
					errChan <- fmt.Errorf("failed to update manifest %s: %w", file, err)
				}
			}(f)
		}
		wg.Wait()
		close(errChan)

		for e := range errChan {
			if e != nil {
				log.Error(e, "Manifest update error")
				return
			}
		}

		// Commit & push changes
		username, password, _, err := giteaclient.GetGiteaSecretUserNamePassword(ctx, mgmtClient)
		if err != nil {
			log.Error(err, "Failed to get Gitea credentials")
			return
		}

		commitMsg := fmt.Sprintf("Update container image %s/%s",
			associatedCheckpoint.Status.LastCheckpointImage,
			associatedCheckpoint.Status.OriginalImage)

		if err := giteaclient.CommitAndPush(ctx, tmpDir, "main", sourceRepo, username, password, commitMsg); err != nil {
			log.Error(err, "Failed to commit & push changes")
			return
		}

		_, err = helpers.CreateAndPushLiveStateBackupRestore(ctx, giteaClient.Get(),
			user.UserName, drRepo.Name, targetRepoName, clusterPolicy,
			transitionPackage, log, backupMatching, associatedCheckpoint,
			mgmtClient, targetClusterName+"-dr")
		if err != nil {
			log.Error(err, "Failed to push live workload pod manifest")
			UpdateClusterPolicyStatus(ctx, mgmtClient, clusterPolicy, transitionPackage, err, transitionv1.PackageTransitionConditionFailed)
			return
		}

		message := "Transitioned stateful package successfully"
		if err := helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd"); err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync")
			message += "; ArgoCD sync failed"
		}

		UpdateClusterPolicyStatus(ctx, mgmtClient, clusterPolicy, transitionPackage, fmt.Errorf("%s", message), transitionv1.PackageTransitionConditionCompleted)
		log.Info("Successfully transitioned stateful package", "package", transitionPackage.Name, "policy", clusterPolicy.Name)

	case transitionv1.PackageTypeStateless:
		log.Info("Handling stateless package transition", "package", transitionPackage.Name)
		ignoreDifferences := []helpers.ArgoAppSkipResourcesIgnoreDifferences{}
		if _, err := helpers.CreateAndPushArgoApp(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, ignoreDifferences, log); err != nil {
			log.Error(err, "Failed to push ArgoCD app manifest")
			UpdateClusterPolicyStatus(ctx, mgmtClient, clusterPolicy, transitionPackage, err, transitionv1.PackageTransitionConditionFailed)
			return
		}

		message := "Transitioned stateless package successfully"
		if err := helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd"); err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync")
			message += "; ArgoCD sync failed"
		}

		UpdateClusterPolicyStatus(ctx, mgmtClient, clusterPolicy, transitionPackage, fmt.Errorf("%s", message), transitionv1.PackageTransitionConditionCompleted)
		log.Info("Successfully transitioned stateless package", "package", transitionPackage.Name, "policy", clusterPolicy.Name)

	default:
		log.Info("Unknown package type; skipping transition", "package", transitionPackage.Name)
	}
}

// UpdateClusterPolicyStatus updates ClusterPolicy transition status cleanly.
func UpdateClusterPolicyStatus(
	ctx context.Context,
	mgmtClient client.Client,
	clusterPolicy *transitionv1.ClusterPolicy,
	transitionPackage transitionv1.PackageSelector,
	updateErr error,
	condition transitionv1.PackageTransitionCondition,
) {
	message := ""
	if updateErr != nil {
		message = updateErr.Error()
	}

	clusterPolicy.Status.TransitionedPackages = append(
		clusterPolicy.Status.TransitionedPackages,
		transitionv1.TransitionedPackages{
			PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
			LastTransitionTime:         metav1.NewTime(time.Now()),
			PackageTransitionCondition: condition,
			PackageTransitionMessage:   message,
		},
	)

	if err := mgmtClient.Status().Update(ctx, clusterPolicy); err != nil {
		// Ideally, log the error but don't panic.
		// Logging should be done at caller site to include context.
	}
}
