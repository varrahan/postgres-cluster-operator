package controllers

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "postgres-operator/api/v1"
	"postgres-operator/internal/k8s"
	"postgres-operator/internal/postgres"
)

type PostgresBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=database.example.com,resources=postgresbackups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.example.com,resources=postgresbackups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.example.com,resources=postgresbackups/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *PostgresBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	backup := &databasev1.PostgresBackup{}
	err := r.Get(ctx, req.NamespacedName, backup)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PostgresBackup resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PostgresBackup")
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(backup, "database.example.com/postgres-backup") {
		controllerutil.AddFinalizer(backup, "database.example.com/postgres-backup")
		return ctrl.Result{}, r.Update(ctx, backup)
	}

	if backup.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, backup)
	}

	cluster := &databasev1.PostgresCluster{}
	clusterKey := types.NamespacedName{
		Name:      backup.Spec.ClusterRef.Name,
		Namespace: backup.Spec.ClusterRef.Namespace,
	}
	if backup.Spec.ClusterRef.Namespace == "" {
		clusterKey.Namespace = backup.Namespace
	}

	err = r.Get(ctx, clusterKey, cluster)
	if err != nil {
		logger.Error(err, "Failed to get referenced PostgresCluster")
		backup.Status.Phase = "Failed"
		backup.Status.Message = fmt.Sprintf("Failed to get cluster %s: %v", backup.Spec.ClusterRef.Name, err)
		return ctrl.Result{}, r.Status().Update(ctx, backup)
	}

	r.setDefaults(backup)

	if err := r.reconcileBackup(ctx, backup, cluster); err != nil {
		logger.Error(err, "Failed to reconcile PostgreSQL backup")
		backup.Status.Phase = "Failed"
		backup.Status.Message = fmt.Sprintf("Failed to reconcile backup: %v", err)
		if err := r.Status().Update(ctx, backup); err != nil {
			logger.Error(err, "Failed to update backup status")
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
}

func (r *PostgresBackupReconciler) setDefaults(backup *databasev1.PostgresBackup) {
	if backup.Spec.Type == "" {
		backup.Spec.Type = "full"
	}
	if backup.Spec.Options.Compression > 9 || backup.Spec.Options.Compression < 0 {
		backup.Spec.Options.Compression = 1
	}
	if backup.Spec.Options.ParallelJobs == 0 {
		backup.Spec.Options.ParallelJobs = 1
	}
	if backup.Spec.Options.Timeout == "" {
		backup.Spec.Options.Timeout = "1h"
	}
}

func (r *PostgresBackupReconciler) reconcileBackup(ctx context.Context, backup *databasev1.PostgresBackup, cluster *databasev1.PostgresCluster) error {
	targetInstance, err := r.getTargetInstance(backup, cluster)
	if err != nil {
		return fmt.Errorf("failed to determine target instance: %w", err)
	}

	backupManager := postgres.NewBackupManager(r.Client)
	job, err := backupManager.CreateBackupJob(backup, cluster, targetInstance)
	if err != nil {
		return fmt.Errorf("failed to create backup job: %w", err)
	}

	if err := controllerutil.SetControllerReference(backup, job, r.Scheme); err != nil {
		return err
	}

	if err := k8s.CreateOrUpdate(ctx, r.Client, job); err != nil {
		return fmt.Errorf("failed to create backup job: %w", err)
	}

	return r.updateBackupStatus(ctx, backup, job, cluster, targetInstance)
}

func (r *PostgresBackupReconciler) getTargetInstance(backup *databasev1.PostgresBackup, cluster *databasev1.PostgresCluster) (*databasev1.PostgresInstanceSpec, error) {
    if len(cluster.Spec.Instances) == 0 {
        return nil, nil // Signals to use cluster-wide defaults
    }

    if backup.Spec.Instances == nil || len(backup.Spec.Instances.Names) == 0 {
        return r.findInstanceByName(cluster, "primary")
    }

    for _, instanceName := range backup.Spec.Instances.Names {
        instance, err := r.findInstanceByName(cluster, instanceName)
        if err == nil {
            return instance, nil
        }
        log.FromContext(context.Background()).Error(err, "Instance not found, trying next", "instanceName", instanceName)
    }

    return nil, fmt.Errorf("none of the specified instances (%v) were found in the cluster", backup.Spec.Instances.Names)
}

func (r *PostgresBackupReconciler) findInstanceByName(cluster *databasev1.PostgresCluster, name string) (*databasev1.PostgresInstanceSpec, error) {
    for _, instance := range cluster.Spec.Instances {
        if instance.Name == name {
            return &instance, nil
        }
    }
    return nil, fmt.Errorf("instance %q not found in cluster", name)
}

func (r *PostgresBackupReconciler) updateBackupStatus(
	ctx context.Context,
	backup *databasev1.PostgresBackup,
	job *batchv1.Job,
	cluster *databasev1.PostgresCluster,
	targetInstance *databasev1.PostgresInstanceSpec,
) error {
	currentJob := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      job.Name,
		Namespace: job.Namespace,
	}, currentJob); err != nil {
		return err
	}

	backup.Status.TargetInstance = "primary" // Default
	backup.Status.Database = databasev1.DatabaseStatus{
		Name:   cluster.Spec.Database.Name,
		Source: "cluster", // default to cluster source
		Exists: true,
	}

	if targetInstance != nil {
		backup.Status.TargetInstance = targetInstance.Name
		if targetInstance.Database != nil && targetInstance.Database.Name != "" {
			backup.Status.Database.Name = targetInstance.Database.Name
			backup.Status.Database.Source = "instance"
		}
	}

	if backup.Spec.IncludeInstanceConfig && targetInstance != nil && targetInstance.Config != nil {
		backup.Status.Database.Config = targetInstance.Config
	}

	switch {
	case currentJob.Status.CompletionTime != nil:
		backup.Status.Phase = "Completed"
		backup.Status.Message = "Backup completed successfully"
		backup.Status.CompletionTime = currentJob.Status.CompletionTime
	case currentJob.Status.Failed > 0:
		backup.Status.Phase = "Failed"
		backup.Status.Message = "Backup job failed"
	case currentJob.Status.Active > 0:
		backup.Status.Phase = "Running"
		backup.Status.Message = "Backup job is running"
		if backup.Status.StartTime == nil {
			backup.Status.StartTime = currentJob.Status.StartTime
		}
	default:
		backup.Status.Phase = "Pending"
		backup.Status.Message = "Backup job is pending"
	}

	backup.Status.JobName = currentJob.Name
	backup.Status.Conditions = r.buildBackupConditions(currentJob)


	return r.Status().Update(ctx, backup)
}

func (r *PostgresBackupReconciler) buildBackupConditions(job *batchv1.Job) []metav1.Condition {
	var conditions []metav1.Condition
	
	for _, jobCondition := range job.Status.Conditions {
		condition := metav1.Condition{
			Type:               string(jobCondition.Type),
			Status:             metav1.ConditionStatus(jobCondition.Status),
			LastTransitionTime: jobCondition.LastTransitionTime,
			Reason:             jobCondition.Reason,
			Message:            jobCondition.Message,
		}
		conditions = append(conditions, condition)
	}
	
	return conditions
}

func (r *PostgresBackupReconciler) handleDeletion(ctx context.Context, backup *databasev1.PostgresBackup) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion of PostgresBackup", "backup", backup.Name)

	if backup.Status.JobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Name: backup.Status.JobName, Namespace: backup.Namespace}, job)
		if err == nil {
			logger.Info("Deleting backup job", "job", backup.Status.JobName)
			if err := r.Delete(ctx, job); client.IgnoreNotFound(err) != nil {
				logger.Error(err, "Failed to delete backup job")
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(backup, "database.example.com/postgres-backup")
	return ctrl.Result{}, r.Update(ctx, backup)
}

func (r *PostgresBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.PostgresBackup{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}