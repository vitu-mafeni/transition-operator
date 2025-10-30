package checkpointtransition

import (
	"context"
	"fmt"

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
	// --- List existing checkpoints ---
	checkpoints := &transitionv1.CheckpointList{}
	if err := mgmtClient.List(ctx, checkpoints, &client.ListOptions{}); err != nil {
		log.Error(err, "Failed to list checkpoints")
		return err
	}

	processed := make(map[string]struct{}) // Deduplication

	// --- Trigger transition on existing checkpoints for this package ---
	for _, checkpoint := range checkpoints.Items {
		annotations := checkpoint.Spec.ResourceRef.Annotations
		if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
			annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {

			if helpers.IsPackageTransitioned(clusterPolicy, pkg) {
				log.Info("Package already transitioned; skipping", "package", pkg.Name, "pod", checkpoint.Spec.PodRef.Name)
				continue
			}

			key := fmt.Sprintf("%s/%s", checkpoint.Spec.PodRef.Name, pkg.Name)
			if _, seen := processed[key]; seen {
				continue
			}
			processed[key] = struct{}{}

			TransitionOnMissedNodeHealth(ctx, mgmtClient, checkpoint.Spec.PodRef.Name, pkg, clusterPolicy, checkpoint.Spec.ClusterRef.Name)
		}
	}

	// --- If no checkpoints exist for this package, create new ones from pods ---
	packageCheckpointsExist := false
	for _, checkpoint := range checkpoints.Items {
		if checkpoint.Spec.ResourceRef.Annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {
			packageCheckpointsExist = true
			break
		}
	}

	if !packageCheckpointsExist {
		// Get workload cluster client
		_, workloadClusterClient, _, err := capi.GetWorkloadClusterClient(ctx, mgmtClient, workloadClusterName)
		if err != nil {
			return err
		}
		if workloadClusterClient == nil {
			log.Info("Cluster client not available yet; skipping checkpoint creation", "cluster", workloadClusterName)
			return fmt.Errorf("workload cluster %q client not available yet", workloadClusterName)
		}

		// List all pods
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
				log.Info("Pod has no owner; skipping", "pod", pod.Name)
				continue
			}

			workloadID := fmt.Sprintf("%s/%s/%s", namespace, workloadName, workloadKind)

			parentObject := &metav1.PartialObjectMetadata{}
			parentAnnotations, parentKind, err := helpers.GetParentAnnotations(ctx, workloadClusterClient, workloadKind, workloadName, namespace)
			if err != nil {
				log.Error(fmt.Errorf("error getting parent annotations"), err.Error())
				continue
			}

			if parentAnnotations != nil {
				parentObject.Kind = parentKind
				parentObject.Name = parentAnnotations.Name
				parentObject.Namespace = parentAnnotations.Namespace
				parentObject.Annotations = parentAnnotations.Annotations
			} else {
				continue
			}

			if parentAnnotations.Annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] != "true" ||
				parentAnnotations.Annotations["transition.dcnlab.ssu.ac.kr/packageName"] != pkg.Name {
				continue
			}

			if helpers.IsPackageTransitioned(clusterPolicy, pkg) {
				log.Info("Package already transitioned; skipping", "package", pkg.Name, "pod", pod.Name)
				continue
			}

			key := fmt.Sprintf("%s/%s", workloadID, pkg.Name)
			if _, seen := processedPods[key]; seen {
				continue
			}
			processedPods[key] = struct{}{}

			if err := CreateCheckpointCR(ctx, mgmtClient, &pod, pkg, clusterPolicy, parentObject); err != nil {
				log.Error(err, "Failed to create Checkpoint CR for pod", "pod", pod.Name, "package", pkg.Name)
				return err
			}
		}
	}

	return nil

}

