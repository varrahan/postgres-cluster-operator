package controllers

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasev1 "postgres-operator/api/v1"
	"postgres-operator/internal/k8s"
	"postgres-operator/internal/utils"
)

// PostgresClusterReconciler reconciles a PostgresCluster object
type PostgresClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=database.example.com,resources=postgresclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=database.example.com,resources=postgresclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=database.example.com,resources=postgresclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *PostgresClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the PostgresCluster instance
	cluster := &databasev1.PostgresCluster{}
	err := r.Get(ctx, req.NamespacedName, cluster)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("PostgresCluster resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get PostgresCluster")
		return ctrl.Result{}, err
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(cluster, "database.example.com/postgres-cluster") {
		controllerutil.AddFinalizer(cluster, "database.example.com/postgres-cluster")
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Handle deletion
	if cluster.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, cluster)
	}

	// Set default values
	r.setDefaults(cluster)

	// Handle backward compatibility - create default instance if none specified
	if len(cluster.Spec.Instances) == 0 {
		cluster.Spec.Instances = []databasev1.PostgresInstanceSpec{
			{
				Name:      "primary",
				Replicas:  &cluster.Spec.Replicas,
				Role:      "primary",
				Database:  &cluster.Spec.Database,
				Storage:   &cluster.Spec.Storage,
				Resources: &cluster.Spec.Resources,
			},
		}
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile the PostgreSQL cluster
	if err := r.reconcileCluster(ctx, cluster); err != nil {
		logger.Error(err, "Failed to reconcile PostgreSQL cluster")
		return ctrl.Result{}, err
	}

	// Update status
	if err := r.updateStatus(ctx, cluster); err != nil {
		logger.Error(err, "Failed to update PostgresCluster status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Minute * 5}, nil
}

func (r *PostgresClusterReconciler) setDefaults(cluster *databasev1.PostgresCluster) {
	if cluster.Spec.Replicas == 0 {
		cluster.Spec.Replicas = 1
	}
	if cluster.Spec.PostgresVersion == "" {
		cluster.Spec.PostgresVersion = "15"
	}
	if cluster.Spec.Database.Name == "" {
		cluster.Spec.Database.Name = "postgres"
	}
	if cluster.Spec.Storage.Size == "" {
		cluster.Spec.Storage.Size = "10Gi"
	}
	if cluster.Spec.Resources.CPU == "" {
		cluster.Spec.Resources.CPU = "500m"
	}
	if cluster.Spec.Resources.Memory == "" {
		cluster.Spec.Resources.Memory = "1Gi"
	}
	if cluster.Spec.HighAvailability.FailoverTimeout == 0 {
		cluster.Spec.HighAvailability.FailoverTimeout = 30
	}
	if cluster.Spec.Monitoring.Enabled && cluster.Spec.Monitoring.Prometheus.Port == 0 {
		cluster.Spec.Monitoring.Prometheus.Port = 9187
	}
}

func (r *PostgresClusterReconciler) reconcileCluster(ctx context.Context, cluster *databasev1.PostgresCluster) error {
	logger := log.FromContext(ctx)

	// Create ConfigMap for PostgreSQL configuration
	if err := r.reconcileConfigMap(ctx, cluster); err != nil {
		return fmt.Errorf("failed to reconcile ConfigMap: %w", err)
	}

	// Create Secret for PostgreSQL credentials
	if err := r.reconcileSecret(ctx, cluster); err != nil {
		return fmt.Errorf("failed to reconcile Secret: %w", err)
	}

	// Reconcile each instance
	for _, instance := range cluster.Spec.Instances {
		if err := r.reconcileInstance(ctx, cluster, instance); err != nil {
			return fmt.Errorf("failed to reconcile instance %s: %w", instance.Name, err)
		}
	}

	// Reconcile monitoring if enabled
	if cluster.Spec.Monitoring.Enabled {
		if err := r.reconcileMonitoring(ctx, cluster); err != nil {
			return fmt.Errorf("failed to reconcile monitoring: %w", err)
		}
	}

	logger.Info("Successfully reconciled PostgreSQL cluster", "cluster", cluster.Name)
	return nil
}

func (r *PostgresClusterReconciler) reconcileConfigMap(ctx context.Context, cluster *databasev1.PostgresCluster) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-config", cluster.Name),
			Namespace: cluster.Namespace,
		},
		Data: map[string]string{
			"postgresql.conf": r.generatePostgresConfig(cluster),
		},
	}

	if err := controllerutil.SetControllerReference(cluster, configMap, r.Scheme); err != nil {
		return err
	}

	return k8s.CreateOrUpdate(ctx, r.Client, configMap)
}

