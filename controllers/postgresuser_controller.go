package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "postgres-operator/api/v1"
	"postgres-operator/internal/postgres"
	"postgres-operator/internal/utils"
)

type PostgresUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=database.example.com,resources=postgresusers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.example.com,resources=postgresusers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.example.com,resources=postgresusers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *PostgresUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("postgresuser", req.NamespacedName)

	user := &databasev1.PostgresUser{}
	if err := r.Get(ctx, req.NamespacedName, user); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PostgresUser resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PostgresUser")
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(user, "database.example.com/postgres-user") {
		controllerutil.AddFinalizer(user, "database.example.com/postgres-user")
		if err := r.Update(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if user.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, user)
	}

	cluster, err := r.getCluster(ctx, user)
	if err != nil {
		if statusErr := r.updateStatus(ctx, user, databasev1.PostgresUserStatus{
			Phase:   "Failed",
			Message: fmt.Sprintf("Failed to get cluster %s: %v", userClusterName(user), err),
		}); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	targetInstances, err := r.getTargetInstances(user, cluster)
	if err != nil {
		if updateErr := r.updateStatus(ctx, user, databasev1.PostgresUserStatus{
			Phase:   "Failed",
			Message: fmt.Sprintf("Failed to get target instances for cluster %s: %v", userClusterName(user), err),
		}); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcilePasswordSecret(ctx, user); err != nil {
		if updateErr := r.updateStatus(ctx, user, databasev1.PostgresUserStatus{
			Phase:   "Failed",
			Message: fmt.Sprintf("Failed to reconcile password secret for cluster %s: %v", userClusterName(user), err),
		}); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, err
	}

	instanceStatuses, reconcileErr := r.reconcileUserOnInstances(ctx, user, cluster, targetInstances)

	status := databasev1.PostgresUserStatus{
		InstanceStatuses:    instanceStatuses,
		LastPasswordChange: user.Status.LastPasswordChange,
	}

	if reconcileErr != nil {
		status.Phase = "Failed"
		status.Message = reconcileErr.Error()
		logger.Error(reconcileErr, "Failed to reconcile user")
	} else {
		status.Phase = "Ready"
		status.Message = "User successfully configured"
	}

	if err := r.updateStatus(ctx, user, status); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update status: %w", err)
	}

	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}

	return ctrl.Result{RequeueAfter: 30 * time.Minute}, nil
}

func (r *PostgresUserReconciler) getCluster(ctx context.Context, user *databasev1.PostgresUser) (*databasev1.PostgresCluster, error) {
	if user.Spec.ClusterRef == nil {
		return nil, fmt.Errorf("clusterRef is required")
	}

	clusterKey := types.NamespacedName{
		Name:      user.Spec.ClusterRef.Name,
		Namespace: user.Spec.ClusterRef.Namespace,
	}
	if clusterKey.Namespace == "" {
		clusterKey.Namespace = user.Namespace
	}

	cluster := &databasev1.PostgresCluster{}
	if err := r.Get(ctx, clusterKey, cluster); err != nil {
		return nil, err
	}
	return cluster, nil
}

func userClusterName(user *databasev1.PostgresUser) string {
	if user == nil || user.Spec.ClusterRef == nil || user.Spec.ClusterRef.Name == "" {
		return "<unknown>"
	}
	return user.Spec.ClusterRef.Name
}

func (r *PostgresUserReconciler) getTargetInstances(user *databasev1.PostgresUser, cluster *databasev1.PostgresCluster) ([]string, error) {
	if user.Spec.InstanceSelector == nil {
		var instances []string
		for _, instance := range cluster.Status.Instances {
			instances = append(instances, instance.Name)
		}
		return instances, nil
	}

	if len(user.Spec.InstanceSelector.Names) > 0 {
		return user.Spec.InstanceSelector.Names, nil
	}

	if user.Spec.InstanceSelector.Role != "" {
		var instances []string
		for _, instance := range cluster.Status.Instances {
			if instance.Role == user.Spec.InstanceSelector.Role {
				instances = append(instances, instance.Name)
			}
		}
		if len(instances) == 0 {
			return nil, fmt.Errorf("no instances found with role %s", user.Spec.InstanceSelector.Role)
		}
		return instances, nil
	}

	if user.Spec.InstanceSelector.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(user.Spec.InstanceSelector.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %w", err)
		}

		var instances []string
		for _, instance := range cluster.Status.Instances {
			if selector.Matches(labels.Set(instance.Labels)) {
				instances = append(instances, instance.Name)
			}
		}
		if len(instances) == 0 {
			return nil, fmt.Errorf("no instances match label selector")
		}
		return instances, nil
	}

	var instances []string
	for _, instance := range cluster.Status.Instances {
		instances = append(instances, instance.Name)
	}
	return instances, nil
}

