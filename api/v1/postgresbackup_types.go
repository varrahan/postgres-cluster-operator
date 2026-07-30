package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +groupName=database.example.com

type BackupReference struct {
	Name string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type PostgresBackupSpec struct {
	ClusterRef ClusterReference `json:"clusterRef"`
	Type string `json:"type,omitempty"`
	Storage BackupStorageSpec `json:"storage,omitempty"`
	RetentionPolicy RetentionPolicy `json:"retentionPolicy,omitempty"`
    Instances *InstanceSelector `json:"instance,omitempty"`
    
    IncludeInstanceConfig bool `json:"includeInstanceConfig,omitempty"`

	Options BackupOptions `json:"options,omitempty"`
}

type RetentionPolicy struct {
	KeepLast int32 `json:"keepLast,omitempty"`
	KeepDaily int32 `json:"keepDaily,omitempty"`
	KeepWeekly int32 `json:"keepWeekly,omitempty"`
	KeepMonthly int32 `json:"keepMonthly,omitempty"`
	DeleteOnClusterDeletion bool `json:"deleteOnClusterDeletion,omitempty"`
}

type BackupOptions struct {
	Compression int32 `json:"compression,omitempty"`
	Encryption EncryptionSpec `json:"encryption,omitempty"`
	ParallelJobs int32 `json:"parallelJobs,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

type EncryptionSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	SecretRef *SecretReference `json:"secretRef,omitempty"`
}

type BackupSpec struct {
	Enabled bool `json:"enabled,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	RetentionPolicy string `json:"retentionPolicy,omitempty"`
	Storage BackupStorageSpec `json:"storage"`
}

type BackupStorageSpec struct {
	Type string `json:"type"`
	Config map[string]string `json:"config"`
}

type PostgresBackupStatus struct {
	Phase string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	StartTime *metav1.Time `json:"startTime,omitempty"`
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	Size int64 `json:"size,omitempty"`
	Location string `json:"location,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	WALStart string `json:"walStart,omitempty"`
	WALEnd string `json:"walEnd,omitempty"`
	JobName string `json:"jobName,omitempty"`
	TargetInstance string `json:"targetInstance"`
	Database DatabaseStatus `json:"database"` 
}

type DatabaseStatus struct {
    Name       string            `json:"name"`
    Source     string            `json:"source"`
    Config map[string]string `json:"config,omitempty"`
    Exists     bool              `json:"exists"`
}

// +groupName=database.example.com  // ADD THIS (change to your domain)
type PostgresBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresBackupSpec   `json:"spec,omitempty"`
	Status PostgresBackupStatus `json:"status,omitempty"`
}

type PostgresBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresBackup{}, &PostgresBackupList{})
}