func TransitionOnMissedNodeHealth(ctx context.Context, mgmtClient client.Client, podName string, transitionPackage transitionv1.PackageSelector, clusterPolicy *transitionv1.ClusterPolicy, workloadClusterName string) {
	log := logf.FromContext(ctx)
	// log.Info("Transitioning Selected live workload", "pod", pod.Name, "package", transitionPackage.Name)

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
	// log.Info("Authenticated with Gitea", "username", user.UserName)

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
		log.Info("Repository named 'dr' not found; using last repository as fallback, skipping transition", clusterPolicy.Name)
		// drRepo = repos[len(repos)-1]
		return
	}

	helpers.LogRepositories(log, repos)
	// log.Info("Source repository", "cluster", clusterPolicy.Spec.ClusterSelector.Name, "repo", sourceRepo)

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
	associatedCheckpoint := transitionv1.Checkpoint{}

	switch transitionPackage.PackageType {
	case transitionv1.PackageTypeStateful:
		// log.Info("Handling stateful live package transition", "package", transitionPackage.Name)

		backupMatching := transitionv1.BackupInformation{}

		for _, backup := range transitionPackage.BackupInformation {
			switch backup.BackupType {
			case transitionv1.BackupTypeSchedule:

				checkpointName := fmt.Sprintf("checkpoint-%s-%s-%s", clusterPolicy.Name, podName, transitionPackage.Name)
				//get checkpoint
				checkpoint := &transitionv1.Checkpoint{}
				err := mgmtClient.Get(ctx, types.NamespacedName{Name: checkpointName, Namespace: "default"}, checkpoint)
				if err != nil {
					log.Error(err, "Failed to get checkpoint", "name", checkpointName)
					return
				}
				if checkpoint.Status.LastCheckpointImage == "" {
					log.Info("Checkpoint has no associated backup; cannot proceed with live state transition", "checkpoint", checkpointName)
					return
				}

				associatedCheckpoint = *checkpoint

			case transitionv1.BackupTypeManual:
				// scheme := runtime.NewScheme()
				// _ = velero.AddToScheme(scheme)
				log.Info("Looking for stateful live manual backup - nothing yet", "name", backup.Name)

			}
		}

		if associatedCheckpoint.Name == "" {
			log.Error(err, "Failed to find a proper associated live backup - backup name cannot be empty")
			return
		}

		tmpDir, matches, err := giteaclient.CheckRepoForMatchingManifests(ctx, sourceRepo, "main", &associatedCheckpoint.Spec.ResourceRef)

		if err != nil {
			log.Error(err, "Failed to find matching manifests in source repo", "repo", sourceRepo)
			return
		}

		if len(matches) > 0 {
			log.Info("Found matching manifests",
				"repo", sourceRepo,
				"tmpDir", tmpDir,
				"files", matches)

			for _, f := range matches {
				// fullPath := filepath.Join(tmpDir, f)
				if err := giteaclient.UpdateResourceContainers(f, associatedCheckpoint.Status.LastCheckpointImage, associatedCheckpoint.Status.OriginalImage); err != nil {
					log.Error(err, "failed to update containers in manifest", "file", f)
					return
				}
				log.Info("Updated containers in manifest", "file", f)
			}

			// commit & push changes back to Gitea
			// log.Info("will commit and push changes back to git here")
			commitMsg := fmt.Sprintf("Update container image %s/%s", associatedCheckpoint.Status.LastCheckpointImage, associatedCheckpoint.Status.OriginalImage)

			username, password, _, err := giteaclient.GetGiteaSecretUserNamePassword(ctx, mgmtClient)
			if err != nil {
				log.Error(err, "failed to get gitea")
			}

			if err := giteaclient.CommitAndPush(ctx, tmpDir, "main", sourceRepo, username, password, commitMsg); err != nil {
				log.Error(err, "failed to commit & push changes")
			}

			// log.Info("Changes committed and pushed", "repo", sourceRepo)

			_, err = helpers.CreateAndPushLiveStateBackupRestore(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, log, backupMatching, associatedCheckpoint, mgmtClient, targetClusterName+"-dr")
			if err != nil {
				log.Error(err, "Failed to push live workloads pod manifest")
				//add status that it failed to transition the package
				clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
					PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
					LastTransitionTime:         metav1.Now(),
					PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
					PackageTransitionMessage:   err.Error(),
				})

				if err := mgmtClient.Status().Update(ctx, clusterPolicy); err != nil {
					log.Error(err, "Failed to update ClusterPolicy status after transition failure")
					return
				}
				return
			}

			message := "Transitioned stateful package successfully"

			err = helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd")
			if err != nil {
				log.Error(err, "Failed to trigger ArgoCD sync with kube client")
				message += "; but the ArgoCD sync was not triggered successfully"
			}
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
				PackageTransitionMessage:   message,
			})

			if err := mgmtClient.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after successful stateful transition")
				return
			}
			log.Info("Successfully transitioned stateful package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
			return
		}
	case transitionv1.PackageTypeStateless:
		log.Info("Handling stateless package transition", "package", transitionPackage.Name)
		ignoreDifferences := []helpers.ArgoAppSkipResourcesIgnoreDifferences{}
		_, err := helpers.CreateAndPushArgoApp(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, ignoreDifferences, log)
		if err != nil {
			log.Error(err, "Failed to push ArgoCD app manifest")
			//add status that it failed to transition the package
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
				PackageTransitionMessage:   err.Error(),
			})

			if err := mgmtClient.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after transition failure")
				return
			}
			return
		}

		message := "Transitioned stateless package successfully"

		err = helpers.TriggerArgoCDSyncWithKubeClient(targetClusterClient, targetClusterName+"-dr", "argocd")
		if err != nil {
			log.Error(err, "Failed to trigger ArgoCD sync with kube client")
			message += "; but the ArgoCD sync was not triggered successfully"
		}
		clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
			PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
			LastTransitionTime:         metav1.Now(),
			PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
			PackageTransitionMessage:   message,
		})

		if err := mgmtClient.Status().Update(ctx, clusterPolicy); err != nil {
			log.Error(err, "Failed to update ClusterPolicy status after successful transition")
			return
		}
		log.Info("Successfully transitioned stateless package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
		return

	default:
		log.Info("Unknown package type; skipping transition", "package", transitionPackage.Name)
	}

}