func (r *PostgresClusterReconciler) generatePostgresConfig(cluster *databasev1.PostgresCluster) string {
	config := ""

	// Add HA settings if enabled
	if cluster.Spec.HighAvailability.Enabled {
		config += "synchronous_commit = on\n"
		if cluster.Spec.HighAvailability.SynchronousReplication {
			config += "synchronous_standby_names = '*'\n"
		}
	}

	// Add instance-specific configs
	for _, instance := range cluster.Spec.Instances {
		if len(instance.Config) > 0 {
			config += fmt.Sprintf("\n# Configuration for instance %s\n", instance.Name)
			for k, v := range instance.Config {
				config += fmt.Sprintf("%s = %s\n", k, v)
			}
		}
	}

	return config
}

func (r *PostgresClusterReconciler) reconcileSecret(ctx context.Context, cluster *databasev1.PostgresCluster) error {
	// Generate secure passwords
	postgresPassword, err := utils.GeneratePassword(16)
	if err != nil {
		return fmt.Errorf("failed to generate postgres password: %w", err)
	}

	replicationPassword, err := utils.GeneratePassword(16)
	if err != nil {
		return fmt.Errorf("failed to generate replication password: %w", err)
	}

	// Generate database access credentials
	dbUsers := make(map[string][]byte)
	for _, access := range cluster.Spec.Database.Access {
		userPassword, err := utils.GeneratePassword(16)
		if err != nil {
			return fmt.Errorf("failed to generate password for user %s: %w", access.Username, err)
		}
		dbUsers[access.Username] = []byte(userPassword)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-credentials", cluster.Name),
			Namespace: cluster.Namespace,
		},
		Data: map[string][]byte{
			"postgres-password":       []byte(postgresPassword),
			"replication-password":    []byte(replicationPassword),
		},
	}

	// Add database users to secret
	for username, password := range dbUsers {
		secret.Data[fmt.Sprintf("user-%s-password", username)] = password
	}

	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return err
	}

	return k8s.CreateOrUpdate(ctx, r.Client, secret)
}

func (r *PostgresClusterReconciler) reconcileInstance(
	ctx context.Context,
	cluster *databasev1.PostgresCluster,
	instance databasev1.PostgresInstanceSpec,
) error {
	// Create Service for this instance
	if err := r.reconcileInstanceService(ctx, cluster, instance); err != nil {
		return err
	}

	// Create StatefulSet for this instance
	if err := r.reconcileInstanceStatefulSet(ctx, cluster, instance); err != nil {
		return err
	}

	return nil
}

func (r *PostgresClusterReconciler) reconcileInstanceService(
	ctx context.Context,
	cluster *databasev1.PostgresCluster,
	instance databasev1.PostgresInstanceSpec,
) error {
	instanceName := getInstanceResourceName(cluster.Name, instance.Name)
	
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app":        "postgres",
				"cluster":    cluster.Name,
				"instance":   instance.Name,
				"role":       instance.Role,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":      "postgres",
				"cluster":  cluster.Name,
				"instance": instance.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "postgres",
					Port:     5432,
					Protocol: corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	if instance.Role == "primary" {
		service.Labels["primary"] = "true"
		service.Annotations = map[string]string{
			"service.alpha.kubernetes.io/tolerate-unready-endpoints": "true",
		}
	}

	if err := controllerutil.SetControllerReference(cluster, service, r.Scheme); err != nil {
		return err
	}

	return k8s.CreateOrUpdate(ctx, r.Client, service)
}

