/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nephio-project/nephio/controllers/pkg/resource"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	corev1 "k8s.io/api/core/v1"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// ClusterPolicyReconciler reconciles a ClusterPolicy object
type ClusterPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=clusterpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=clusterpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=transition.dcnlab.ssu.ac.kr,resources=clusterpolicies/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ClusterPolicy object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *ClusterPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// println("Reconciling ClusterPolicy:")
	// fmt.Println("Request:", req)
	log := logf.FromContext(ctx)
	log.Info("Reconciling ClusterPolicy")
	// List all Cluster resources
	clusterList := &capiv1beta1.ClusterList{}
	if err := r.Client.List(ctx, clusterList); err != nil {
		return ctrl.Result{}, err
	}
	// numClusters := len(clusterList.Items)
	// println("Number of clusters:", numClusters)

	clusterPolicy := &transitionv1.ClusterPolicy{}
	if err := r.Get(ctx, req.NamespacedName, clusterPolicy); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "ClusterPolicy resource not found.")
			return ctrl.Result{RequeueAfter: 60 * time.Second}, err
		}
		log.Error(err, "Failed to get ClusterPolicy resource")
		return ctrl.Result{RequeueAfter: 60 * time.Second}, err

	}

	//get the clusters
	for _, workload_cluster := range clusterList.Items {
		// log.Info("Cluster found", "name", cluster.Name, "namespace", cluster.Namespace)
		// log.Info("Cluster details", "spec", cluster.Spec, "status", cluster.Status)
		// You can add logic here to process each cluster as needed
		if clusterPolicy.Spec.ClusterSelector.Name == workload_cluster.Name {
			// log.Info("Cluster matches ClusterPolicy selector", "cluster", cluster.Name)
			// Perform actions based on the matching cluster
			// For example, you can update the ClusterPolicy status or perform other operations
			r.performWorkloadClusterPolicyActions(ctx, clusterPolicy, &workload_cluster, req)
			// log.Info("Performed actions for matching cluster", "cluster", cluster.Name)
		} else {
			log.Info("Cluster does not match ClusterPolicy selector", "cluster", workload_cluster.Name)

		}
	}

	// You can log or use numClusters as needed
	// Example: log the number of clusters
	// logf.FromContext(ctx).Info("Number of clusters", "count", numClusters)

	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

func (r *ClusterPolicyReconciler) performWorkloadClusterPolicyActions(ctx context.Context, clusterPolicy *transitionv1.ClusterPolicy, cluster *capiv1beta1.Cluster, req ctrl.Request) {
	//get cluster pods and machines
	log := logf.FromContext(ctx)
	log.Info("Performing actions for ClusterPolicy", "policy", clusterPolicy.Name, "cluster", cluster.Name)
	// Here you can implement the logic to perform actions based on the ClusterPolicy and Cluster
	// For example, you can update the ClusterPolicy status or perform other operations
	capiCluster, err := capictrl.GetCapiClusterFromName(ctx, cluster.Name, cluster.Namespace, r.Client)
	if err != nil {
		log.Error(err, "Failed to get CAPI cluster")
		return
	}

	//get cluster status
	if cluster.Status.Phase != "Provisioned" {
		log.Info("Cluster is not ready or not provisioned yet to apply cluster policy", "cluster", cluster.Name)
		return
	}

	// list cluster machines
	machineList := &capiv1beta1.MachineList{}
	if err := r.Client.List(ctx, machineList, client.InNamespace(cluster.Namespace), client.MatchingLabels{"cluster.x-k8s.io/cluster-name": cluster.Name}); err != nil {
		log.Error(err, "Failed to list machines for cluster", "cluster", cluster.Name)
		return
	}

	//get all machine statuses
	for _, machine := range machineList.Items {
		log.Info("Machine found", "name", machine.Name, "status", machine.Status.Phase)
		//check
		if machine.Status.Phase != "Running" {
			r.handleWorkloadClusterMachine(ctx, clusterPolicy, capiCluster, &machine, req)
		}

	}

	r.handleNodesInWorkloadCluster(ctx, capiCluster, cluster)

	r.handlePodsInWorkloadCluster(ctx, capiCluster, cluster)

}

func (r *ClusterPolicyReconciler) handleNodesInWorkloadCluster(ctx context.Context, capiCluster *capictrl.Capi, cluster *capiv1beta1.Cluster) {
	log := logf.FromContext(ctx)
	// get all pods in the cluster
	clusterClient, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return
	}
	if !ready {
		log.Info("Cluster is not ready", "cluster", capiCluster.GetClusterName())
		return
	}

	nodeList := &corev1.NodeList{}
	if err := clusterClient.List(ctx, nodeList); err != nil {
		log.Error(err, "Failed to list pods for cluster", "cluster", cluster.Name)
		return
	}
	log.Info("Found nodes in cluster", "count", len(nodeList.Items))
	//list all pods in the cluster
	for _, node := range nodeList.Items {
		log.Info("Node found", "name", node.Name, "status", node.Status.Phase, "addresses", node.Status.Addresses)
		//get node type, control-plane or worker

	}
}

