package postgres

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1 "postgres-operator/api/v1"
)

type BackupManager struct {
	client.Client
}

func NewBackupManager(client client.Client) *BackupManager {
	return &BackupManager{
		Client: client,
	}
}

func NormalizeBackupType(backupType string) string {
	switch strings.ToLower(strings.TrimSpace(backupType)) {
	case "", "full", "physical":
		return "physical"
	case "logical":
		return "logical"
	default:
		return strings.ToLower(strings.TrimSpace(backupType))
	}
}

func (bm *BackupManager) CreateBackupJob(
	backup *databasev1.PostgresBackup,
	cluster *databasev1.PostgresCluster,
	instance *databasev1.PostgresInstanceSpec,
) (*batchv1.Job, error) {
	pvcName, ok := cluster.Spec.Backup.Storage.Config["pvcName"]
	if !ok || pvcName == "" {
		return nil, fmt.Errorf("pvcName not set in cluster backup storage config")
	}

	jobName := backup.Name + "-job"
	if len(jobName) > 63 {
		jobName = jobName[:63]
	}

	image := "postgres:" + cluster.Spec.PostgresVersion

	targetHost := cluster.Status.CurrentPrimary
	if targetHost == "" {
		targetHost = fmt.Sprintf("%s-primary.%s.svc.cluster.local", cluster.Name, cluster.Namespace)
	}
	if instance != nil && instance.Name != "" {
		targetHost = fmt.Sprintf("%s-%s.%s.svc.cluster.local", cluster.Name, instance.Name, cluster.Namespace)
	}

	backupType := NormalizeBackupType(backup.Spec.Type)
	var command []string
	var args []string

	switch backupType {
	case "logical":
		dbName := cluster.Spec.Database.Name
		if instance != nil && instance.Database != nil && instance.Database.Name != "" {
			dbName = instance.Database.Name
		}

		command = []string{"pg_dump"}
		args = []string{
			"-h", targetHost,
			"-U", "postgres",
			"-d", dbName,
			"-f", "/backup/data.dump",
			"-F", "c",
		}
		if backup.Spec.Options.Compression > 0 {
			args = append(args, "-Z", fmt.Sprintf("%d", backup.Spec.Options.Compression))
		}
		if backup.Spec.Options.ParallelJobs > 1 {
			args = append(args, "-j", fmt.Sprintf("%d", backup.Spec.Options.ParallelJobs))
		}

		if instance != nil && instance.Config != nil {
			for param, value := range instance.Config {
				args = append(args, "--"+param, value)
			}
		}

	case "physical":
		command = []string{"pg_basebackup"}
		args = []string{
			"-D", "/backup",
			"-h", targetHost,
			"-U", "postgres",
			"--checkpoint", "fast",
		}
		if backup.Spec.Options.Compression > 0 {
			args = append(args, "--compress="+fmt.Sprintf("%d", backup.Spec.Options.Compression))
		}
		if backup.Spec.Options.ParallelJobs > 1 {
			args = append(args, "--jobs", fmt.Sprintf("%d", backup.Spec.Options.ParallelJobs))
		}

	default:
		return nil, fmt.Errorf("unsupported backup type: %s", backup.Spec.Type)
	}

	env := []corev1.EnvVar{
		{
			Name: "PGPASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cluster.Name + "-credentials",
					},
					Key: "postgres-password",
				},
			},
		},
	}

	if backupType == "logical" && instance != nil && instance.Database != nil && instance.Database.InitScript != "" {
		env = append(env, corev1.EnvVar{
			Name:  "PGINITSCRIPT",
			Value: instance.Database.InitScript,
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: backup.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "postgres-backup",
				"app.kubernetes.io/instance":   backup.Spec.ClusterRef.Name,
				"app.kubernetes.io/managed-by": "postgres-operator",
				"database.example.com/cluster": backup.Spec.ClusterRef.Name,
				"database.example.com/type":    backupType,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":       "postgres-backup",
						"app.kubernetes.io/instance":   backup.Spec.ClusterRef.Name,
						"app.kubernetes.io/managed-by": "postgres-operator",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:         "backup",
							Image:        image,
							Command:      command,
							Args:         args,
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "backup-storage",
									MountPath: "/backup",
								},
							},
							Env: env,
						},
					},
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Volumes: []corev1.Volume{
						{
							Name: "backup-storage",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}

	if instance != nil {
		job.Labels["database.example.com/instance"] = instance.Name
		if instance.Role != "" {
			job.Labels["database.example.com/role"] = instance.Role
		}
	}

	return job, nil
}
func (bm *BackupManager) ListBackups(ctx context.Context, clusterName, namespace string) (*databasev1.PostgresBackupList, error) {
	backupList := &databasev1.PostgresBackupList{}
	
	err := bm.List(ctx, backupList, client.InNamespace(namespace), client.MatchingLabels{
		"database.example.com/cluster": clusterName,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list backups: %w", err)
	}

	return backupList, nil
}

func (bm *BackupManager) DeleteBackup(ctx context.Context, backupName, namespace string) error {
	backup := &databasev1.PostgresBackup{}
	err := bm.Get(ctx, types.NamespacedName{Name: backupName, Namespace: namespace}, backup)
	if err != nil {
		return fmt.Errorf("failed to get backup: %w", err)
	}

	err = bm.Delete(ctx, backup)
	if err != nil {
		return fmt.Errorf("failed to delete backup: %w", err)
	}

	return nil
}