func (r *PostgresClusterReconciler) reconcileInstanceStatefulSet(
	ctx context.Context,
	cluster *databasev1.PostgresCluster,
	instance databasev1.PostgresInstanceSpec,
) error {
	logger := log.FromContext(ctx)
	instanceName := getInstanceResourceName(cluster.Name, instance.Name)

	// Determine effective values
	replicas := int32(1)
	if instance.Replicas != nil {
		replicas = *instance.Replicas
	}

	dbConfig := cluster.Spec.Database
	if instance.Database != nil {
		dbConfig = *instance.Database
	}

	storage := cluster.Spec.Storage
	if instance.Storage != nil {
		storage = *instance.Storage
	}

	resources := cluster.Spec.Resources
	if instance.Resources != nil {
		resources = *instance.Resources
	}

	// Parse resource requirements
	cpuRequest, err := resource.ParseQuantity(resources.CPU)
	if err != nil {
		return fmt.Errorf("invalid CPU value: %w", err)
	}
	memRequest, err := resource.ParseQuantity(resources.Memory)
	if err != nil {
		return fmt.Errorf("invalid Memory value: %w", err)
	}
	storageSize, err := resource.ParseQuantity(storage.Size)
	if err != nil {
		return fmt.Errorf("invalid storage size: %w", err)
	}

	// Set access modes
	accessModes := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	if len(storage.AccessModes) > 0 {
		accessModes = make([]corev1.PersistentVolumeAccessMode, len(storage.AccessModes))
		for i, mode := range storage.AccessModes {
			accessModes[i] = corev1.PersistentVolumeAccessMode(mode)
		}
	}

	// Configure storage class
	var storageClassName *string
	if storage.StorageClass != "" {
		storageClassName = &storage.StorageClass
	}

	// Security context
	runAsUser := int64(999)
	fsGroup := int64(999)

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app":      "postgres",
				"cluster":  cluster.Name,
				"instance": instance.Name,
				"role":     instance.Role,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: instanceName,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":      "postgres",
					"cluster":  cluster.Name,
					"instance": instance.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":      "postgres",
						"cluster":  cluster.Name,
						"instance": instance.Name,
						"role":     instance.Role,
					},
				},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: &runAsUser,
						FSGroup:   &fsGroup,
					},
					Containers: []corev1.Container{
						{
							Name:  "postgres",
							Image: fmt.Sprintf("postgres:%s", cluster.Spec.PostgresVersion),
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: 5432,
									Name:          "postgres",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name:  "POSTGRES_DB",
									Value: dbConfig.Name,
								},
								{
									Name: "POSTGRES_PASSWORD",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: fmt.Sprintf("%s-credentials", cluster.Name),
											},
											Key: "postgres-password",
										},
									},
								},
								{
									Name: "POSTGRES_REPLICATION_PASSWORD",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: fmt.Sprintf("%s-credentials", cluster.Name),
											},
											Key: "replication-password",
										},
									},
								},
								{
									Name:  "PGDATA",
									Value: "/var/lib/postgresql/data/pgdata",
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/var/lib/postgresql/data",
								},
								{
									Name:      "config",
									MountPath: "/etc/postgresql",
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuRequest,
									corev1.ResourceMemory: memRequest,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    cpuRequest,
									corev1.ResourceMemory: memRequest,
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{
											"pg_isready",
											"-U", "postgres",
											"-h", "localhost",
											"-p", "5432",
										},
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{
										Command: []string{
											"pg_isready",
											"-U", "postgres",
											"-h", "localhost",
											"-p", "5432",
										},
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
								TimeoutSeconds:      1,
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: fmt.Sprintf("%s-config", cluster.Name),
									},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
						Labels: map[string]string{
							"app":      "postgres",
							"cluster":  cluster.Name,
							"instance": instance.Name,
						},
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: accessModes,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
						StorageClassName: storageClassName,
					},
				},
			},
		},
	}

	// Add HA configuration for replicas
	if instance.Role == "replica" && cluster.Spec.HighAvailability.Enabled {
		primarySvc := fmt.Sprintf("%s-primary.%s.svc.cluster.local", cluster.Name, cluster.Namespace)
		statefulSet.Spec.Template.Spec.Containers[0].Env = append(
			statefulSet.Spec.Template.Spec.Containers[0].Env,
			corev1.EnvVar{
				Name:  "POSTGRES_REPLICATION_MODE",
				Value: "replica",
			},
			corev1.EnvVar{
				Name:  "POSTGRES_REPLICATION_PRIMARY_HOST",
				Value: primarySvc,
			},
		)
	}

	// Add init script if specified
	if dbConfig.InitScript != "" {
		statefulSet.Spec.Template.Spec.InitContainers = []corev1.Container{
			{
				Name:    "init-db",
				Image:   "busybox:1.28",
				Command: []string{"sh", "-c", dbConfig.InitScript},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "data",
						MountPath: "/var/lib/postgresql/data",
					},
				},
			},
		}
	}

	if err := controllerutil.SetControllerReference(cluster, statefulSet, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	if err := k8s.CreateOrUpdate(ctx, r.Client, statefulSet); err != nil {
		return fmt.Errorf("failed to create/update StatefulSet: %w", err)
	}

	logger.Info("Successfully reconciled StatefulSet", "statefulSet", statefulSet.Name)
	return nil
}

