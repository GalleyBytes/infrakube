//go:build integration

package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/galleybytes/infrakube/pkg/apis"
	infrakubev1 "github.com/galleybytes/infrakube/pkg/apis/infrakube/v1"
	"github.com/go-logr/logr"
	localcache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestIntegrationTerraformReconcileCreatesRunnerResources(t *testing.T) {
	testEnv := startEnvtest(t)
	ctx, stop := testEnv.ctx, testEnv.stop
	defer stop()

	namespace := newNamespace("tf-integration")
	if err := testEnv.rawClient.Create(ctx, namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	tf := &infrakubev1.Terraform{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "inline-module",
			Namespace: namespace.Name,
		},
		Spec: infrakubev1.TerraformSpec{
			TerraformVersion:     "1.5.7",
			WriteOutputsToStatus: true,
			TerraformModule: infrakubev1.Module{
				Inline: `output "hello" { value = "world" }`,
			},
			Backend: backendForNamespace(namespace.Name, "inline-module"),
		},
	}
	if err := testEnv.rawClient.Create(ctx, tf); err != nil {
		t.Fatalf("create terraform resource: %v", err)
	}

	reconciler := ReconcileTf{
		Client:               testEnv.cachedClient,
		Scheme:               testEnv.scheme,
		Recorder:             record.NewFakeRecorder(100),
		Log:                  logr.Discard(),
		Cache:                localcache.New(localcache.NoExpiration, localcache.NoExpiration),
		AffinityCacheKey:     "affinity",
		NodeSelectorCacheKey: "node-selector",
		TolerationsCacheKey:  "tolerations",
		CacheURL:             "http://cache",
		AutoDownload:         true,
		TfDownloadBaseURL:    "https://releases.hashicorp.com/terraform",
		TaskImage:            "ghcr.io/galleybytes/infrakube-task:test",
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(tf)}
	eventually(t, 15*time.Second, func() (bool, error) {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			return false, err
		}

		current := &infrakubev1.Terraform{}
		if err := testEnv.rawClient.Get(ctx, req.NamespacedName, current); err != nil {
			return false, err
		}
		if current.Status.PodNamePrefix == "" || current.Status.Stage.TaskType != infrakubev1.RunSetup || current.Status.Stage.State != infrakubev1.StateInProgress {
			return false, nil
		}

		runOpts := newTaskOptions(current, current.Status.Stage.TaskType, current.Status.Stage.Generation, nil, nil, nil, nil, "", "http://cache", true, "https://releases.hashicorp.com/terraform", "ghcr.io/galleybytes/infrakube-task:test")
		podList := &corev1.PodList{}
		if err := testEnv.rawClient.List(ctx, podList, client.InNamespace(namespace.Name)); err != nil {
			return false, err
		}
		if len(podList.Items) == 0 {
			return false, nil
		}

		for _, key := range []types.NamespacedName{
			{Name: runOpts.prefixedName, Namespace: namespace.Name},
			{Name: runOpts.versionedName, Namespace: namespace.Name},
			{Name: runOpts.outputsSecretName, Namespace: namespace.Name},
			{Name: runOpts.serviceAccount, Namespace: namespace.Name},
		} {
			if key.Name == "" {
				return false, fmt.Errorf("expected generated resource name to be set")
			}
		}
		return true, nil
	})

	current := &infrakubev1.Terraform{}
	if err := testEnv.rawClient.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get reconciled terraform resource: %v", err)
	}

	runOpts := newTaskOptions(current, current.Status.Stage.TaskType, current.Status.Stage.Generation, nil, nil, nil, nil, "", "http://cache", true, "https://releases.hashicorp.com/terraform", "ghcr.io/galleybytes/infrakube-task:test")
	assertConfigMapContainsKeys(t, ctx, testEnv.rawClient, types.NamespacedName{Name: runOpts.versionedName, Namespace: namespace.Name}, "backend_override.tf", "inline-module.tf")
	assertObjectExists(t, ctx, testEnv.rawClient, &corev1.PersistentVolumeClaim{}, types.NamespacedName{Name: runOpts.prefixedName, Namespace: namespace.Name})
	assertObjectExists(t, ctx, testEnv.rawClient, &corev1.Secret{}, types.NamespacedName{Name: runOpts.versionedName, Namespace: namespace.Name})
	assertObjectExists(t, ctx, testEnv.rawClient, &corev1.Secret{}, types.NamespacedName{Name: runOpts.outputsSecretName, Namespace: namespace.Name})
	assertObjectExists(t, ctx, testEnv.rawClient, &corev1.ServiceAccount{}, types.NamespacedName{Name: runOpts.serviceAccount, Namespace: namespace.Name})
	assertObjectExists(t, ctx, testEnv.rawClient, &rbacv1.Role{}, types.NamespacedName{Name: runOpts.versionedName, Namespace: namespace.Name})
	assertObjectExists(t, ctx, testEnv.rawClient, &rbacv1.RoleBinding{}, types.NamespacedName{Name: runOpts.versionedName, Namespace: namespace.Name})

	podList := &corev1.PodList{}
	if err := testEnv.rawClient.List(ctx, podList, client.InNamespace(namespace.Name)); err != nil {
		t.Fatalf("list terraform pods: %v", err)
	}
	if len(podList.Items) != 1 {
		t.Fatalf("expected one setup pod, got %d", len(podList.Items))
	}
	pod := podList.Items[0]
	if !strings.HasPrefix(pod.GenerateName, runOpts.versionedName+"-"+string(infrakubev1.RunSetup)+"-") {
		t.Fatalf("unexpected pod generateName %q", pod.GenerateName)
	}
	envVars := envVarMap(pod.Spec.Containers[0].Env)
	if envVars["INFRAKUBE_TF_VERSION"] != "1.5.7" {
		t.Fatalf("expected terraform version env, got %#v", envVars["INFRAKUBE_TF_VERSION"])
	}
	if envVars["INFRAKUBE_TASK_EXEC_INLINE_SOURCE_FILE"] != "default-setup.sh" {
		t.Fatalf("expected default setup script, got %#v", envVars["INFRAKUBE_TASK_EXEC_INLINE_SOURCE_FILE"])
	}
}

