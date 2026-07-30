package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +groupName=database.example.com

type PostgresClusterSpec struct {

	PostgresVersion string `json:"postgresVersion"`
	Instances []PostgresInstanceSpec `json:"instances,omitempty"`
	HighAvailability HASpec `json:"highAvailability,omitempty"`
	Backup BackupSpec `json:"backup"`

	Replicas  int32                `json:"replicas,omitempty"`
    Role      string                `json:"role,omitempty"`
    Storage   StorageSpec          `json:"storage,omitempty"`
    Resources ResourceRequirements	`json:"resources,omitempty"`
	Database  DatabaseSpec			`json:"database,omitempty"`

	Monitoring MonitoringSpec `json:"monitoring,omitempty"`
}

type PostgresInstanceSpec struct {
    Name string `json:"name"`
    
    Replicas  *int32                `json:"replicas,omitempty"`
    Role      string                `json:"role,omitempty"`
    Storage   *StorageSpec          `json:"storage,omitempty"`
    Resources *ResourceRequirements	`json:"resources,omitempty"`
	Database  *DatabaseSpec			`json:"database,omitempty"`
    
    Config map[string]string      `json:"config,omitempty"`
}

type DatabaseSpec struct {
	Name string `json:"name,omitempty"`
	InitScript string `json:"initScript,omitempty"`
	Config map[string]string `json:"config,omitempty"`
    Access []DatabaseAccess `json:"access,omitempty"`
}

type DatabaseAccess struct {
    Username   string   `json:"username"`
	// +optional
    Privileges []string `json:"privileges"`
	// +optional
    Schemas    []SchemaAccess `json:"schemas,omitempty"`
}

type SchemaAccess struct {
    Name       string   `json:"name"`
	// +optional
    Privileges []string `json:"privileges,omitempty"`
}

type StorageSpec struct {
    VolumeName string `json:"volumeName,omitempty"`
	Size string `json:"size,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	AccessModes []string `json:"accessModes,omitempty"`
}

type ResourceRequirements struct {
	CPU string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Storage string `json:"storage,omitempty"`
}

type HASpec struct {
	Enabled bool `json:"enabled,omitempty"`
	SynchronousReplication bool `json:"synchronousReplication,omitempty"`
	FailoverTimeout int32 `json:"failoverTimeout,omitempty"`
}

type MonitoringSpec struct {
	Enabled bool `json:"enabled,omitempty"`

	Prometheus PrometheusSpec `json:"prometheus,omitempty"`
}

type PrometheusSpec struct {
	Enabled bool `json:"enabled,omitempty"`

	Port int32 `json:"port,omitempty"`

	Path string `json:"path,omitempty"`
}

type PostgresClusterStatus struct {
	Phase string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	CurrentPrimary string `json:"currentPrimary,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	LastBackup *metav1.Time `json:"lastBackup,omitempty"`
	DatabaseVersion string `json:"databaseVersion,omitempty"`

	Instances []PostgresInstanceStatus `json:"instances,omitempty"`
}

type PostgresInstanceStatus struct {
    Name           string `json:"name"`
    ReadyReplicas  int32  `json:"readyReplicas"`
    Role           string `json:"role,omitempty"`
    CurrentLeader  string `json:"currentLeader,omitempty"`
	Labels		   map[string]string `json:"labels,omitempty"`
}


//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.readyReplicas`
//+kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimary`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PostgresCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresClusterSpec   `json:"spec,omitempty"`
	Status PostgresClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true
type PostgresClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresCluster{}, &PostgresClusterList{})
}