func (r *PostgresClusterReconciler) reconcileMonitoring(ctx context.Context, cluster *databasev1.PostgresCluster) error {
	if !cluster.Spec.Monitoring.Prometheus.Enabled {
		return nil
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-metrics", cluster.Name),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app":     "postgres",
				"cluster": cluster.Name,
				"monitor": "prometheus",
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app":     "postgres",
				"cluster": cluster.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "metrics",
					Port:       cluster.Spec.Monitoring.Prometheus.Port,
					TargetPort: intstr.FromInt(int(cluster.Spec.Monitoring.Prometheus.Port)),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	if err := controllerutil.SetControllerReference(cluster, service, r.Scheme); err != nil {
		return err
	}

	return k8s.CreateOrUpdate(ctx, r.Client, service)
}

func (r *PostgresClusterReconciler) updateStatus(ctx context.Context, cluster *databasev1.PostgresCluster) error {
	// Initialize instance status
	cluster.Status.Instances = nil
	totalReadyReplicas := int32(0)

	// Check status for each instance
	for _, instance := range cluster.Spec.Instances {
		instanceName := getInstanceResourceName(cluster.Name, instance.Name)
		
		sts := &appsv1.StatefulSet{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      instanceName,
			Namespace: cluster.Namespace,
		}, sts)
		
		instanceStatus := databasev1.PostgresInstanceStatus{
			Name:          instance.Name,
			Role:          instance.Role,
			ReadyReplicas: 0,
			Labels: map[string]string{
				"app":      "postgres",
				"cluster":  cluster.Name,
				"instance": instance.Name,
				"role":     instance.Role,
			},
		}
		
		if err == nil {
			instanceStatus.ReadyReplicas = sts.Status.ReadyReplicas
			totalReadyReplicas += sts.Status.ReadyReplicas
			
			// Mark first ready pod as leader for primary instances
			if instance.Role == "primary" && sts.Status.ReadyReplicas > 0 {
				instanceStatus.CurrentLeader = fmt.Sprintf("%s-0", instanceName)
				cluster.Status.CurrentPrimary = instanceStatus.CurrentLeader
			}
		}
		
		cluster.Status.Instances = append(cluster.Status.Instances, instanceStatus)
	}

	// Update overall cluster status
	expectedReplicas := int32(0)
	for _, instance := range cluster.Spec.Instances {
		if instance.Replicas != nil {
			expectedReplicas += *instance.Replicas
		} else {
			expectedReplicas += 1
		}
	}

	cluster.Status.ReadyReplicas = totalReadyReplicas
	if totalReadyReplicas == expectedReplicas {
		cluster.Status.Phase = "Running"
		cluster.Status.Message = "All instances are ready"
	} else if totalReadyReplicas > 0 {
		cluster.Status.Phase = "Degraded"
		cluster.Status.Message = fmt.Sprintf("Some instances not ready: %d/%d", totalReadyReplicas, expectedReplicas)
	} else {
		cluster.Status.Phase = "Pending"
		cluster.Status.Message = fmt.Sprintf("Waiting for instances: %d/%d ready", totalReadyReplicas, expectedReplicas)
	}

	cluster.Status.DatabaseVersion = cluster.Spec.PostgresVersion
	return r.Status().Update(ctx, cluster)
}

func getInstanceResourceName(clusterName, instanceName string) string {
	return fmt.Sprintf("%s-%s", clusterName, instanceName)
}

func (r *PostgresClusterReconciler) handleDeletion(ctx context.Context, cluster *databasev1.PostgresCluster) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Handling deletion of PostgresCluster", "cluster", cluster.Name)

	// Clean up resources
	for _, instance := range cluster.Spec.Instances {
		instanceName := getInstanceResourceName(cluster.Name, instance.Name)
		
		// Delete StatefulSet
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instanceName,
				Namespace: cluster.Namespace,
			},
		}
		if err := r.Delete(ctx, sts); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		
		// Delete Service
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instanceName,
				Namespace: cluster.Namespace,
			},
		}
		if err := r.Delete(ctx, svc); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
	}

	// Delete ConfigMap
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-config", cluster.Name),
			Namespace: cluster.Namespace,
		},
	}
	if err := r.Delete(ctx, configMap); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	// Delete Secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-credentials", cluster.Name),
			Namespace: cluster.Namespace,
		},
	}
	if err := r.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(cluster, "database.example.com/postgres-cluster")
	if err := r.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *PostgresClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1.PostgresCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