func (r *PostgresUserReconciler) reconcileUserOnInstances(ctx context.Context, user *databasev1.PostgresUser, cluster *databasev1.PostgresCluster, targetInstances []string) ([]databasev1.UserInstanceStatus, error) {
	logger := log.FromContext(ctx)
	var instanceStatuses []databasev1.UserInstanceStatus
	var reconcileErr error

	password, err := r.getPasswordFromSecret(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("failed to get password: %w", err)
	}

	for _, instanceName := range targetInstances {
		instanceStatus := databasev1.UserInstanceStatus{
			Name:  instanceName,
			Ready: false,
		}

		pgClient, err := postgres.NewClientForInstance(ctx, r.Client, cluster, instanceName)
		if err != nil {
			instanceStatus.Message = fmt.Sprintf("Failed to connect to instance: %v", err)
			instanceStatuses = append(instanceStatuses, instanceStatus)
			reconcileErr = fmt.Errorf("failed on instance %s: %w", instanceName, err)
			continue
		}
		defer pgClient.Close()

		if err := pgClient.CreateOrUpdateUser(ctx, user.Spec.Username, password, user.Spec.Privileges); err != nil {
			instanceStatus.Message = fmt.Sprintf("Failed to create/update user: %v", err)
			instanceStatuses = append(instanceStatuses, instanceStatus)
			reconcileErr = fmt.Errorf("failed on instance %s: %w", instanceName, err)
			continue
		}

		if user.Spec.ConnectionLimit >= 0 {
			if err := pgClient.SetConnectionLimit(ctx, user.Spec.Username, user.Spec.ConnectionLimit); err != nil {
				instanceStatus.Message = fmt.Sprintf("Failed to set connection limit: %v", err)
				instanceStatuses = append(instanceStatuses, instanceStatus)
				reconcileErr = fmt.Errorf("failed on instance %s: %w", instanceName, err)
				continue
			}
		}

		instanceStatus.Ready = true
		instanceStatus.Message = "User successfully configured"
		instanceStatus.LastUpdated = &metav1.Time{Time: time.Now()}
		instanceStatuses = append(instanceStatuses, instanceStatus)
		logger.Info("Successfully configured user on instance", "instance", instanceName)
	}

	return instanceStatuses, reconcileErr
}

func (r *PostgresUserReconciler) reconcilePasswordSecret(ctx context.Context, user *databasev1.PostgresUser) error {
	logger := log.FromContext(ctx)
	
	if user.Spec.Password == nil {
		return fmt.Errorf("password configuration is required")
	}

	if user.Spec.Password.SecretRef != nil {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      user.Spec.Password.SecretRef.Name,
			Namespace: user.Namespace,
		}, secret)
		
		if err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced secret %s not found", user.Spec.Password.SecretRef.Name)
			}
			return fmt.Errorf("failed to get referenced secret: %w", err)
		}
		
		key := "password"
		if user.Spec.Password.SecretRef.Key != "" {
			key = user.Spec.Password.SecretRef.Key
		}
		
		if _, exists := secret.Data[key]; !exists {
			return fmt.Errorf("secret %s missing required key '%s'", user.Spec.Password.SecretRef.Name, key)
		}
		
		return nil
	}

	if !user.Spec.Password.Generate {
		return fmt.Errorf("password must be either provided via secretRef or generated")
	}

	if !r.shouldRotatePassword(user) {
		return nil
	}

	length := 16
	if user.Spec.Password.Length > 0 {
		length = int(user.Spec.Password.Length)
	}

	password, err := utils.GeneratePassword(length)
	if err != nil {
		return fmt.Errorf("failed to generate password: %w", err)
	}

	secretName := fmt.Sprintf("%s-password", user.Name)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: user.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "postgres-user",
				"app.kubernetes.io/instance":   user.Name,
				"app.kubernetes.io/managed-by": "postgres-operator",
			},
		},
		Data: map[string][]byte{
			"password": []byte(password),
		},
	}

	if err := controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return err
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Data["password"] = []byte(password)
		return nil
	})
	
	if err != nil {
		return fmt.Errorf("failed to create/update password secret: %w", err)
	}

	now := metav1.Now()
	user.Status.LastPasswordChange = &now
	logger.Info("Successfully rotated password", "user", user.Name)

	return nil
}

