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

	"code.gitea.io/sdk/gitea"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	"github.com/nephio-project/nephio/controllers/pkg/resource"
	transitionv1 "github.com/vitu1234/transition-operator/api/v1"
	capictrl "github.com/vitu1234/transition-operator/reconcilers/capi"
	giteaclient "github.com/vitu1234/transition-operator/reconcilers/gitaclient"
	"github.com/vitu1234/transition-operator/reconcilers/helpers"
	giteahelpers "github.com/vitu1234/transition-operator/reconcilers/helpers"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			log.Info("Cluster does not match ClusterPolicy selector - skipping", "cluster", workload_cluster.Name)

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

	r.handleNodesInWorkloadCluster(ctx, clusterPolicy, capiCluster, cluster, req)

	// r.handlePodsInWorkloadCluster(ctx, capiCluster, cluster)
}

// Node failure
func (r *ClusterPolicyReconciler) handleNodesInWorkloadCluster(ctx context.Context, clusterPolicy *transitionv1.ClusterPolicy, capiCluster *capictrl.Capi, cluster *capiv1beta1.Cluster, req ctrl.Request) {
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
		// log.Info("Node found", "name", node.Name, "status", node.Status, "addresses", node.Status.Addresses)
		//get node type, control-plane or worker
		status := helpers.GetNodeStatusSummary(node)
		log.Info("Node status", "name", node.Name, "status", status)
		if status != "Ready" {
			log.Info("Node is not ready, checking conditions", "name", node.Name)
			// transition workloads on this node
			//get all pods on the machine or node
			podList := &corev1.PodList{}
			if err := clusterClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
				log.Error(err, "Failed to list pods for machine", "machine", capiCluster.GetClusterName())
				return
			}
			// log.Info("Found pods on machine", "machine", machine.Name, "count", len(podList.Items))

			// Iterate through the pods and take action based on the cluster policy
			log.Info("Cluster Policy SelectMode", "Mode", string(clusterPolicy.Spec.SelectMode), "node", node.Name, "status", status)
			r.HandlePodsOnNodeForPolicy(ctx, clusterClient, node, podList, clusterPolicy, req, log)

		}

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

	//testing git client
	r.TransitionSelectedWorkloads(ctx, clusterClient, &podList.Items[0], transitionv1.PackageSelector{Name: "test-package"}, &transitionv1.ClusterPolicy{}, ctrl.Request{})

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
		hasBadConditions := false

		if node != nil {
			for _, cond := range node.Status.Conditions {
				switch cond.Type {
				case corev1.NodeReady:
					if cond.Status != corev1.ConditionTrue {
						hasBadConditions = true
					}
				case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure, corev1.NodeNetworkUnavailable:
					if cond.Status == corev1.ConditionTrue {
						hasBadConditions = true
					}
				}
			}
		}

		status := helpers.GetNodeStatusSummary(*node)

		isMachineFailed := machine.Status.Phase == "Failed" || machine.Status.Phase == "Unknown"
		// isNodeNotRunning := node == nil || node.Status.Phase != corev1.NodeRunning

		if isMachineFailed || status != "Ready" || hasBadConditions {
			log.Info("Machine is not running, applying cluster policy", "machine", machine.Name)

			//get all pods on the machine or node
			podList := &corev1.PodList{}
			if err := clusterClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": machine.Name}); err != nil {
				log.Error(err, "Failed to list pods for machine", "machine", capiCluster.GetClusterName())
				return
			}
			// log.Info("Found pods on machine", "machine", machine.Name, "count", len(podList.Items))

			r.HandlePodsOnNodeForPolicy(ctx, clusterClient, *node, podList, clusterPolicy, req, log)

		} else {
			log.Info("Skip, Machine is running", "machine", machine.Name)

		}
	}
}

func (r *ClusterPolicyReconciler) TransitionAllWorkloads(ctx context.Context, clusterClient resource.APIPatchingApplicator, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request) {
	log := logf.FromContext(ctx)
	log.Info("Transitioning all workloads", clusterPolicy.Name)
	//transition all workloads in the cluster

}

