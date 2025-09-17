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

// helper function to get parent workload object metadata
func GetParentAnnotations(
	ctx context.Context,
	c client.Client,
	kind, name, namespace string,
) (*metav1.ObjectMeta, error) {

	log := logf.FromContext(ctx)

	switch kind {

	case "ReplicaSet":
		rs := &appsv1.ReplicaSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, rs); err != nil {
			log.Error(err, "Failed to get ReplicaSet", "name", name)
			return nil, err
		}

		deployName, found := GetWorkloadControllerOwnerName(rs.OwnerReferences, "Deployment")
		if !found {
			return &rs.ObjectMeta, nil
		}

		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: deployName, Namespace: namespace}, deploy); err != nil {
			log.Error(err, "Failed to get Deployment", "name", deployName)
			return &rs.ObjectMeta, nil // fallback to RS metadata if deploy not found
		}
		return &deploy.ObjectMeta, nil

	case "Deployment":
		deploy := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy); err != nil {
			log.Error(err, "Failed to get Deployment", "name", name)
			return nil, err
		}
		return &deploy.ObjectMeta, nil

	case "DaemonSet":
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ds); err != nil {
			log.Error(err, "Failed to get DaemonSet", "name", name)
			return nil, err
		}
		return &ds.ObjectMeta, nil

	case "StatefulSet":
		ss := &appsv1.StatefulSet{}
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ss); err != nil {
			log.Error(err, "Failed to get StatefulSet", "name", name)
			return nil, err
		}
		return &ss.ObjectMeta, nil
	}

	return nil, nil // unsupported kind
}