func (r *ClusterPolicyReconciler) handlePodsInWorkloadCluster(ctx context.Context, capiCluster *capictrl.Capi, cluster *capiv1beta1.Cluster) {
	log := logf.FromContext(ctx)
	// get all pods in the cluster
	clusterClient, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return
	}
	if !ready {
		log.Info("Cluster is not ready", "cluster", capiCluster.GetClusterName())
		return
	}

	podList := &corev1.PodList{}
	if err := clusterClient.List(ctx, podList); err != nil {
		log.Error(err, "Failed to list pods for cluster", "cluster", cluster.Name)
		return
	}
	log.Info("Found pods in cluster", "count", len(podList.Items))
	//list all pods in the cluster
	for _, pod := range podList.Items {
		log.Info("Pod found", "name", pod.Name, "status", pod.Status.Phase, "node", pod.Spec.NodeName)

	}
}

// this metthod is called when a machine is not running
// it should recover the machine by checking the cluster status and applying the cluster policy
func (r *ClusterPolicyReconciler) handleWorkloadClusterMachine(ctx context.Context, clusterPolicy *transitionv1.ClusterPolicy, capiCluster *capictrl.Capi, machine *capiv1beta1.Machine, req ctrl.Request) {
	log := logf.FromContext(ctx)
	log.Info("Handling machine in workload cluster", "machine", machine.Name, "status", machine.Status.Phase)

	clusterClient, ready, err := capiCluster.GetClusterClient(ctx)
	if err != nil {
		log.Error(err, "Failed to get workload cluster client", "cluster", capiCluster.GetClusterName())
		return
	}
	if !ready {
		log.Info("Cluster is not ready", "cluster", capiCluster.GetClusterName())
		return
	}

	node := &corev1.Node{}
	if err := clusterClient.Get(ctx, types.NamespacedName{Name: machine.Name}, node); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "Node not found", "node", machine.Name)
			node = nil // Explicitly set node to nil if not found
		} else {
			log.Error(err, "Failed to get node", "node", machine.Name)
			return
		}
	}

	// Determine if it's a control plane machine
	if machine.Labels["cluster.x-k8s.io/cluster-name"] == clusterPolicy.Spec.ClusterSelector.Name {
		log.Info("Control plane machine found", "machine", machine.Name)

	} else {
		log.Info("Worker machine found", "machine", machine.Name)

		if machine.Status.Phase == "Failed" || machine.Status.Phase == "Unknown" || (node == nil || (node.Status.Phase != corev1.NodeRunning)) {
			log.Info("Machine is not running, applying cluster policy", "machine", machine.Name)
			//get all pods on the machine or node
			podList := &corev1.PodList{}
			if err := clusterClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": machine.Name}); err != nil {
				log.Error(err, "Failed to list pods for machine", "machine", capiCluster.GetClusterName())
				return
			}
			// log.Info("Found pods on machine", "machine", machine.Name, "count", len(podList.Items))

			// Iterate through the pods and take action based on the cluster policy
			if clusterPolicy.Spec.SelectMode == transitionv1.SelectSpecific {
				for _, pod := range podList.Items {

					//check pod if it has a specific label or annotation
					for _, transitionPackage := range clusterPolicy.Spec.PackageSelectors {
						if _, ok := pod.Labels["transition.dcnlab.ssu.ac.kr/cluster-policy"]; ok &&
							pod.Labels["transition.dcnlab.ssu.ac.kr/package-name"] == transitionPackage.Name {
							log.Info("Pod part of transition package label, applying transition policy", "pod", pod.Name, "package", transitionPackage.Name)
							r.TransitionWorkloads(ctx, clusterClient, &pod, transitionPackage, clusterPolicy, req)
						}
					}
				}
			} else if clusterPolicy.Spec.SelectMode == transitionv1.SelectAll {

			} else {
				log.Info("No selector label or annotation specified in cluster policy, applying to all pods")
			}

		} else {
			log.Info("Machine is running", "machine", machine.Name)

		}
	}
}

func (r *ClusterPolicyReconciler) TransitionWorkloads(ctx context.Context, clusterClient resource.APIPatchingApplicator, pod *corev1.Pod, transitionPackage transitionv1.PackageSelector, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request) {

	log := logf.FromContext(ctx)
	log.Info("Transitioning workload", "pod", pod.Name, "package", transitionPackage.Name)
	//create argocd application resource

}

func (r *ClusterPolicyReconciler) mapClusterToClusterPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)
	log.Info("Mapping Cluster to ClusterPolicy", "cluster", obj.GetName())

	// Assuming the ClusterPolicy is named after the Cluster
	// clusterName := obj.GetName()
	// policyName := fmt.Sprintf("%s-policy", clusterName)

	// Create a request for the corresponding ClusterPolicy
	// return []reconcile.Request{
	// 	{NamespacedName: types.NamespacedName{Name: policyName}},
	// }
	return []reconcile.Request{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return builder.ControllerManagedBy(mgr).
		For(&transitionv1.ClusterPolicy{}).
		Watches(
			&capiv1beta1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.mapClusterToClusterPolicy),
		).
		Complete(r)
}