func (r *ClusterPolicyReconciler) TransitionSelectedWorkloads(ctx context.Context, clusterClient resource.APIPatchingApplicator, pod *corev1.Pod, transitionPackage transitionv1.PackageSelector, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request) {
	log := logf.FromContext(ctx)
	log.Info("Transitioning Selected workload", "pod", pod.Name, "package", transitionPackage.Name)

	apiClient := resource.NewAPIPatchingApplicator(r.Client)
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
	log.Info("Authenticated with Gitea", "username", user.UserName)

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

	giteahelpers.LogRepositories(log, repos)
	log.Info("Source repository", "cluster", clusterPolicy.Spec.ClusterSelector.Name, "repo", sourceRepo)

	targetRepoName, found := giteahelpers.DetermineTargetRepo(clusterPolicy, log)
	if !found {
		log.Info("No suitable target repository found; canceling transition")
		return
	}

	switch transitionPackage.PackageType {
	case transitionv1.PackageTypeStateful:
		log.Info("Handling stateful package transition", "package", transitionPackage.Name)

	case transitionv1.PackageTypeStateless:
		log.Info("Handling stateless package transition", "package", transitionPackage.Name)
		err, _ := giteahelpers.CreateAndPushArgoApp(ctx, giteaClient.Get(), user.UserName, drRepo.Name, targetRepoName, clusterPolicy, transitionPackage, log)
		if err != nil {
			log.Error(err, "Failed to push ArgoCD app manifest")
			//add status that it failed to transition the package
			clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
				PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
				LastTransitionTime:         metav1.Now(),
				PackageTransitionCondition: transitionv1.PackageTransitionConditionFailed,
				PackageTransitionMessage:   err.Error(),
			})

			if err := r.Status().Update(ctx, clusterPolicy); err != nil {
				log.Error(err, "Failed to update ClusterPolicy status after transition failure")
				return
			}
			return
		}
		clusterPolicy.Status.TransitionedPackages = append(clusterPolicy.Status.TransitionedPackages, transitionv1.TransitionedPackages{
			PackageSelectors:           []transitionv1.PackageSelector{transitionPackage},
			LastTransitionTime:         metav1.Now(),
			PackageTransitionCondition: transitionv1.PackageTransitionConditionCompleted,
			PackageTransitionMessage:   "Transitioned stateless package successfully",
		})

		if err := r.Status().Update(ctx, clusterPolicy); err != nil {
			log.Error(err, "Failed to update ClusterPolicy status after successful transition")
			return
		}
		log.Info("Successfully transitioned stateless package", "package", transitionPackage.Name, "clusterPolicy", clusterPolicy.Name)
		return

	default:
		log.Info("Unknown package type; skipping transition", "package", transitionPackage.Name)
	}

}

// HandlePodsOnNodeForPolicy processes pods on a specific node based on the cluster policy.
// It checks the owning ReplicaSet and Deployment of each pod and applies the transition policy if applicable.
// It handles both specific and all pod selection modes as defined in the cluster policy.
// If the cluster policy specifies a specific selection mode, it will only apply the transition to pods that match the specified criteria.
// If the cluster policy specifies an all selection mode, it will apply the transition to all pods on the node.
// If no selection mode is specified, it will apply the transition to all pods on the node.
// This function is called when a node is not ready or has bad conditions, and it processes the pods on that node accordingly.
func (r *ClusterPolicyReconciler) HandlePodsOnNodeForPolicy(ctx context.Context, clusterClient resource.APIPatchingApplicator, node corev1.Node, podList *corev1.PodList, clusterPolicy *transitionv1.ClusterPolicy, req ctrl.Request, log logr.Logger) {
	// Iterate through the pods and take action based on the cluster policy
	if clusterPolicy.Spec.SelectMode == transitionv1.SelectSpecific {
		log.Info("Applying transition policy to specific pods on node", "node", node.Name)
		for _, pod := range podList.Items {
			// Step 1: Get the owning ReplicaSet of the Pod
			for _, ownerRef := range pod.OwnerReferences {
				if ownerRef.Kind == "ReplicaSet" && ownerRef.Controller != nil && *ownerRef.Controller {
					replicaSet := &appsv1.ReplicaSet{}
					err := clusterClient.Get(ctx, types.NamespacedName{
						Name:      ownerRef.Name,
						Namespace: pod.Namespace,
					}, replicaSet)
					if err != nil {
						log.Error(err, "Failed to get ReplicaSet for pod", "pod", pod.Name)
						continue
					}

					// Step 2: Get the Deployment that owns the ReplicaSet
					for _, rsOwnerRef := range replicaSet.OwnerReferences {
						if rsOwnerRef.Kind == "Deployment" && rsOwnerRef.Controller != nil && *rsOwnerRef.Controller {
							deployment := &appsv1.Deployment{}
							err := clusterClient.Get(ctx, types.NamespacedName{
								Name:      rsOwnerRef.Name,
								Namespace: replicaSet.Namespace,
							}, deployment)
							if err != nil {
								log.Error(err, "Failed to get Deployment for ReplicaSet", "replicaSet", replicaSet.Name)
								continue
							}

							// log.Info("Found Deployment for Pod", "pod", pod.Name, "deployment", deployment.Name)

							// Step 3: Check annotation on Deployment
							for _, transitionPackage := range clusterPolicy.Spec.PackageSelectors {

								if deployment.Annotations["transition.dcnlab.ssu.ac.kr/cluster-policy"] == "true" &&
									deployment.Annotations["transition.dcnlab.ssu.ac.kr/packageName"] == transitionPackage.Name {
									//skip all packages that are already transitioned
									if helpers.IsPackageTransitioned(clusterPolicy, transitionPackage) {
										log.Info("Package already transitioned, skip and continue", "package", transitionPackage.Name, "pod", pod.Name)
										continue
									}

									log.Info("Pod's parent Deployment matches transition policy", "deployment", deployment.Name, "pod", pod.Name)
									r.TransitionSelectedWorkloads(ctx, clusterClient, &pod, transitionPackage, clusterPolicy, req)
								} else {
									log.Info("Pod's parent Deployment does not match transition policy", "deployment", "Node", node.Name, deployment.Name, "pod", pod.Name)
								}
							}
						}
					}
				}
			}
		}

	} else if clusterPolicy.Spec.SelectMode == transitionv1.SelectAll {
		r.TransitionAllWorkloads(ctx, clusterClient, clusterPolicy, req)
	} else {
		log.Info("No selector label or annotation specified in cluster policy, applying to all pods")
	}
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
