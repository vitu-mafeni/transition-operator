package helpers

import (
	"context"
	"os"
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

// helper function to get parent workload annotations
func GetParentAnnotations(
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
		deployName, found := GetWorkloadControllerOwnerName(rs.OwnerReferences, "Deployment")
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
