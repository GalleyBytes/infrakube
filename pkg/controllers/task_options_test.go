package controllers

import (
	"testing"

	infrakubev1 "github.com/galleybytes/infrakube/pkg/apis/infrakube/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewTaskOptionsPrefersResourceImageAndMergesTaskOptions(t *testing.T) {
	t.Parallel()

	tf := &infrakubev1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "example",
			Namespace:  "default",
			UID:        types.UID("terraform-uid"),
			Generation: 7,
		},
		Spec: infrakubev1.TerraformSpec{
			TerraformVersion: "1.6.0",
			Backend: `terraform {
  backend "kubernetes" {
    secret_suffix = "example"
    in_cluster_config = true
    namespace = "default"
  }
}`,
			Images: &infrakubev1.Images{
				Terraform: &infrakubev1.ImageConfig{
					Image:           "ghcr.io/example/custom-terraform-task",
					ImagePullPolicy: corev1.PullNever,
				},
			},
			TaskOptions: []infrakubev1.TaskOption{
				{
					For:         []infrakubev1.TaskName{"*"},
					Annotations: map[string]string{"global": "true"},
					Labels:      map[string]string{"team": "platform"},
					Env: []corev1.EnvVar{
						{Name: "GLOBAL_ENV", Value: "set"},
					},
					PolicyRules: []rbacv1.PolicyRule{{
						Verbs:     []string{"list"},
						APIGroups: []string{""},
						Resources: []string{"pods"},
					}},
				},
				{
					For: []infrakubev1.TaskName{infrakubev1.RunPlan},
					Script: infrakubev1.StageScript{
						Inline: "echo plan",
					},
				},
			},
			WriteOutputsToStatus: true,
		},
		Status: infrakubev1.TerraformStatus{
			PodNamePrefix: "example-abcd1234",
		},
	}

	runOpts := newTaskOptions(tf, infrakubev1.RunPlan, tf.Generation, nil, nil, nil, nil, "ghcr.io/galleybytes/require-approval:0.2.0", "http://cache", true, "https://releases.hashicorp.com/terraform", "ghcr.io/galleybytes/infrakube-task:controller-default")

	if runOpts.image != "ghcr.io/example/custom-terraform-task" {
		t.Fatalf("expected terraform image override to win, got %q", runOpts.image)
	}
	if runOpts.imagePullPolicy != corev1.PullNever {
		t.Fatalf("expected terraform pull policy override, got %q", runOpts.imagePullPolicy)
	}
	if runOpts.inlineTaskExecutionFile != "inline-plan.sh" {
		t.Fatalf("expected plan inline script, got %q", runOpts.inlineTaskExecutionFile)
	}
	if runOpts.serviceAccount != "tf-example-abcd1234-v7" {
		t.Fatalf("unexpected service account %q", runOpts.serviceAccount)
	}
	if runOpts.outputsSecretName != "example-abcd1234-v7-outputs" {
		t.Fatalf("unexpected outputs secret name %q", runOpts.outputsSecretName)
	}
	if !runOpts.saveOutputs {
		t.Fatalf("expected outputs to be saved when writeOutputsToStatus is enabled")
	}
	if runOpts.labels["team"] != "platform" {
		t.Fatalf("expected merged label, got %#v", runOpts.labels)
	}
	if runOpts.annotations["global"] != "true" {
		t.Fatalf("expected merged annotation, got %#v", runOpts.annotations)
	}
	if len(runOpts.env) != 1 || runOpts.env[0].Name != "GLOBAL_ENV" {
		t.Fatalf("expected wildcard env to be merged, got %#v", runOpts.env)
	}
	if len(runOpts.policyRules) != 1 {
		t.Fatalf("expected wildcard policy rules to be merged, got %#v", runOpts.policyRules)
	}

	runOpts.mainModulePluginData["backend_override.tf"] = tf.Spec.Backend
	role := runOpts.generateRole()
	if len(role.Rules) < 4 {
		t.Fatalf("expected kubernetes backend role rules plus custom rules, got %#v", role.Rules)
	}
}

