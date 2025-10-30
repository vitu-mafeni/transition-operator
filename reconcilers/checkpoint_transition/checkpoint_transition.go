package checkpointtransition

import (
	"context"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capi "github.com/vitu1234/transition-operator/reconcilers/capi"
	"github.com/vitu1234/transition-operator/reconcilers/helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

//performs checkpoint transition actions on the workload cluster

func PerformWorkloadClusterCheckpointAction(
	ctx context.Context,
	mgmtClient client.Client,
	pkg transitionv1.PackageSelector,
	clusterPolicy *transitionv1.ClusterPolicy,
	workloadCluster *capiv1beta1.Cluster,
) error {
	log := logf.FromContext(ctx)

	checkpoints := &transitionv1.CheckpointList{}
	if err := mgmtClient.List(ctx, checkpoints, &client.ListOptions{}); err != nil {
		log.Error(err, "Failed to list checkpoints")
		return err
	}
	processed := make(map[string]struct{}) // Deduplication
	for _, checkpoint := range checkpoints.Items {
		annotations := checkpoint.Annotations

		if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
			annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {

			if helpers.IsPackageTransitioned(clusterPolicy, pkg) {
				log.Info("Package already transitioned; skipping", "package", pkg.Name, "pod", checkpoint.Spec.PodRef.Name)
				continue
			}

			// Deduplicate by workload and package
			key := fmt.Sprintf("%s/%s", checkpoint.Spec.PodRef.Name, pkg.Name)
			if _, seen := processed[key]; seen {
				continue
			}
			processed[key] = struct{}{}

			// log.Info("Matched workload for transition", "workload", workloadID, "package", pkg.Name, "pod", pod.Name)
			// if err := CreateCheckpointCR(ctx, mgmtClient, &pod, pkg, clusterPolicy, parentObject); err != nil {
			// 	log.Error(err, "Failed to create Checkpoint CR for workload/pod resource",
			// 		"workload", workloadID,
			// 		"package", pkg.Name)
			// 	return err
			// }

			TransitionOnMissedNodeHealth(ctx, mgmtClient, checkpoint.Spec.PodRef.Name, pkg, clusterPolicy, workloadCluster)
			continue

		}
	}

	if len(checkpoints.Items) == 0 {
		// log.Info("No checkpoints found for transition")
		// --- Get workload cluster client ---
		_, workloadClusterClient, _, err := capi.GetWorkloadClusterClient(ctx, mgmtClient, workloadCluster.Name)
		if err != nil {
			return err
		}
		if workloadClusterClient == nil {
			log.Info("Cluster client not available yet in ready state", "cluster", workloadCluster.Name)
			return fmt.Errorf("workload cluster %q client not available yet", workloadCluster.Name)
		}

		// --- List all pods ---
		podList := &corev1.PodList{}
		if err := workloadClusterClient.List(ctx, podList, &client.ListOptions{}); err != nil {
			log.Error(err, "Failed to list pods in workload cluster", "cluster", workloadCluster.Name)
			return err
		}

		processed := make(map[string]struct{}) // Deduplication

		// --- Iterate over pods ---
		for _, pod := range podList.Items {
			namespace := pod.Namespace
			workloadKind, workloadName, hasOwner := helpers.GetWorkloadOwnerControllerInfo(pod)
			workloadID := fmt.Sprintf("%s/%s/%s", namespace, workloadName, workloadKind)

			var annotations map[string]string
			// --- Step 1: Pod-level annotations ---
			if !hasOwner {
				// If no owner, skip to avoid pod-level checkpoint creation
				log.Info("Pod has no owner; skipping pod-level checkpoint creation", "pod", pod.Name)
				continue

			}

			// --- Step 2: Parent workload annotations ---
			parentObject := &metav1.PartialObjectMetadata{}
			parentAnnotations, parentKind, err := helpers.GetParentAnnotations(ctx, workloadClusterClient, workloadKind, workloadName, namespace)
			if err != nil {
				log.Error(fmt.Errorf("an error occured finding object parent"), err.Error())
			}

			if parentAnnotations != nil {
				annotations = parentAnnotations.Annotations
				parentObject.Kind = parentKind
				parentObject.Name = parentAnnotations.Name
				parentObject.Namespace = parentAnnotations.Namespace
				parentObject.Annotations = annotations
			}

			// if val, ok := parentAnnotations.Annotations["transition.dcnlab.ssu.ac.kr/packageName"]; ok {
			// 	log.Info("Annotation found", "value", val)

			// 	log.Info("Parent Annotations", "annotations", parentAnnotations.Annotations)
			// }

			if annotations == nil {
				continue
			}

			if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
				annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {

				if helpers.IsPackageTransitioned(clusterPolicy, pkg) {
					log.Info("Package already transitioned; skipping", "package", pkg.Name, "pod", pod.Name)
					continue
				}

				// Deduplicate by workload and package
				key := fmt.Sprintf("%s/%s", workloadID, pkg.Name)
				if _, seen := processed[key]; seen {
					continue
				}
				processed[key] = struct{}{}

				// log.Info("Matched workload for transition", "workload", workloadID, "package", pkg.Name, "pod", pod.Name)
				if err := CreateCheckpointCR(ctx, mgmtClient, &pod, pkg, clusterPolicy, parentObject); err != nil {
					log.Error(err, "Failed to create Checkpoint CR for workload/pod resource",
						"workload", workloadID,
						"package", pkg.Name)
					return err
				}
				continue

			}

		}

		return nil
	}

	return nil
}

// create checkpoint CR in the workload cluster
func CreateCheckpointCR(
	ctx context.Context,
	client client.Client,
	pod *corev1.Pod,
	pkg transitionv1.PackageSelector,
	clusterPolicy *transitionv1.ClusterPolicy,
	parentObject *metav1.PartialObjectMetadata,
) error {
	log := logf.FromContext(ctx)

	schedule := transitionv1.BackupInformation{}

	for _, b := range pkg.BackupInformation {
		if b.BackupType == transitionv1.BackupTypeSchedule {
			schedule = transitionv1.BackupInformation{
				// Name:           b.Name,
				// BackupType:     b.BackupType,
				SchedulePeriod: b.SchedulePeriod,
			}
			break
		}
	}

	//get target cluster from cluster policy with lowest weight
	var targetCluster *transitionv1.ClusterRef
	lowestWeight := int32(math.MaxInt32)
	for _, cluster := range clusterPolicy.Spec.TargetClusterPolicy.PreferClusters {
		if int32(cluster.Weight) < lowestWeight {
			lowestWeight = int32(cluster.Weight)
			targetCluster = &transitionv1.ClusterRef{
				Name: cluster.Name,
				// Repository: cluster.Repo,
			}
		}
	}

	checkpoint := &transitionv1.Checkpoint{
		Spec: transitionv1.CheckpointSpec{
			Schedule: schedule.SchedulePeriod,
			ClusterRef: &transitionv1.ClusterRef{
				Name:       clusterPolicy.Spec.ClusterSelector.Name,
				Repository: clusterPolicy.Spec.ClusterSelector.Repo,
			},
			TargetClusterRef: &transitionv1.ClusterRef{
				Name: targetCluster.Name,
			},
			PodRef: transitionv1.PodRef{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				ContainerRef: transitionv1.ContainerRef{
					Containers: pod.Spec.Containers,
				},
			},

			Registry: transitionv1.Registry{
				URL:        helpers.GetEnv("REGISTRY_URL", "docker.io"), // Example registry URL
				Repository: helpers.GetEnv("REPOSITORY", "vitu1"),       // Example repository
				SecretRef: &transitionv1.SecretRef{
					Name:      helpers.GetEnv("SECRET_NAME_REF", " reg-credentials"), // Example secret name
					Namespace: helpers.GetEnv("SECRET_NAMESPACE_REF", "default"),     // Example secret namespace
				},
			},
		},
	}

	if parentObject != nil {
		// log.Info("Annotations Creating ", "annotations", parentObject.Annotations)
		checkpoint.Spec.ResourceRef = transitionv1.ResourceRef{
			APIVersion:  parentObject.APIVersion,
			Kind:        parentObject.Kind,
			Name:        parentObject.Name,
			Namespace:   parentObject.Namespace,
			Annotations: parentObject.Annotations,
		}
	}

	// Set namespace
	checkpoint.Namespace = "default"

	// Generate a unique name for the Checkpoint CR
	checkpoint.Name = fmt.Sprintf("checkpoint-%s-%s-%s", clusterPolicy.Name, pod.Name, pkg.Name)

	// Create the Checkpoint CR
	// Create the Checkpoint CR
	if err := client.Create(ctx, checkpoint); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Just log and skip, no error returned
			// log.Info("Checkpoint CR already exists, skipping creation", "name", checkpoint.Name)
			return nil
		}
		log.Error(err, "Failed to create Checkpoint CR", "name", checkpoint.Name)
		return err
	}

	// log.Info("Created Checkpoint CR", "name", checkpoint.Name)
	return nil
}
