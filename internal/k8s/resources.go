package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	databasev1 "postgres-operator/api/v1"
)

// CreateOrUpdate creates or updates a Kubernetes resource
func CreateOrUpdate(ctx context.Context, c client.Client, obj client.Object) error {
	key := client.ObjectKeyFromObject(obj)
	current := obj.DeepCopyObject().(client.Object)

	err := c.Get(ctx, key, current)
	if err != nil {
		if errors.IsNotFound(err) {
			return c.Create(ctx, obj)
		}
		return err
	}

	// Set resource version for update
	obj.SetResourceVersion(current.GetResourceVersion())
	return c.Update(ctx, obj)
}

// CreateBackupPVC creates a PVC for backup storage
func CreateBackupPVC(ctx context.Context, k8sClient client.Client, cluster *databasev1.PostgresCluster, size string) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-backup-storage", cluster.Name),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "postgres-backup",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/managed-by": "postgres-operator",
				"database.example.com/cluster": cluster.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
		},
	}

	return k8sClient.Create(ctx, pvc)
}
