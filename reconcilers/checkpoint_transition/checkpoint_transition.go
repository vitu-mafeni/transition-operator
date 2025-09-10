package checkpointtransition

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/types"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capi "github.com/vitu1234/transition-operator/reconcilers/capi"
	"github.com/vitu1234/transition-operator/reconcilers/helpers"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//performs checkpoint transition actions on the workload cluster

func PerformWorkloadClusterCheckpointAction(ctx context.Context, mgmtClient client.Client, pkg transitionv1.PackageSelector, workload_cluster *capiv1beta1.Cluster) error {
	log := logf.FromContext(ctx)
	//get workload cluster client
	_, workloadClusterClient, _, err := capi.GetWorkloadClusterClient(ctx, mgmtClient, workload_cluster.Name)
	if err != nil {
		return err
	}
	if workloadClusterClient == nil {
		// Cluster not ready or being deleted; just requeue
		log.Info("Cluster client not available yet in ready state", "cluster", workload_cluster.Name)
		return fmt.Errorf("workload cluster %q client not available yet", workload_cluster.Name)
	}
	//get all pods and their parent resources in all namespaces
	podList := &corev1.PodList{}
	if err := workloadClusterClient.List(ctx, podList, &client.ListOptions{}); err != nil {
		log.Error(err, "Failed to list pods in workload cluster", "cluster", workload_cluster.Name)
		return err
	}

	processed := make(map[string]struct{}) // To avoid duplicate transitions

	//iterate through all pods and check if they match the package selector
	for _, pod := range podList.Items {
		//get parent resource of the pod
		workloadKind, workloadName, ok := helpers.GetWorkloadOwnerControllerInfo(pod)
		if !ok {
			log.Info("No controller owner reference found for pod", "pod", pod.Name)
			continue
		}

		var annotations map[string]string
		var namespace = pod.Namespace
		var workloadID = fmt.Sprintf("%s/%s/%s", namespace, workloadName, workloadKind)

		switch workloadKind {
		case "ReplicaSet":
			replicaSet := &appsv1.ReplicaSet{}
			if err := workloadClusterClient.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: namespace}, replicaSet); err != nil {
				log.Error(err, "Failed to get ReplicaSet", "pod", pod.Name)
				continue
			}

			// Get Deployment owner
			deployName, found := helpers.GetWorkloadControllerOwnerName(replicaSet.OwnerReferences, "Deployment")
			if !found {
				log.Info("ReplicaSet has no Deployment owner", "replicaSet", replicaSet.Name)
				continue
			}

			deployment := &appsv1.Deployment{}
			if err := workloadClusterClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: namespace}, deployment); err != nil {
				log.Error(err, "Failed to get Deployment", "deployment", deployName)
				continue
			}
			annotations = deployment.Annotations
			workloadID = fmt.Sprintf("%s/%s/Deployment", namespace, deployName)

		case "DaemonSet":
			daemonSet := &appsv1.DaemonSet{}
			if err := workloadClusterClient.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: namespace}, daemonSet); err != nil {
				log.Error(err, "Failed to get DaemonSet", "daemonSet", workloadName)
				continue
			}
			annotations = daemonSet.Annotations

		case "StatefulSet":
			statefulSet := &appsv1.StatefulSet{}
			if err := workloadClusterClient.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: namespace}, statefulSet); err != nil {
				log.Error(err, "Failed to get StatefulSet", "statefulSet", workloadName)
				continue
			}
			annotations = statefulSet.Annotations
		case "Deployment":
			deployment := &appsv1.Deployment{}
			if err := workloadClusterClient.Get(ctx, types.NamespacedName{Name: workloadName, Namespace: namespace}, deployment); err != nil {
				log.Error(err, "Failed to get Deployment", "deployment", workloadName)
				continue
			}
			annotations = deployment.Annotations

		default:
			log.Info("Unsupported controller kind; skipping", "kind", workloadKind, "pod", pod.Name)
			continue
		}

		if annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
			annotations["transition.dcnlab.ssu.ac.kr/packageName"] == pkg.Name {

			// Deduplicate by workload and package
			key := fmt.Sprintf("%s/%s", workloadID, pkg.Name)
			if _, seen := processed[key]; seen {
				continue
			}
			processed[key] = struct{}{}

			log.Info("Matched workload for creating checkpoint resoource", "workload", workloadID, "package", pkg.Name, "pod", pod.Name)

			//create checkpoint resource
		}
	}

	return nil
}