func TestNewTaskOptionsUsesControllerTaskImageForDefaults(t *testing.T) {
	t.Parallel()

	tf := &infrakubev1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "example",
			Namespace:  "default",
			UID:        types.UID("terraform-uid"),
			Generation: 3,
		},
		Spec: infrakubev1.TerraformSpec{
			TerraformVersion: "1.6.0",
			Backend:          `terraform {}`,
		},
		Status: infrakubev1.TerraformStatus{
			PodNamePrefix: "example-abcd1234",
		},
	}

	runOpts := newTaskOptions(tf, infrakubev1.RunSetup, tf.Generation, nil, nil, nil, nil, "", "http://cache", true, "https://releases.hashicorp.com/terraform", "ghcr.io/galleybytes/infrakube-task:controller-default")
	if runOpts.image != "ghcr.io/galleybytes/infrakube-task:controller-default" {
		t.Fatalf("expected controller task image default, got %q", runOpts.image)
	}
	if runOpts.inlineTaskExecutionFile != "default-setup.sh" {
		t.Fatalf("expected default setup script, got %q", runOpts.inlineTaskExecutionFile)
	}

	pod, err := runOpts.generatePod()
	if err != nil {
		t.Fatalf("generate pod: %v", err)
	}

	env := envMap(pod.Spec.Containers[0].Env)
	if env["INFRAKUBE_TASK_EXEC_INLINE_SOURCE_FILE"] != "default-setup.sh" {
		t.Fatalf("expected setup pod to execute default setup script, got %#v", env["INFRAKUBE_TASK_EXEC_INLINE_SOURCE_FILE"])
	}
	if env["INFRAKUBE_TF_VERSION"] != "1.6.0" {
		t.Fatalf("expected terraform version env, got %#v", env["INFRAKUBE_TF_VERSION"])
	}
	if env["INFRAKUBE_TF_DOWNLOAD_URL_BASE"] != "https://releases.hashicorp.com/terraform" {
		t.Fatalf("expected terraform download URL env, got %#v", env["INFRAKUBE_TF_DOWNLOAD_URL_BASE"])
	}
}

func TestNewTofuTaskOptionsUsesControllerTaskImageForDefaults(t *testing.T) {
	t.Parallel()

	tofu := &infrakubev1.Tofu{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "example",
			Namespace:  "default",
			UID:        types.UID("tofu-uid"),
			Generation: 5,
		},
		Spec: infrakubev1.TofuSpec{
			TofuVersion: "1.9.0",
			Backend:     `terraform {}`,
		},
		Status: infrakubev1.TofuStatus{
			PodNamePrefix: "example-abcd1234",
		},
	}

	runOpts := newTofuTaskOptions(tofu, infrakubev1.RunPlan, tofu.Generation, nil, nil, nil, nil, "", "http://cache", true, "https://github.com/opentofu/opentofu/releases/download", "ghcr.io/galleybytes/infrakube-task:controller-default")
	if runOpts.image != "ghcr.io/galleybytes/infrakube-task:controller-default" {
		t.Fatalf("expected controller task image default, got %q", runOpts.image)
	}
	if runOpts.inlineTaskExecutionFile != "default-tofu.sh" {
		t.Fatalf("expected default tofu script, got %q", runOpts.inlineTaskExecutionFile)
	}

	pod, err := runOpts.generatePod()
	if err != nil {
		t.Fatalf("generate pod: %v", err)
	}

	env := envMap(pod.Spec.Containers[0].Env)
	if env["INFRAKUBE_TOFU_VERSION"] != "1.9.0" {
		t.Fatalf("expected tofu version env, got %#v", env["INFRAKUBE_TOFU_VERSION"])
	}
	if env["INFRAKUBE_TOFU_DOWNLOAD_URL_BASE"] != "https://github.com/opentofu/opentofu/releases/download" {
		t.Fatalf("expected tofu download URL env, got %#v", env["INFRAKUBE_TOFU_DOWNLOAD_URL_BASE"])
	}
}

func TestValidateVolumeRejectsReservedVolumeNames(t *testing.T) {
	t.Parallel()

	runOpts := TaskOptions{
		task:    infrakubev1.RunPlan,
		volumes: []corev1.Volume{{Name: "ssh"}},
		volumeMounts: []corev1.VolumeMount{{
			Name:      "ssh",
			MountPath: "/tmp/custom-ssh",
		}},
	}

	if err := runOpts.validateVolume(); err == nil {
		t.Fatalf("expected reserved volume names to be rejected")
	}
}

func envMap(envs []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(envs))
	for _, env := range envs {
		out[env.Name] = env.Value
	}
	return out
}
