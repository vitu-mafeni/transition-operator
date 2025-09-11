package checkpointtransition

import (
	"context"
	"fmt"
	"math"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/types"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capi "github.com/vitu1234/transition-operator/reconcilers/capi"
	"github.com/vitu1234/transition-operator/reconcilers/helpers"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		if len(pod.Annotations) > 0 {
			annotations = pod.Annotations
			if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
				annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {

				key := fmt.Sprintf("%s/%s/%s", namespace, pod.Name, pkg.Name) // pod-level dedup
				if _, seen := processed[key]; seen {
					continue
				}
				processed[key] = struct{}{}

				log.Info("Matched pod-level checkpoint policy",
					"pod", pod.Name,
					"package", pkg.Name,
					"namespace", namespace)

				// --- Create pod-level checkpoint CR here ---
				// checkpoint := &transitionv1.Checkpoint{ ... }
				// if err := workloadClusterClient.Create(ctx, checkpoint); err != nil { ... }
				if err := CreateCheckpointCR(ctx, mgmtClient, &pod, pkg, clusterPolicy); err != nil {
					log.Error(err, "Failed to create Checkpoint CR for pod",
						"pod", pod.Name,
						"package", pkg.Name)
					return err
				}

				continue
			}
		}

		// --- Step 2: Parent workload annotations ---
		if hasOwner {
			parentAnnotations := getParentAnnotations(ctx, workloadClusterClient, workloadKind, workloadName, namespace)
			if parentAnnotations != nil {
				annotations = parentAnnotations
			}
		}

		if annotations == nil {
			continue
		}

		// --- Step 3: Match workload-level policy ---
		if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
			annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {

			key := fmt.Sprintf("%s/%s", workloadID, pkg.Name) // workload-level dedup
			if _, seen := processed[key]; seen {
				continue
			}
			processed[key] = struct{}{}

			log.Info("Matched workload-level checkpoint policy",
				"workload", workloadID,
				"package", pkg.Name,
				"pod", pod.Name)

			// --- Create workload-level checkpoint CR here ---
			// checkpoint := &transitionv1.Checkpoint{ ... }
			// if err := workloadClusterClient.Create(ctx, checkpoint); err != nil { ... }
			if err := CreateCheckpointCR(ctx, mgmtClient, &pod, pkg, clusterPolicy); err != nil {
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

// helper function to get parent workload annotations
func getParentAnnotations(
	ctx context.Context,
	c client.Client,
	kind, name, namespace string,
) map[string]string {

	log := logf.FromContext(ctx)
	switch kind {
	case "ReplicaSet":
		rs := &appsv1.ReplicaSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, rs); err != nil {
			log.Error(err, "Failed to get ReplicaSet", "name", name)
			return nil
		}
		deployName, found := helpers.GetWorkloadControllerOwnerName(rs.OwnerReferences, "Deployment")
		if !found {
			return rs.Annotations
		}
		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: deployName, Namespace: namespace}, deploy); err != nil {
			log.Error(err, "Failed to get Deployment", "name", deployName)
			return rs.Annotations
		}
		return deploy.Annotations

	case "Deployment":
		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy); err != nil {
			log.Error(err, "Failed to get Deployment", "name", name)
			return nil
		}
		return deploy.Annotations

	case "DaemonSet":
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ds); err != nil {
			log.Error(err, "Failed to get DaemonSet", "name", name)
			return nil
		}
		return ds.Annotations

	case "StatefulSet":
		ss := &appsv1.StatefulSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ss); err != nil {
			log.Error(err, "Failed to get StatefulSet", "name", name)
			return nil
		}
		return ss.Annotations
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

	// Set namespace
	checkpoint.Namespace = "default"

	// Generate a unique name for the Checkpoint CR
	checkpoint.Name = fmt.Sprintf("checkpoint-%s-%s-%s", clusterPolicy.Name, pod.Name, pkg.Name)

	// Create the Checkpoint CR
	// Create the Checkpoint CR
	if err := client.Create(ctx, checkpoint); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Just log and skip, no error returned
			log.Info("Checkpoint CR already exists, skipping creation", "name", checkpoint.Name)
			return nil
		}
		log.Error(err, "Failed to create Checkpoint CR", "name", checkpoint.Name)
		return err
	}

	log.Info("Created Checkpoint CR", "name", checkpoint.Name)
	return nil
}
