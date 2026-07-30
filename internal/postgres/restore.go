package postgres

import (
	"fmt"

	databasev1 "postgres-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	batchv1 "k8s.io/api/batch/v1"
)

func BuildRestoreCommand(
	backup *databasev1.PostgresBackup,
	options databasev1.RestoreOptions,
	dbConfig databasev1.DatabaseSpec,
	storage databasev1.StorageSpec,
) (string, error) {
	if backup == nil {
		return "", fmt.Errorf("backup cannot be nil")
	}

	restoreType := NormalizeBackupType(backup.Spec.Type)
	switch restoreType {
	case "logical":
		dbName := dbConfig.Name
		if dbName == "" {
			dbName = "postgres"
		}

		cmd := "pg_restore"
		cmd += " --host=${POSTGRES_HOST:-localhost}"
		cmd += " --port=${POSTGRES_PORT:-5432}"
		cmd += " --username=${POSTGRES_USER:-postgres}"
		cmd += fmt.Sprintf(" --dbname=%s", dbName)
		cmd += " --format=c /backup/data.dump"

		if options.DropExisting {
			cmd += " --clean --if-exists"
		}
		if options.DataOnly {
			cmd += " --data-only"
		}
		if options.SchemaOnly {
			cmd += " --schema-only"
		}
		if options.ParallelRestores > 1 {
			cmd += fmt.Sprintf(" --jobs=%d", options.ParallelRestores)
		}

		return cmd, nil

	case "physical":
		cmd := "mkdir -p /var/lib/postgresql/data"
		cmd += " && find /var/lib/postgresql/data -mindepth 1 -exec rm -rf {} + 2>/dev/null || true"
		cmd += " && cp -a /backup/. /var/lib/postgresql/data/"
		cmd += " && chown -R 999:999 /var/lib/postgresql/data/"
		return cmd, nil

	default:
		return "", fmt.Errorf("unsupported backup type: %s", backup.Spec.Type)
	}
}

func AddBackupVolumeToJob(job *batchv1.Job, backup *databasev1.PostgresBackup) error {
	if job == nil {
		return fmt.Errorf("job cannot be nil")
	}
	if backup == nil {
		return fmt.Errorf("backup cannot be nil")
	}

	switch backup.Spec.Storage.Type {
	case "local":
		pvcName, ok := backup.Spec.Storage.Config["pvcName"]
		if !ok || pvcName == "" {
			return fmt.Errorf("pvcName not specified in local backup storage config")
		}

		volume := corev1.Volume{
			Name: "backup",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
				},
			},
		}

		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, volume)

		container := &job.Spec.Template.Spec.Containers[0]
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "backup",
			MountPath: "/backup",
			ReadOnly:  true,
		})

	case "s3":
		initContainer := corev1.Container{
			Name:    "download-backup",
			Image:   "amazon/aws-cli:latest",
			Command: []string{"sh", "-c"},
			Args: []string{
				fmt.Sprintf("aws s3 cp s3://%s/%s /backup --recursive",
					backup.Spec.Storage.Config["bucket"],
					backup.Spec.Storage.Config["path"]),
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "backup",
					MountPath: "/backup",
				},
			},
			Env: []corev1.EnvVar{
				{
					Name: "AWS_ACCESS_KEY_ID",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: backup.Spec.Storage.Config["secretName"],
							},
							Key: "accessKey",
						},
					},
				},
				{
					Name: "AWS_SECRET_ACCESS_KEY",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: backup.Spec.Storage.Config["secretName"],
							},
							Key: "secretKey",
						},
					},
				},
				{
					Name:  "AWS_DEFAULT_REGION",
					Value: backup.Spec.Storage.Config["region"],
				},
			},
		}

		volume := corev1.Volume{
			Name: "backup",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}

		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, volume)
		job.Spec.Template.Spec.InitContainers = append(job.Spec.Template.Spec.InitContainers, initContainer)

		container := &job.Spec.Template.Spec.Containers[0]
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      "backup",
			MountPath: "/backup",
		})

	default:
		return fmt.Errorf("unsupported backup storage type: %s", backup.Spec.Storage.Type)
	}

	return nil
}