func (r *PostgresUserReconciler) shouldRotatePassword(user *databasev1.PostgresUser) bool {
	if user.Spec.Password.RotationPolicy == nil || !user.Spec.Password.RotationPolicy.Enabled {
		return user.Status.LastPasswordChange == nil
	}

	if user.Status.LastPasswordChange == nil {
		return true
	}

	interval := time.Duration(user.Spec.Password.RotationPolicy.Interval) * time.Hour
	return time.Since(user.Status.LastPasswordChange.Time) >= interval
}

func (r *PostgresUserReconciler) getPasswordFromSecret(ctx context.Context, user *databasev1.PostgresUser) (string, error) {
	var secretName, secretKey string

	if user.Spec.Password.SecretRef != nil {
		secretName = user.Spec.Password.SecretRef.Name
		secretKey = user.Spec.Password.SecretRef.Key
		if secretKey == "" {
			secretKey = "password"
		}
	} else {
		secretName = fmt.Sprintf("%s-password", user.Name)
		secretKey = "password"
	}

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: user.Namespace}, secret)
	if err != nil {
		return "", err
	}

	password, exists := secret.Data[secretKey]
	if !exists {
		return "", fmt.Errorf("password key %s not found in secret %s", secretKey, secretName)
	}

	return string(password), nil
}

func (r *PostgresUserReconciler) handleDeletion(ctx context.Context, user *databasev1.PostgresUser) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion of PostgresUser", "user", user.Name)

	if user.Status.Phase == "Ready" && user.Spec.ClusterRef != nil {
		cluster := &databasev1.PostgresCluster{}
		clusterKey := types.NamespacedName{
			Name:      user.Spec.ClusterRef.Name,
			Namespace: user.Spec.ClusterRef.Namespace,
		}
		if user.Spec.ClusterRef.Namespace == "" {
			clusterKey.Namespace = user.Namespace
		}

		err := r.Get(ctx, clusterKey, cluster)
		if err == nil && len(user.Status.InstanceStatuses) > 0 {
			for _, instanceStatus := range user.Status.InstanceStatuses {
				if instanceStatus.Ready {
					pgClient, err := postgres.NewClientForInstance(ctx, r.Client, cluster, instanceStatus.Name)
					if err == nil {
						defer pgClient.Close()
						if err := pgClient.DropUser(ctx, user.Spec.Username); err != nil {
							logger.Error(err, "Failed to drop user from instance", "instance", instanceStatus.Name)
						}
					}
				}
			}
		}
	}

	controllerutil.RemoveFinalizer(user, "database.example.com/postgres-user")
	if err := r.Update(ctx, user); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PostgresUserReconciler) updateStatus(ctx context.Context, user *databasev1.PostgresUser, status databasev1.PostgresUserStatus) error {
	if len(status.InstanceStatuses) == 0 && len(user.Status.InstanceStatuses) > 0 {
		status.InstanceStatuses = user.Status.InstanceStatuses
	}

	if status.LastPasswordChange == nil && user.Status.LastPasswordChange != nil {
		status.LastPasswordChange = user.Status.LastPasswordChange
	}

	user.Status = status
	return r.Status().Update(ctx, user)
}

func (r *PostgresUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.PostgresUser{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