func TestIntegrationTofuReconcileCreatesRunnerResources(t *testing.T) {
	testEnv := startEnvtest(t)
	ctx, stop := testEnv.ctx, testEnv.stop
	defer stop()

	namespace := newNamespace("tofu-integration")
	if err := testEnv.rawClient.Create(ctx, namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	tofu := &infrakubev1.Tofu{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "inline-module",
			Namespace: namespace.Name,
		},
		Spec: infrakubev1.TofuSpec{
			TofuVersion:          "1.9.0",
			WriteOutputsToStatus: true,
			TofuModule: infrakubev1.Module{
				Inline: `output "hello" { value = "world" }`,
			},
			Backend: backendForNamespace(namespace.Name, "inline-module"),
		},
	}
	if err := testEnv.rawClient.Create(ctx, tofu); err != nil {
		t.Fatalf("create tofu resource: %v", err)
	}

	reconciler := ReconcileTofu{
		Client:               testEnv.cachedClient,
		Scheme:               testEnv.scheme,
		Recorder:             record.NewFakeRecorder(100),
		Log:                  logr.Discard(),
		Cache:                localcache.New(localcache.NoExpiration, localcache.NoExpiration),
		AffinityCacheKey:     "affinity",
		NodeSelectorCacheKey: "node-selector",
		TolerationsCacheKey:  "tolerations",
		CacheURL:             "http://cache",
		AutoDownload:         true,
		TofuDownloadBaseURL:  "https://github.com/opentofu/opentofu/releases/download",
		TaskImage:            "ghcr.io/galleybytes/infrakube-task:test",
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(tofu)}
	eventually(t, 15*time.Second, func() (bool, error) {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			return false, err
		}

		current := &infrakubev1.Tofu{}
		if err := testEnv.rawClient.Get(ctx, req.NamespacedName, current); err != nil {
			return false, err
		}
		if current.Status.PodNamePrefix == "" || current.Status.Stage.TaskType != infrakubev1.RunSetup || current.Status.Stage.State != infrakubev1.StateInProgress {
			return false, nil
		}

		podList := &corev1.PodList{}
		if err := testEnv.rawClient.List(ctx, podList, client.InNamespace(namespace.Name)); err != nil {
			return false, err
		}
		return len(podList.Items) == 1, nil
	})

	current := &infrakubev1.Tofu{}
	if err := testEnv.rawClient.Get(ctx, req.NamespacedName, current); err != nil {
		t.Fatalf("get reconciled tofu resource: %v", err)
	}

	runOpts := newTofuTaskOptions(current, current.Status.Stage.TaskType, current.Status.Stage.Generation, nil, nil, nil, nil, "", "http://cache", true, "https://github.com/opentofu/opentofu/releases/download", "ghcr.io/galleybytes/infrakube-task:test")
	assertConfigMapContainsKeys(t, ctx, testEnv.rawClient, types.NamespacedName{Name: runOpts.versionedName, Namespace: namespace.Name}, "backend_override.tf", "inline-module.tf")
	assertObjectExists(t, ctx, testEnv.rawClient, &corev1.PersistentVolumeClaim{}, types.NamespacedName{Name: runOpts.prefixedName, Namespace: namespace.Name})
	assertObjectExists(t, ctx, testEnv.rawClient, &rbacv1.Role{}, types.NamespacedName{Name: runOpts.versionedName, Namespace: namespace.Name})

	podList := &corev1.PodList{}
	if err := testEnv.rawClient.List(ctx, podList, client.InNamespace(namespace.Name)); err != nil {
		t.Fatalf("list tofu pods: %v", err)
	}
	if len(podList.Items) != 1 {
		t.Fatalf("expected one setup pod, got %d", len(podList.Items))
	}
	envVars := envVarMap(podList.Items[0].Spec.Containers[0].Env)
	if envVars["INFRAKUBE_TOFU_VERSION"] != "1.9.0" {
		t.Fatalf("expected tofu version env, got %#v", envVars["INFRAKUBE_TOFU_VERSION"])
	}
	if envVars["INFRAKUBE_TASK_EXEC_INLINE_SOURCE_FILE"] != "default-setup.sh" {
		t.Fatalf("expected default setup script, got %#v", envVars["INFRAKUBE_TASK_EXEC_INLINE_SOURCE_FILE"])
	}
}

