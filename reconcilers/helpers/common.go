package helpers

import (
	"context"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "k8s.io/api/apps/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func GetWorkloadOwnerControllerInfo(pod corev1.Pod) (kind string, name string, ok bool) {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind, ref.Name, true
		}
	}
	return "", "", false
}

func GetWorkloadControllerOwnerName(refs []metav1.OwnerReference, kind string) (string, bool) {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Controller != nil && *ref.Controller {
			return ref.Name, true
		}
	}
	return "", false
}

// Helper: finds the name of a specific-kind owner, ignoring nil Controller fields.
func GetOwnerName(refs []metav1.OwnerReference, kind string) (string, string, bool) {
	for _, ref := range refs {
		log := logf.FromContext(context.TODO())
		// log.V(1).Info("Checking owner reference", "ref", ref, "kind", kind)
		if strings.EqualFold(ref.Kind, kind) &&
			(ref.Controller == nil || *ref.Controller) {
			// log.V(1).Info("Found matching owner reference Returning success", "name", ref.Name)
			// log.V(1).Info("Found matching owner reference Returning success", "kind", ref.Kind)
			return ref.Name, ref.Kind, true
		}
		log.V(1).Info("Owner reference did not match error", "ref", ref)
	}
	return "", "", false
}

func GetEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func GetEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultVal
}

// helper function to get parent workload object metadata
func GetParentAnnotations(
	ctx context.Context,
	c client.Client,
	kind, name, namespace string,
) (*metav1.ObjectMeta, string, error) {

	log := logf.FromContext(ctx)

	switch kind {

	case "ReplicaSet":
		rs := &appsv1.ReplicaSet{}
		if err := c.Get(ctx,
			types.NamespacedName{Name: name, Namespace: namespace}, rs); err != nil {
			log.Error(err, "Failed to get ReplicaSet", "name", name, "namespace", namespace)
			return nil, "", err
		}

		// Log what we really got back for easier troubleshooting

		deployName, _, found := GetOwnerName(rs.OwnerReferences, "Deployment")
		if !found {
			// log.Info("ReplicaSet OwnerReferences", "refs", rs.OwnerReferences)
			// No Deployment owner; just return RS metadata
			return &rs.ObjectMeta, "ReplicaSet", nil
		}

		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx,
			types.NamespacedName{Name: deployName, Namespace: rs.Namespace}, deploy); err != nil {
			log.Error(err, "Failed to get Deployment",
				"name", deployName, "namespace", rs.Namespace)
			// Fallback to RS metadata if the Deployment really isn’t there
			return &rs.ObjectMeta, "ReplicaSet", nil
		}
		return &deploy.ObjectMeta, "Deployment", nil

	case "Deployment":
		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy); err != nil {
			log.Error(err, "Failed to get Deployment", "name", name)
			return nil, "", err
		}
		return &deploy.ObjectMeta, "Deployment", nil

	case "DaemonSet":
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ds); err != nil {
			log.Error(err, "Failed to get DaemonSet", "name", name)
			return nil, "", err
		}
		return &ds.ObjectMeta, "DaemonSet", nil

	case "StatefulSet":
		ss := &appsv1.StatefulSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ss); err != nil {
			log.Error(err, "Failed to get StatefulSet", "name", name)
			return nil, "", err
		}
		return &ss.ObjectMeta, "StatefulSet", nil
	}

	return nil, "", nil // unsupported kind
}
