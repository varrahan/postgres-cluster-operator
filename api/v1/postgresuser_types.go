package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +groupName=database.example.com

type PostgresUserSpec struct {
	ClusterRef *ClusterReference `json:"clusterRef"`
	Username string `json:"username"`
	Password *PasswordSpec `json:"password"`
	// +optional
	Privileges []string `json:"privileges,omitempty"`
	ConnectionLimit int32 `json:"connectionLimit,omitempty"`
	InstanceSelector *InstanceSelector `json:"instanceSelector,omitempty"`
}

type ClusterReference struct {
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

type PasswordSpec struct {
	SecretRef *SecretReference `json:"secretRef,omitempty"`
	// +optional
	Generate bool `json:"generate,omitempty"`
	// +optional
	Length int32 `json:"length,omitempty"`
	// +optional
	RotationPolicy *PasswordRotationPolicy `json:"rotationPolicy,omitempty"`
}

type SecretReference struct {
	Name string `json:"name"`

	// +optional
	Key string `json:"key,omitempty"`
}

type PasswordRotationPolicy struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +optional
	Interval int32 `json:"interval,omitempty"`
}

type PostgresUserStatus struct {
	Phase string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	LastPasswordChange *metav1.Time `json:"lastPasswordChange,omitempty"`
	DatabasesGranted []string `json:"databasesGranted,omitempty"`
	// +optional
	InstanceStatuses []UserInstanceStatus `json:"instanceStatuses,omitempty"`
}

type UserInstanceStatus struct {
	Name string `json:"name"`
	Ready bool `json:"ready"`
	// +optional
	Message string `json:"message,omitempty"`
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

type PostgresUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresUserSpec   `json:"spec,omitempty"`
	Status PostgresUserStatus `json:"status,omitempty"`
}

type PostgresUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresUser{}, &PostgresUserList{})
}