type testEnvironment struct {
	ctx          context.Context
	stop         func()
	scheme       *runtime.Scheme
	cachedClient client.Client
	rawClient    client.Client
}

func startEnvtest(t *testing.T) testEnvironment {
	t.Helper()

	ctrl.SetLogger(logr.Discard())

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := apis.AddToScheme(scheme); err != nil {
		t.Fatalf("add infrakube scheme: %v", err)
	}

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{"../../deploy/crds"},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, "metadata.generateName", func(obj client.Object) []string {
		return []string{obj.(*corev1.Pod).GenerateName}
	}); err != nil {
		t.Fatalf("index pod generateName: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		t.Fatalf("manager cache failed to sync")
	}

	rawClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		cancel()
		t.Fatalf("create raw client: %v", err)
	}

	stop := func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil && !strings.Contains(err.Error(), "context canceled") {
				t.Fatalf("manager stopped with error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("manager did not stop in time")
		}
		if err := testEnv.Stop(); err != nil {
			t.Fatalf("stop envtest: %v", err)
		}
	}

	return testEnvironment{
		ctx:          ctx,
		stop:         stop,
		scheme:       scheme,
		cachedClient: mgr.GetClient(),
		rawClient:    rawClient,
	}
}

func newNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func backendForNamespace(namespace, suffix string) string {
	return fmt.Sprintf(`terraform {
  backend "kubernetes" {
    secret_suffix = %q
    in_cluster_config = true
    namespace = %q
  }
}`, suffix, namespace)
}

func eventually(t *testing.T, timeout time.Duration, fn func() (bool, error)) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		ok, err := fn()
		if err == nil && ok {
			return
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("condition not met before timeout: %v", err)
			}
			t.Fatalf("condition not met before timeout")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func assertObjectExists(t *testing.T, ctx context.Context, c client.Client, obj client.Object, key types.NamespacedName) {
	t.Helper()

	if err := c.Get(ctx, key, obj); err != nil {
		t.Fatalf("expected %T %s to exist: %v", obj, key.String(), err)
	}
}

func assertConfigMapContainsKeys(t *testing.T, ctx context.Context, c client.Client, key types.NamespacedName, requiredKeys ...string) {
	t.Helper()

	configMap := &corev1.ConfigMap{}
	if err := c.Get(ctx, key, configMap); err != nil {
		t.Fatalf("get configmap %s: %v", key.String(), err)
	}
	for _, requiredKey := range requiredKeys {
		if _, ok := configMap.Data[requiredKey]; !ok {
			t.Fatalf("expected configmap %s to contain key %q", key.String(), requiredKey)
		}
	}
}

func envVarMap(envs []corev1.EnvVar) map[string]string {
	out := make(map[string]string, len(envs))
	for _, env := range envs {
		out[env.Name] = env.Value
	}
	return out
}
