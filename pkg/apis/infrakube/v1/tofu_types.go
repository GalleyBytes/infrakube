package v1

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +genclient
// Tofu is the Schema for the tofus API
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +k8s:openapi-gen=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:singular=tofu,path=tofus,shortName=tofu
type Tofu struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TofuSpec   `json:"spec,omitempty"`
	Status TofuStatus `json:"status,omitempty"`
}

// TofuSpec defines the desired state of Tofu
// +k8s:openapi-gen=true
type TofuSpec struct {

	// KeepLatestPodsOnly when true will keep only the pods that match the
	// current generation of the Tofu k8s-resource. This overrides the
	// behavior of `keepCompletedPods`.
	KeepLatestPodsOnly bool `json:"keepLatestPodsOnly,omitempty"`

	// KeepCompletedPods when true will keep completed pods. Default is false
	// and completed pods are removed.
	KeepCompletedPods bool `json:"keepCompletedPods,omitempty"`

	// OutputsSecret will create a secret with the outputs from the module. All
	// outputs from the module will be written to the secret unless the user
	// defines "outputsToInclude" or "outputsToOmit".
	OutputsSecret string `json:"outputsSecret,omitempty"`

	// OutputsToInclude is a whitelist of outputs to write when writing the
	// outputs to kubernetes.
	OutputsToInclude []string `json:"outputsToInclude,omitempty"`

	// OutputsToOmit is a blacklist of outputs to omit when writing the
	// outputs to kubernetes.
	OutputsToOmit []string `json:"outputsToOmit,omitempty"`

	// WriteOutputsToStatus will add the outputs from the module to the status
	// of the Tofu CustomResource.
	WriteOutputsToStatus bool `json:"writeOutputsToStatus,omitempty"`

	// PersistentVolumeSize define the size of the disk used to store
	// run data. If not defined, a default of "2Gi" is used.
	PersistentVolumeSize *resource.Quantity `json:"persistentVolumeSize,omitempty"`

	// StorageClassName is the name of the volume that infrakube will use to store
	// data. An empty value means that this volume does not belong to any StorageClassName and will
	// use the clusters default StorageClassName
	StorageClassName *string `json:"storageClassName,omitempty"`

	// ServiceAccount use a specific kubernetes ServiceAccount for running the create + destroy pods.
	// If not specified we create a new ServiceAccount per resource
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// Credentials is an array of credentials generally used for providers
	Credentials []Credentials `json:"credentials,omitempty"`

	// IgnoreDelete will bypass the finalization process and remove the resource
	// without running any delete jobs.
	IgnoreDelete bool `json:"ignoreDelete,omitempty"`

	// SSHTunnel can be defined for pulling from scm sources that cannot be accessed by the network the
	// operator/runner runs in. An example is enterprise-Github servers running on a private network.
	SSHTunnel *ProxyOpts `json:"sshTunnel,omitempty"`

	// SCMAuthMethods define multiple SCMs that require tokens/keys
	SCMAuthMethods []SCMAuthMethod `json:"scmAuthMethods,omitempty"`

	// Images describes the container images used by task classes.
	Images *TofuImages `json:"images,omitempty"`

	// Setup is configuration generally used once in the setup task
	Setup *Setup `json:"setup,omitempty"`

	// TofuModule is used to configure the source of the tofu module.
	TofuModule Module `json:"tofuModule"`

	// TofuVersion is the version of tofu which is used to run the module. The tofu version is
	// used as the tag of the tofu image regardless if images.tofu.image is defined with a tag. In
	// that case, the tag is stripped and replaced with this value.
	TofuVersion string `json:"tofuVersion"`

	// Backend is used to define a mandatory backend. Value must be a valid backend block.
	// For more information see https://opentofu.org/docs/language/settings/backends/configuration/
	Backend string `json:"backend"`

	// TaskOptions are a list of configuration options to be injected into task pods.
	TaskOptions []TaskOption `json:"taskOptions,omitempty"`

	// Plugins are tasks that run during a workflow but are not part of the main workflow.
	// Plugins can be treated as just another task, however, plugins do not have completion or failure
	// detection.
	// +optional
	Plugins map[TaskName]Plugin `json:"plugins,omitempty"`

	// RequireApproval will place a hold after completing a plan that prevents the workflow from continuing.
	// +optional
	RequireApproval bool `json:"requireApproval,omitempty"`
}

// TofuImages describes the container images used by task classes
// +k8s:openapi-gen=true
type TofuImages struct {
	// Tofu task type container image definition
	Tofu *ImageConfig `json:"tofu,omitempty"`
	// Script task type container image definition
	Script *ImageConfig `json:"script,omitempty"`
	// Setup task type container image definition
	Setup *ImageConfig `json:"setup,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// TofuList contains a list of Tofu
type TofuList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tofu `json:"items"`
}

// TofuStatus defines the observed state of Tofu
// +k8s:openapi-gen=true
type TofuStatus struct {
	// PodNamePrefix is used to identify this installation of the resource. For
	// very long resource names, like those greater than 220 characters, the
	// prefix ensures resource uniqueness for runners and other resources used
	// by the runner.
	PodNamePrefix string `json:"podNamePrefix"`

	// Phase is the current phase of the workflow
	Phase StatusPhase `json:"phase"`

	// LastCompletedGeneration shows the generation of the last completed workflow. This is not relevant for remotely
	// executed workflows.
	LastCompletedGeneration int64 `json:"lastCompletedGeneration"`

	// Outputs tofu outputs, when opt-in, will be added to this `status.outputs` field as key/value pairs
	Outputs map[string]string `json:"outputs,omitempty"`

	// Stage stores information about the current stage
	Stage Stage `json:"stage"`

	// PluginsStarted is a list of plugins that have been executed by the controller. Will get
	// refreshed each generation.
	// +optional
	PluginsStarted []TaskName `json:"pluginsStarted,omitempty"`

	// RetryEventReason copies the value of the resource label for 'kubernetes.io/change-cause'.
	// When '.setup' is is the suffix of the value, the pipeline will retry from the setup task.
	RetryEventReason *string      `json:"retryEventReason,omitempty"`
	RetryTimestamp   *metav1.Time `json:"retryTimestamp,omitempty"`
}

// MarshalJSON for TofuSpec ensures boolean fields are always marshalled even when false.
func (s TofuSpec) MarshalJSON() ([]byte, error) {
	type Alias TofuSpec
	return json.Marshal(&struct {
		*Alias
		KeepLatestPodsOnly   bool         `json:"keepLatestPodsOnly"`
		KeepCompletedPods    bool         `json:"keepCompletedPods"`
		WriteOutputsToStatus bool         `json:"writeOutputsToStatus"`
		IgnoreDelete         bool         `json:"ignoreDelete"`
		RequireApproval      bool         `json:"requireApproval"`
		TaskOptions          []TaskOption `json:"taskOptions"`
	}{
		Alias:                (*Alias)(&s),
		KeepLatestPodsOnly:   s.KeepLatestPodsOnly,
		KeepCompletedPods:    s.KeepCompletedPods,
		WriteOutputsToStatus: s.WriteOutputsToStatus,
		IgnoreDelete:         s.IgnoreDelete,
		RequireApproval:      s.RequireApproval,
		TaskOptions:          s.TaskOptions,
	})
}

func init() {
	SchemeBuilder.Register(&Tofu{}, &TofuList{})
}
