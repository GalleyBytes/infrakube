package controllers

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/MakeNowJust/heredoc"
	tfv1beta1 "github.com/galleybytes/infrakube/pkg/apis/infrakube/v1"
	"github.com/galleybytes/infrakube/pkg/utils"
	"github.com/go-logr/logr"
	localcache "github.com/patrickmn/go-cache"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

//go:embed scripts/tofu.sh
var defaultInlineTofuTaskExecutionFile string

const tofuLabelPrefix = "tofus.infrakube.galleybytes.com/"

type ReconcileTofu struct {
	Client                  client.Client
	Scheme                  *runtime.Scheme
	Recorder                record.EventRecorder
	Log                     logr.Logger
	MaxConcurrentReconciles int
	Cache                   *localcache.Cache

	GlobalEnvFromConfigmapData map[string]string
	GlobalEnvFromSecretData    map[string][]byte
	GlobalEnvSuffix            string

	InheritNodeSelector  bool
	NodeSelectorCacheKey string

	InheritAffinity  bool
	AffinityCacheKey string

	InheritTolerations  bool
	TolerationsCacheKey string

	RequireApprovalImage string

	CacheURL            string
	AutoDownload        bool
	TofuDownloadBaseURL string
	TaskImage           string
}

func (r ReconcileTofu) createEnvFromSources(ctx context.Context, tofu *tfv1beta1.Tofu) error {
	resourceName := tofu.Name
	resourceNamespace := tofu.Namespace
	name := fmt.Sprintf("%s-%s", resourceName, r.GlobalEnvSuffix)
	if len(r.GlobalEnvFromConfigmapData) > 0 {
		configMap := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: resourceNamespace,
			},
			Data: r.GlobalEnvFromConfigmapData,
		}
		controllerutil.SetControllerReference(tofu, &configMap, r.Scheme)
		errOnCreate := r.Client.Create(ctx, &configMap)
		if errOnCreate != nil {
			if errors.IsAlreadyExists(errOnCreate) {
				errOnUpdate := r.Client.Update(ctx, &configMap)
				if errOnUpdate != nil {
					return errOnUpdate
				}
			} else {
				return errOnCreate
			}
		}
	}

	if len(r.GlobalEnvFromSecretData) > 0 {
		secret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: resourceNamespace,
			},
			Data: r.GlobalEnvFromSecretData,
		}
		controllerutil.SetControllerReference(tofu, &secret, r.Scheme)
		errOnCreate := r.Client.Create(ctx, &secret)
		if errOnCreate != nil {
			if errors.IsAlreadyExists(errOnCreate) {
				errOnUpdate := r.Client.Update(ctx, &secret)
				if errOnUpdate != nil {
					return errOnUpdate
				}
			} else {
				return errOnCreate
			}
		}
	}

	return nil
}

func (r ReconcileTofu) listEnvFromSources(tofu *tfv1beta1.Tofu) []corev1.EnvFromSource {
	envFrom := []corev1.EnvFromSource{}
	resourceName := tofu.Name
	name := fmt.Sprintf("%s-%s", resourceName, r.GlobalEnvSuffix)

	if len(r.GlobalEnvFromConfigmapData) > 0 {
		envFrom = append(envFrom, corev1.EnvFromSource{
			ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: name,
				},
			},
		})
	}

	if len(r.GlobalEnvFromSecretData) > 0 {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: name,
				},
			},
		})
	}

	return envFrom
}

func (r *ReconcileTofu) SetupWithManager(mgr ctrl.Manager) error {
	controllerOptions := runtimecontroller.Options{
		MaxConcurrentReconciles: r.MaxConcurrentReconciles,
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&tfv1beta1.Tofu{}).
		Owns(&corev1.Pod{}).
		WithOptions(controllerOptions).
		Complete(r)
	if err != nil {
		return err
	}
	return nil
}

func newTofuTaskOptions(tofu *tfv1beta1.Tofu, task tfv1beta1.TaskName, generation int64, globalEnvFrom []corev1.EnvFromSource, affinity *corev1.Affinity, nodeSelector map[string]string, tolerations []corev1.Toleration, requireApprovalImage string, cacheURL string, autoDownload bool, tofuDownloadBaseURL string, taskImage string) TaskOptions {
	resourceName := tofu.Name
	resourceUUID := string(tofu.UID)
	prefixedName := tofu.Status.PodNamePrefix
	versionedName := prefixedName + "-v" + fmt.Sprint(tofu.Generation)
	tofuVersion := tofu.Spec.TofuVersion
	if tofuVersion == "" {
		tofuVersion = "latest"
	}

	image := ""
	imagePullPolicy := corev1.PullAlways
	policyRules := []rbacv1.PolicyRule{}
	lbls := make(map[string]string)
	annotations := make(map[string]string)
	env := []corev1.EnvVar{}
	envFrom := globalEnvFrom
	cleanupDisk := false
	urlSource := ""
	configMapSourceName := ""
	configMapSourceKey := ""
	restartPolicy := corev1.RestartPolicyNever
	inlineTaskExecutionFile := ""
	useDefaultInlineTaskExecutionFile := false
	volumes := []corev1.Volume{}
	volumeMounts := []corev1.VolumeMount{}

	for _, taskOption := range tofu.Spec.TaskOptions {
		if tfv1beta1.ListContainsTask(taskOption.For, task) ||
			tfv1beta1.ListContainsTask(taskOption.For, "*") {

			policyRules = append(policyRules, taskOption.PolicyRules...)
			for key, value := range taskOption.Annotations {
				annotations[key] = value
			}
			for key, value := range taskOption.Labels {
				lbls[key] = value
			}
			env = append(env, taskOption.Env...)
			envFrom = append(envFrom, taskOption.EnvFrom...)
			if taskOption.RestartPolicy != "" {
				restartPolicy = taskOption.RestartPolicy
			}

			volumes = append(volumes, taskOption.Volumes...)
			volumeMounts = append(volumeMounts, taskOption.VolumeMounts...)
		}
		if tfv1beta1.ListContainsTask(taskOption.For, task) {
			urlSource = taskOption.Script.Source
			if configMapSelector := taskOption.Script.ConfigMapSelector; configMapSelector != nil {
				configMapSourceName = configMapSelector.Name
				configMapSourceKey = configMapSelector.Key
			}
			if inlineScript := taskOption.Script.Inline; inlineScript != "" {
				inlineTaskExecutionFile = fmt.Sprintf("inline-%s.sh", task)
			}
		}
	}

	images := tofu.Spec.Images
	if images == nil {
		images = &tfv1beta1.TofuImages{}
	}

	defaultImage := fmt.Sprintf("%s:%s", tfv1beta1.TaskImageRepoDefault, tfv1beta1.TaskImageTagDefault)
	if taskImage != "" {
		defaultImage = taskImage
	}

	if images.Tofu == nil {
		images.Tofu = &tfv1beta1.ImageConfig{
			ImagePullPolicy: corev1.PullIfNotPresent,
		}
	}

	if images.Tofu.Image == "" {
		images.Tofu.Image = defaultImage
	}

	if images.Setup == nil {
		images.Setup = &tfv1beta1.ImageConfig{
			ImagePullPolicy: corev1.PullIfNotPresent,
		}
	}

	if images.Setup.Image == "" {
		images.Setup.Image = defaultImage
	}

	if images.Script == nil {
		images.Script = &tfv1beta1.ImageConfig{
			ImagePullPolicy: corev1.PullIfNotPresent,
		}
	}

	if images.Script.Image == "" {
		images.Script.Image = defaultImage
	}

	if inlineTaskExecutionFile == "" && urlSource == "" && (configMapSourceKey == "" || configMapSourceName == "") {
		useDefaultInlineTaskExecutionFile = true
	}
	if tfv1beta1.ListContainsTask(tfTaskList(), task) {
		image = images.Tofu.Image
		imagePullPolicy = images.Tofu.ImagePullPolicy
		if useDefaultInlineTaskExecutionFile {
			inlineTaskExecutionFile = "default-tofu.sh"
		}
	} else if tfv1beta1.ListContainsTask(scriptTaskList(), task) {
		image = images.Script.Image
		imagePullPolicy = images.Script.ImagePullPolicy
		if useDefaultInlineTaskExecutionFile {
			inlineTaskExecutionFile = "default-noop.sh"
		}
	} else if tfv1beta1.ListContainsTask(setupTaskList(), task) {
		image = images.Setup.Image
		imagePullPolicy = images.Setup.ImagePullPolicy
		if useDefaultInlineTaskExecutionFile {
			inlineTaskExecutionFile = "default-setup.sh"
		}
	}

	serviceAccount := tofu.Spec.ServiceAccount
	if serviceAccount == "" {
		serviceAccount = "tofu-" + versionedName
	}

	credentials := tofu.Spec.Credentials

	outputsSecretName := versionedName + "-outputs"
	saveOutputs := false
	stripGenerationLabelOnOutputsSecret := false
	if tofu.Spec.OutputsSecret != "" {
		outputsSecretName = tofu.Spec.OutputsSecret
		saveOutputs = true
		stripGenerationLabelOnOutputsSecret = true
	} else if tofu.Spec.WriteOutputsToStatus {
		saveOutputs = true
	}
	outputsToInclude := tofu.Spec.OutputsToInclude
	outputsToOmit := tofu.Spec.OutputsToOmit

	if tofu.Spec.Setup != nil {
		cleanupDisk = tofu.Spec.Setup.CleanupDisk
	}

	resourceLabels := map[string]string{
		tofuLabelPrefix + "generation":   fmt.Sprintf("%d", generation),
		tofuLabelPrefix + "resourceName": utils.AutoHashLabeler(resourceName),
		tofuLabelPrefix + "podPrefix":    prefixedName,
		tofuLabelPrefix + "tofuVersion":  tofuVersion,
		"app.kubernetes.io/name":         "infrakube",
		"app.kubernetes.io/component":    "infrakube-runner",
		"app.kubernetes.io/created-by":   "controller",
	}

	requireApproval := tofu.Spec.RequireApproval

	if task.ID() == -2 {
		resourceLabels[tofuLabelPrefix+"isPlugin"] = "true"
	}

	return TaskOptions{
		env:                                 env,
		generation:                          generation,
		configMapSourceName:                 configMapSourceName,
		configMapSourceKey:                  configMapSourceKey,
		envFrom:                             envFrom,
		policyRules:                         policyRules,
		annotations:                         annotations,
		labels:                              lbls,
		imagePullPolicy:                     imagePullPolicy,
		inheritedAffinity:                   affinity,
		inheritedNodeSelector:               nodeSelector,
		inheritedTolerations:                tolerations,
		inlineTaskExecutionFile:             inlineTaskExecutionFile,
		namespace:                           tofu.Namespace,
		resourceName:                        resourceName,
		prefixedName:                        prefixedName,
		versionedName:                       versionedName,
		credentials:                         credentials,
		tfVersion:                           tofuVersion,
		cacheURL:                            cacheURL,
		autoDownload:                        autoDownload,
		tfDownloadBaseURL:                   tofuDownloadBaseURL,
		image:                               image,
		task:                                task,
		resourceLabels:                      resourceLabels,
		resourceUUID:                        resourceUUID,
		serviceAccount:                      serviceAccount,
		mainModulePluginData:                make(map[string]string),
		secretData:                          make(map[string][]byte),
		cleanupDisk:                         cleanupDisk,
		outputsSecretName:                   outputsSecretName,
		saveOutputs:                         saveOutputs,
		stripGenerationLabelOnOutputsSecret: stripGenerationLabelOnOutputsSecret,
		outputsToInclude:                    outputsToInclude,
		outputsToOmit:                       outputsToOmit,
		urlSource:                           urlSource,
		requireApproval:                     requireApproval,
		requireApprovalImage:                requireApprovalImage,
		restartPolicy:                       restartPolicy,
		volumes:                             volumes,
		volumeMounts:                        volumeMounts,
		sidecarPlugins:                      nil,
		crdResource:                         "tofus",
		versionEnvVar:                       "INFRAKUBE_TOFU_VERSION",
		downloadURLEnvVar:                   "INFRAKUBE_TOFU_DOWNLOAD_URL_BASE",
	}
}

func (r *ReconcileTofu) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	reconcilerID := string(uuid.NewUUID())
	reqLogger := r.Log.WithValues("Tofu", request.NamespacedName, "id", reconcilerID)
	err := r.cacheNodeSelectors(ctx, reqLogger)
	if err != nil {
		panic(err)
	}
	lockKey := request.String() + "-reconcile-lock"
	lockOwner, lockFound := r.Cache.Get(lockKey)
	if lockFound {
		reqLogger.Info(fmt.Sprintf("Request is locked by '%s'", lockOwner.(string)))
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}
	r.Cache.Set(lockKey, reconcilerID, -1)
	defer r.Cache.Delete(lockKey)
	defer reqLogger.V(6).Info("Request has released reconcile lock")
	reqLogger.V(6).Info("Request has acquired reconcile lock")

	tofu, err := r.getTofuResource(ctx, request.NamespacedName, 3, reqLogger)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.V(1).Info("Tofu resource not found. Ignoring since object must be deleted")
			return reconcile.Result{}, nil
		}
		reqLogger.Error(err, "Failed to get Tofu")
		return reconcile.Result{}, err
	}

	if tofu.Status.Phase == tfv1beta1.PhaseDeleted {
		reqLogger.Info("Remove finalizers")
		if err := r.updateSecretFinalizer(ctx, tofu); err != nil {
			r.Recorder.Event(tofu, "Warning", "ProcessingError", err.Error())
			return reconcile.Result{}, err
		}
		_ = updateTofuFinalizer(tofu)
		err := r.update(ctx, tofu)
		if err != nil {
			r.Recorder.Event(tofu, "Warning", "ProcessingError", err.Error())
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	if updateTofuFinalizer(tofu) {
		err := r.update(ctx, tofu)
		if err != nil {
			return reconcile.Result{}, err
		}
		reqLogger.V(1).Info("Updated finalizer")
		return reconcile.Result{}, nil
	}

	if tofu.Status.PodNamePrefix == "" {
		tofu.Status.PodNamePrefix = fmt.Sprintf("%s-%s",
			utils.TruncateResourceName(tofu.Name, 54),
			utils.StringWithCharset(8, utils.AlphaNum),
		)
		tofu.Status.LastCompletedGeneration = 0
		tofu.Status.Phase = tfv1beta1.PhaseInitializing

		err := r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
		if err != nil {
			reqLogger.V(1).Info(err.Error())
		}
		return reconcile.Result{}, nil
	}

	if tofu.Status.Stage.Generation == 0 {
		task := tfv1beta1.RunSetup
		stageState := tfv1beta1.StateInitializing
		interruptible := tfv1beta1.CanNotBeInterrupt
		stage := newTofuStage(tofu, task, "TF_RESOURCE_CREATED", interruptible, stageState)
		if stage == nil {
			return reconcile.Result{}, fmt.Errorf("failed to create a new stage")
		}
		tofu.Status.Stage = *stage
		tofu.Status.PluginsStarted = []tfv1beta1.TaskName{}

		err := r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
		if err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	deletePhases := []string{
		string(tfv1beta1.PhaseDeleting),
		string(tfv1beta1.PhaseInitDelete),
		string(tfv1beta1.PhaseDeleted),
	}

	if tofu.GetDeletionTimestamp() != nil && !utils.ListContainsStr(deletePhases, string(tofu.Status.Phase)) {
		tofu.Status.Phase = tfv1beta1.PhaseInitDelete
	}

	retry := false
	if tofu.Labels != nil {
		if label, found := tofu.Labels["kubernetes.io/change-cause"]; found {

			if tofu.Status.RetryEventReason == nil {
				retry = true
			} else if *tofu.Status.RetryEventReason != label {
				retry = true
			}

			if retry {
				now := metav1.Now()
				tofu.Status.RetryEventReason = &label
				tofu.Status.RetryTimestamp = &now
				tofu.Status.Phase = tfv1beta1.PhaseInitializing
			}
		}
	}

	stage := r.checkSetNewStage(ctx, tofu, retry)
	if stage != nil {
		if stage.Reason == "RESTARTED_WORKFLOW" || stage.Reason == "RESTARTED_DELETE_WORKFLOW" {
			_ = r.removeOldPlan(tofu.Namespace, tofu.Name, tofu.Status.Stage.Reason, tofu.Generation)
		}
		reqLogger.V(2).Info(fmt.Sprintf("Stage moving from '%s' -> '%s'", tofu.Status.Stage.TaskType, stage.TaskType))
		tofu.Status.Stage = *stage
		desiredStatus := tofu.Status
		err := r.updateStatusWithRetry(ctx, tofu, &desiredStatus, reqLogger)
		if err != nil {
			reqLogger.V(1).Info(fmt.Sprintf("Error adding stage '%s': %s", stage.TaskType, err.Error()))
		}
		if tofu.Spec.KeepLatestPodsOnly {
			go r.backgroundReapOldGenerationPods(tofu, 0)
		}
		return reconcile.Result{}, nil
	}

	globalEnvFrom := r.listEnvFromSources(tofu)
	if err != nil {
		return reconcile.Result{}, err
	}
	currentStage := tofu.Status.Stage
	podType := currentStage.TaskType
	generation := currentStage.Generation
	affinity, nodeSelector, tolerations := r.getNodeSelectorsFromCache()
	runOpts := newTofuTaskOptions(tofu, currentStage.TaskType, generation, globalEnvFrom, affinity, nodeSelector, tolerations, r.RequireApprovalImage, r.CacheURL, r.AutoDownload, r.TofuDownloadBaseURL, r.TaskImage)

	if podType == tfv1beta1.RunNil {
		if tofu.Status.Phase == tfv1beta1.PhaseRunning {
			tofu.Status.Phase = tfv1beta1.PhaseCompleted
			if tofu.Spec.WriteOutputsToStatus {
				secret, err := r.loadSecret(ctx, runOpts.outputsSecretName, runOpts.namespace)
				if err != nil {
					reqLogger.Error(err, fmt.Sprintf("failed to load secret '%s'", runOpts.outputsSecretName))
				}
				keysInOutputs := []string{}
				for key := range secret.Data {
					keysInOutputs = append(keysInOutputs, key)
				}
				for key := range tofu.Status.Outputs {
					if !utils.ListContainsStr(keysInOutputs, key) {
						delete(tofu.Status.Outputs, key)
					}
				}
				for key, value := range secret.Data {
					if tofu.Status.Outputs == nil {
						tofu.Status.Outputs = make(map[string]string)
					}
					tofu.Status.Outputs[key] = string(value)
				}
			}
			err := r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
			if err != nil {
				reqLogger.V(1).Info(err.Error())
				return reconcile.Result{}, err
			}
		} else if tofu.Status.Phase == tfv1beta1.PhaseDeleting {
			tofu.Status.Phase = tfv1beta1.PhaseDeleted
			err := r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
			if err != nil {
				reqLogger.V(1).Info(err.Error())
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{Requeue: false}, nil
	}

	inNamespace := client.InNamespace(tofu.Namespace)
	f := fields.Set{
		"metadata.generateName": fmt.Sprintf("%s-%s-", tofu.Status.PodNamePrefix+"-v"+fmt.Sprint(generation), podType),
	}
	labelSelector := map[string]string{
		tofuLabelPrefix + "generation": fmt.Sprintf("%d", generation),
	}
	matchingFields := client.MatchingFields(f)
	matchingLabels := client.MatchingLabels(labelSelector)
	pods := &corev1.PodList{}
	err = r.Client.List(ctx, pods, inNamespace, matchingFields, matchingLabels)
	if err != nil {
		reqLogger.Error(err, "")
		return reconcile.Result{}, nil
	}

	if tofu.Status.RetryTimestamp != nil {
		podSlice := []corev1.Pod{}
		for _, pod := range pods.Items {
			if pod.CreationTimestamp.IsZero() || !pod.CreationTimestamp.Before(tofu.Status.RetryTimestamp) {
				podSlice = append(podSlice, pod)
			}
		}
		pods.Items = podSlice
	}

	if len(pods.Items) == 0 && tofu.Status.Stage.State == tfv1beta1.StateInProgress {
		tofu.Status.Stage.State = tfv1beta1.StateInitializing
		err = r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
		if err != nil {
			reqLogger.V(1).Info(err.Error())
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, nil
	}

	if len(pods.Items) == 0 {
		sidecarNames := []string{}
		for pluginTaskName, pluginConfig := range tofu.Spec.Plugins {
			if tfv1beta1.ListContainsTask(tofu.Status.PluginsStarted, pluginTaskName) {
				continue
			}

			when := pluginConfig.When
			whenTask := pluginConfig.Task
			switch when {
			case "After":
				if whenTask.ID() < podType.ID() {
					defer r.createPluginJob(ctx, reqLogger, tofu, pluginTaskName, pluginConfig, globalEnvFrom)
				}
			case "At":
				if whenTask.ID() == podType.ID() {
					defer r.createPluginJob(ctx, reqLogger, tofu, pluginTaskName, pluginConfig, globalEnvFrom)
				}
			case "Sidecar":
				if whenTask.ID() == podType.ID() {
					pluginSidecarPod, err := r.getPluginSidecarPod(ctx, reqLogger, tofu, pluginTaskName, pluginConfig, globalEnvFrom)
					if err != nil {
						if pluginConfig.Must {
							reqLogger.V(1).Info(err.Error())
							return reconcile.Result{Requeue: true}, nil
						}
						reqLogger.V(1).Info("Error adding sidecar plugin: %s", err.Error())
						continue
					}

					exists := false
					for _, c := range pluginSidecarPod.Spec.Containers {
						if utils.ListContainsStr(sidecarNames, c.Name) {
							exists = true
						}
					}
					if !exists {
						sidecarNames = append(sidecarNames, getContainerNames(pluginSidecarPod)...)
						runOpts.sidecarPlugins = append(runOpts.sidecarPlugins, *pluginSidecarPod)
					}
				}
			}
		}

		if (podType == tfv1beta1.RunPlan || podType == tfv1beta1.RunPlanDelete) && runOpts.requireApproval {
			requireApprovalSidecarPlugin := tfv1beta1.Plugin{
				ImageConfig: tfv1beta1.ImageConfig{
					Image:           runOpts.requireApprovalImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
				},
				Must: true,
			}
			pluginSidecarPod, err := r.getPluginSidecarPod(ctx, reqLogger, tofu, tfv1beta1.TaskName("require-approval"), requireApprovalSidecarPlugin, globalEnvFrom)
			if err != nil {
				reqLogger.V(1).Info("Error adding require-approval plugin: %s", err.Error())
				return reconcile.Result{Requeue: true}, nil
			}

			exists := false
			for _, c := range pluginSidecarPod.Spec.Containers {
				if utils.ListContainsStr(sidecarNames, c.Name) {
					exists = true
				}
			}
			if !exists {
				runOpts.sidecarPlugins = append(runOpts.sidecarPlugins, *pluginSidecarPod)
			}
		}

		reqLogger.V(1).Info(fmt.Sprintf("Setting up the '%s' pod", podType))
		err := r.setupAndRun(ctx, tofu, runOpts)
		if err != nil {
			reqLogger.Error(err, err.Error())
			return reconcile.Result{}, err
		}
		if tofu.Status.Phase == tfv1beta1.PhaseInitializing {
			tofu.Status.Phase = tfv1beta1.PhaseRunning
		} else if tofu.Status.Phase == tfv1beta1.PhaseInitDelete {
			tofu.Status.Phase = tfv1beta1.PhaseDeleting
		}
		tofu.Status.Stage.State = tfv1beta1.StateInProgress

		err = r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
		if err != nil {
			reqLogger.V(1).Info(err.Error())
			return reconcile.Result{Requeue: true}, nil
		}
		return reconcile.Result{}, nil
	}

	realPod := pods.Items[0]
	podName := realPod.ObjectMeta.Name
	podPhase := realPod.Status.Phase
	msg := fmt.Sprintf("Pod '%s' %s", podName, podPhase)

	tofu.Status.Stage.PodUID = string(realPod.UID)
	tofu.Status.Stage.PodName = podName
	if tofu.Status.Stage.Message != msg {
		tofu.Status.Stage.Message = msg
		reqLogger.Info(msg)
	}

	if realPod.Status.Phase == corev1.PodFailed {
		tofu.Status.Stage.State = tfv1beta1.StateFailed
		tofu.Status.Stage.StopTime = metav1.NewTime(time.Now())
		err = r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
		if err != nil {
			reqLogger.V(1).Info(err.Error())
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	if realPod.Status.Phase == corev1.PodSucceeded {
		tofu.Status.Stage.State = tfv1beta1.StateComplete
		tofu.Status.Stage.StopTime = metav1.NewTime(time.Now())
		err = r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
		if err != nil {
			reqLogger.V(1).Info(err.Error())
			return reconcile.Result{}, err
		}
		if !tofu.Spec.KeepCompletedPods && !tofu.Spec.KeepLatestPodsOnly {
			err := r.Client.Delete(ctx, &realPod)
			if err != nil {
				reqLogger.V(1).Info(err.Error())
			}
		}
		return reconcile.Result{}, nil
	}
	tofu.Status.Stage.State = tfv1beta1.StageState(realPod.Status.Phase)

	err = r.updateStatusWithRetry(ctx, tofu, &tofu.Status, reqLogger)
	if err != nil {
		reqLogger.V(1).Info(err.Error())
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r ReconcileTofu) getTofuResource(ctx context.Context, namespacedName types.NamespacedName, maxRetry int, reqLogger logr.Logger) (*tfv1beta1.Tofu, error) {
	tofu := &tfv1beta1.Tofu{}
	for retryCount := 1; retryCount <= maxRetry; retryCount++ {
		err := r.Client.Get(ctx, namespacedName, tofu)
		if err != nil {
			if errors.IsNotFound(err) {
				return tofu, err
			} else if retryCount < maxRetry {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return tofu, err
		} else {
			break
		}
	}
	return tofu, nil
}

func newTofuStage(tofu *tfv1beta1.Tofu, taskType tfv1beta1.TaskName, reason string, interruptible tfv1beta1.Interruptible, stageState tfv1beta1.StageState) *tfv1beta1.Stage {
	if reason == "GENERATION_CHANGE" {
		tofu.Status.PluginsStarted = []tfv1beta1.TaskName{}
		tofu.Status.Phase = tfv1beta1.PhaseInitializing
	}
	startTime := metav1.NewTime(time.Now())
	stopTime := metav1.NewTime(time.Unix(0, 0))
	if stageState == tfv1beta1.StateComplete {
		stopTime = startTime
	}
	return &tfv1beta1.Stage{
		Generation:    tofu.Generation,
		Interruptible: interruptible,
		Reason:        reason,
		State:         stageState,
		TaskType:      taskType,
		StartTime:     startTime,
		StopTime:      stopTime,
	}
}

func (r ReconcileTofu) checkSetNewStage(ctx context.Context, tofu *tfv1beta1.Tofu, isRetry bool) *tfv1beta1.Stage {
	var isNewStage bool
	var podType tfv1beta1.TaskName
	var reason string
	configuredTasks := getConfiguredTasks(&tofu.Spec.TaskOptions)

	deletePhases := []string{
		string(tfv1beta1.PhaseDeleted),
		string(tfv1beta1.PhaseInitDelete),
		string(tfv1beta1.PhaseDeleting),
	}
	isToBeDeletedOrIsDeleting := utils.ListContainsStr(deletePhases, string(tofu.Status.Phase))
	initDelete := tofu.Status.Phase == tfv1beta1.PhaseInitDelete
	stageState := tfv1beta1.StateInitializing
	interruptible := tfv1beta1.CanBeInterrupt

	currentStage := tofu.Status.Stage
	currentStagePodType := currentStage.TaskType
	currentStageCanNotBeInterrupted := currentStage.Interruptible == tfv1beta1.CanNotBeInterrupt
	currentStageIsRunning := currentStage.State == tfv1beta1.StateInProgress
	isNewGeneration := currentStage.Generation != tofu.Generation

	if isRetry && !isToBeDeletedOrIsDeleting && !isNewGeneration {
		isNewStage = true
		reason = *tofu.Status.RetryEventReason
		podType = tfv1beta1.RunInit
		if strings.HasSuffix(reason, ".setup") {
			podType = tfv1beta1.RunSetup
		}
		interruptible = isTaskInterruptable(podType)
	} else if isRetry && isToBeDeletedOrIsDeleting && !isNewGeneration {
		isNewStage = true
		reason = *tofu.Status.RetryEventReason
		podType = tfv1beta1.RunInitDelete
		if strings.HasSuffix(reason, ".setup") {
			podType = tfv1beta1.RunSetupDelete
		}
		interruptible = isTaskInterruptable(podType)
	} else if currentStageCanNotBeInterrupted && currentStageIsRunning {
		isNewStage = false
	} else if isNewGeneration && !isToBeDeletedOrIsDeleting {
		isNewStage = true
		reason = "GENERATION_CHANGE"
		podType = tfv1beta1.RunSetup
	} else if isNewGeneration && initDelete {
		isNewStage = true
		reason = "TF_RESOURCE_DELETED"
		podType = tfv1beta1.RunSetupDelete
		interruptible = tfv1beta1.CanNotBeInterrupt
	} else if isNewGeneration && isToBeDeletedOrIsDeleting {
		isNewStage = true
		reason = "TF_RESOURCE_DELETED"
		podType = tfv1beta1.RunSetupDelete
	} else if currentStage.State == tfv1beta1.StateComplete {
		isNewStage = true
		reason = fmt.Sprintf("COMPLETED_%s", strings.ToUpper(currentStage.TaskType.String()))

		switch currentStagePodType {
		case tfv1beta1.RunNil:
			isNewStage = false
		default:
			podType = nextTask(currentStagePodType, configuredTasks)
			interruptible = isTaskInterruptable(podType)
			if podType == tfv1beta1.RunNil {
				stageState = tfv1beta1.StateComplete
			}
		}
	} else if currentStage.State == tfv1beta1.StateFailed {
		if currentStage.TaskType == tfv1beta1.RunApply {
			err := r.Client.Get(ctx, types.NamespacedName{Namespace: tofu.Namespace, Name: tofu.Status.Stage.PodName}, &corev1.Pod{})
			if err != nil && errors.IsNotFound(err) {
				isNewStage = true
				reason = "RESTARTED_WORKFLOW"
				podType = nextTask(tfv1beta1.RunPostInit, configuredTasks)
				interruptible = isTaskInterruptable(podType)
			}
		} else if currentStage.TaskType == tfv1beta1.RunApplyDelete {
			pod := corev1.Pod{}
			err := r.Client.Get(ctx, types.NamespacedName{Namespace: tofu.Namespace, Name: tofu.Status.Stage.PodName}, &pod)
			if err != nil && errors.IsNotFound(err) {
				isNewStage = true
				reason = "RESTARTED_DELETE_WORKFLOW"
				podType = nextTask(tfv1beta1.RunPostInitDelete, configuredTasks)
				interruptible = isTaskInterruptable(podType)
			}
		}
	}
	if !isNewStage {
		return nil
	}
	return newTofuStage(tofu, podType, reason, interruptible, stageState)
}

func (r ReconcileTofu) removeOldPlan(namespace, name, reason string, generation int64) error {
	labelSelectors := []string{
		fmt.Sprintf(tofuLabelPrefix+"generation==%d", generation),
		fmt.Sprintf(tofuLabelPrefix+"resourceName=%s", utils.AutoHashLabeler(name)),
		"app.kubernetes.io/instance",
	}
	if reason == "RESTARTED_WORKFLOW" {
		labelSelectors = append(labelSelectors, []string{
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunSetup),
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunPreInit),
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunInit),
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunPostInit),
		}...)
	} else if reason == "RESTARTED_DELETE_WORKFLOW" {
		labelSelectors = append(labelSelectors, []string{
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunSetupDelete),
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunPreInitDelete),
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunInitDelete),
			fmt.Sprintf("app.kubernetes.io/instance!=%s", tfv1beta1.RunPostInitDelete),
		}...)
	}
	labelSelector, err := labels.Parse(strings.Join(labelSelectors, ","))
	if err != nil {
		return err
	}
	fieldSelector, err := fields.ParseSelector("status.phase!=Running")
	if err != nil {
		return err
	}
	err = r.Client.DeleteAllOf(context.TODO(), &corev1.Pod{}, &client.DeleteAllOfOptions{
		ListOptions: client.ListOptions{
			LabelSelector: labelSelector,
			Namespace:     namespace,
			FieldSelector: fieldSelector,
		},
	})
	if err != nil {
		return err
	}
	return nil
}

func (r ReconcileTofu) backgroundReapOldGenerationPods(tofu *tfv1beta1.Tofu, attempt int) {
	logger := r.Log.WithName("Reaper").WithValues("Tofu", fmt.Sprintf("%s/%s", tofu.Namespace, tofu.Name))
	if attempt > 20 {
		logger.Info("Could not reap resources: Max attempts to reap old-generation resources")
		return
	}

	ctx := context.TODO()
	namespacedName := types.NamespacedName{Namespace: tofu.Namespace, Name: tofu.Name}
	tofu, err := r.getTofuResource(ctx, namespacedName, 3, logger)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Tofu resource not found. Ignoring since object must be deleted")
			return
		}
		logger.Error(err, "Failed to get Tofu")
		return
	}

	labelSelector, err := labels.Parse(fmt.Sprintf(tofuLabelPrefix+"generation,"+tofuLabelPrefix+"generation!=%d,"+tofuLabelPrefix+"resourceName,"+tofuLabelPrefix+"resourceName=%s", tofu.Generation, utils.AutoHashLabeler(tofu.Name)))
	if err != nil {
		logger.Error(err, "Could not parse labels")
		return
	}
	fieldSelector, err := fields.ParseSelector("status.phase!=Running")
	if err != nil {
		logger.Error(err, "Could not parse fields")
		return
	}

	err = r.Client.DeleteAllOf(context.TODO(), &corev1.Pod{}, &client.DeleteAllOfOptions{
		ListOptions: client.ListOptions{
			LabelSelector: labelSelector,
			Namespace:     tofu.Namespace,
			FieldSelector: fieldSelector,
		},
	})
	if err != nil {
		logger.Error(err, "Could not reap old generation pods")
		return
	}

	podList := corev1.PodList{}
	err = r.Client.List(context.TODO(), &podList, &client.ListOptions{
		LabelSelector: labelSelector,
		Namespace:     tofu.Namespace,
	})
	if err != nil {
		logger.Error(err, "Could not list pods to reap")
		return
	}
	if len(podList.Items) > 0 {
		time.Sleep(30 * time.Second)
		attempt++
		go r.backgroundReapOldGenerationPods(tofu, attempt)
	} else {
		err = r.Client.DeleteAllOf(context.TODO(), &corev1.ConfigMap{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{
				LabelSelector: labelSelector,
				Namespace:     tofu.Namespace,
			},
		})
		if err != nil {
			logger.Error(err, "Could not reap old generation configmaps")
			return
		}

		err = r.Client.DeleteAllOf(context.TODO(), &corev1.Secret{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{
				LabelSelector: labelSelector,
				Namespace:     tofu.Namespace,
			},
		})
		if err != nil {
			logger.Error(err, "Could not reap old generation secrets")
			return
		}

		err = r.Client.DeleteAllOf(context.TODO(), &rbacv1.Role{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{
				LabelSelector: labelSelector,
				Namespace:     tofu.Namespace,
			},
		})
		if err != nil {
			logger.Error(err, "Could not reap old generation roles")
			return
		}

		err = r.Client.DeleteAllOf(context.TODO(), &rbacv1.RoleBinding{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{
				LabelSelector: labelSelector,
				Namespace:     tofu.Namespace,
			},
		})
		if err != nil {
			logger.Error(err, "Could not reap old generation roleBindings")
			return
		}

		err = r.Client.DeleteAllOf(context.TODO(), &corev1.ServiceAccount{}, &client.DeleteAllOfOptions{
			ListOptions: client.ListOptions{
				LabelSelector: labelSelector,
				Namespace:     tofu.Namespace,
			},
		})
		if err != nil {
			logger.Error(err, "Could not reap old generation serviceAccounts")
			return
		}
	}
}

func (r ReconcileTofu) reapPlugins(tofu *tfv1beta1.Tofu, attempt int) {
	logger := r.Log.WithName("ReaperPlugins").WithValues("Tofu", fmt.Sprintf("%s/%s", tofu.Namespace, tofu.Name))
	if attempt > 20 {
		logger.Info("Could not reap resources: Max attempts to reap old-generation resources")
		return
	}
	ctx := context.TODO()
	namespacedName := types.NamespacedName{Namespace: tofu.Namespace, Name: tofu.Name}
	tofu, err := r.getTofuResource(ctx, namespacedName, 3, logger)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.V(1).Info("Tofu resource not found. Ignoring since object must be deleted")
			return
		}
		logger.Error(err, "Failed to get Tofu")
		return
	}

	labelSelectorForPlugins, err := labels.Parse(fmt.Sprintf(tofuLabelPrefix+"isPlugin=true,"+tofuLabelPrefix+"generation,"+tofuLabelPrefix+"generation!=%d,"+tofuLabelPrefix+"resourceName,"+tofuLabelPrefix+"resourceName=%s", tofu.Generation, utils.AutoHashLabeler(tofu.Name)))
	if err != nil {
		logger.Error(err, "Could not parse labels")
	}

	deleteProppagationBackground := metav1.DeletePropagationBackground
	err = r.Client.DeleteAllOf(context.TODO(), &batchv1.Job{}, &client.DeleteAllOfOptions{
		ListOptions: client.ListOptions{
			LabelSelector: labelSelectorForPlugins,
			Namespace:     tofu.Namespace,
		},
		DeleteOptions: client.DeleteOptions{
			PropagationPolicy: &deleteProppagationBackground,
		},
	})
	if err != nil {
		logger.Error(err, "Could not reap old generation jobs")
	}

	err = r.Client.DeleteAllOf(context.TODO(), &corev1.Pod{}, &client.DeleteAllOfOptions{
		ListOptions: client.ListOptions{
			LabelSelector: labelSelectorForPlugins,
			Namespace:     tofu.Namespace,
		},
	})
	if err != nil {
		logger.Error(err, "Could not reap old generation pods")
	}

	podList := corev1.PodList{}
	err = r.Client.List(context.TODO(), &podList, &client.ListOptions{
		LabelSelector: labelSelectorForPlugins,
		Namespace:     tofu.Namespace,
	})
	if err != nil {
		logger.Error(err, "Could not list pods to reap")
	}
	if len(podList.Items) > 0 {
		time.Sleep(30 * time.Second)
		attempt++
		go r.reapPlugins(tofu, attempt)
	}
}

func (r ReconcileTofu) getNodeSelectorsFromCache() (*corev1.Affinity, map[string]string, []corev1.Toleration) {
	var affinity *corev1.Affinity
	var nodeSelector map[string]string
	var tolerations []corev1.Toleration
	if r.InheritAffinity {
		if obj, found := r.Cache.Get(r.AffinityCacheKey); found {
			affinity = obj.(*corev1.Affinity)
		}
	}
	if r.InheritNodeSelector {
		if obj, found := r.Cache.Get(r.NodeSelectorCacheKey); found {
			nodeSelector = obj.(map[string]string)
		}
	}
	if r.InheritTolerations {
		if obj, found := r.Cache.Get(r.TolerationsCacheKey); found {
			tolerations = obj.([]corev1.Toleration)
		}
	}

	return affinity, nodeSelector, tolerations
}

func (r ReconcileTofu) getPluginRunOpts(tofu *tfv1beta1.Tofu, pluginTaskName tfv1beta1.TaskName, pluginConfig tfv1beta1.Plugin, globalEnvFrom []corev1.EnvFromSource) TaskOptions {
	affinity, nodeSelector, tolerations := r.getNodeSelectorsFromCache()
	pluginRunOpts := newTofuTaskOptions(tofu, pluginTaskName, tofu.Generation, globalEnvFrom, affinity, nodeSelector, tolerations, r.RequireApprovalImage, r.CacheURL, r.AutoDownload, r.TofuDownloadBaseURL, r.TaskImage)
	pluginRunOpts.image = pluginConfig.Image
	pluginRunOpts.imagePullPolicy = pluginConfig.ImagePullPolicy
	return pluginRunOpts
}

func (r ReconcileTofu) getPluginSidecarPod(ctx context.Context, logger logr.Logger, tofu *tfv1beta1.Tofu, pluginTaskName tfv1beta1.TaskName, pluginConfig tfv1beta1.Plugin, globalEnvFrom []corev1.EnvFromSource) (*corev1.Pod, error) {
	return r.getPluginRunOpts(tofu, pluginTaskName, pluginConfig, globalEnvFrom).generatePod()
}

func (r ReconcileTofu) createPluginJob(ctx context.Context, logger logr.Logger, tofu *tfv1beta1.Tofu, pluginTaskName tfv1beta1.TaskName, pluginConfig tfv1beta1.Plugin, globalEnvFrom []corev1.EnvFromSource) (reconcile.Result, error) {
	pluginRunOpts := r.getPluginRunOpts(tofu, pluginTaskName, pluginConfig, globalEnvFrom)

	go func() {
		err := r.createJob(ctx, tofu, pluginRunOpts)
		if err != nil {
			logger.Error(err, fmt.Sprintf("Failed creating plugin job %s", pluginTaskName))
		} else {
			logger.Info(fmt.Sprintf("Starting the plugin job '%s'", pluginTaskName.String()))
		}
	}()
	tofu.Status.PluginsStarted = append(tofu.Status.PluginsStarted, pluginTaskName)
	err := r.updateStatusWithRetry(ctx, tofu, &tofu.Status, logger)
	if err != nil {
		logger.V(1).Info(err.Error())
	}
	return reconcile.Result{}, err
}

func updateTofuFinalizer(tofu *tfv1beta1.Tofu) bool {
	finalizers := tofu.GetFinalizers()

	if tofu.Status.Phase == tfv1beta1.PhaseDeleted {
		if utils.ListContainsStr(finalizers, tfFinalizer) {
			tofu.SetFinalizers(utils.ListRemoveStr(finalizers, tfFinalizer))
			return true
		}
	}

	if tofu.Spec.IgnoreDelete && len(finalizers) > 0 {
		if utils.ListContainsStr(finalizers, tfFinalizer) {
			tofu.SetFinalizers(utils.ListRemoveStr(finalizers, tfFinalizer))
			return true
		}
	}

	if !tofu.Spec.IgnoreDelete {
		if !utils.ListContainsStr(finalizers, tfFinalizer) {
			tofu.SetFinalizers(append(finalizers, tfFinalizer))
			return true
		}
	}
	return false
}

func (r ReconcileTofu) getGitSecrets(tofu *tfv1beta1.Tofu) []gitSecret {
	secrets := []gitSecret{}
	for _, m := range tofu.Spec.SCMAuthMethods {
		if m.Git.HTTPS != nil {
			ref := m.Git.HTTPS.TokenSecretRef
			namespace := ref.Namespace
			if ref.Namespace == "" {
				namespace = tofu.Namespace
			}
			secrets = append(secrets, gitSecret{
				name:          ref.Name,
				namespace:     namespace,
				shoudBeLocked: ref.LockSecretDeletion && !tofu.Spec.IgnoreDelete,
			})
		}
		if m.Git.SSH != nil {
			ref := m.Git.SSH.SSHKeySecretRef
			namespace := ref.Namespace
			if ref.Namespace == "" {
				namespace = tofu.Namespace
			}
			secrets = append(secrets, gitSecret{
				name:          ref.Name,
				namespace:     namespace,
				shoudBeLocked: ref.LockSecretDeletion && !tofu.Spec.IgnoreDelete,
			})
		}
	}
	return secrets
}

func (r ReconcileTofu) updateSecretFinalizer(ctx context.Context, tofu *tfv1beta1.Tofu) error {
	finalizerKey := utils.TruncateResourceName(fmt.Sprintf("finalizer.infrakube.galleybytes.com/%s", tofu.Name), 53)

	secrets := r.getGitSecrets(tofu)
	for _, m := range secrets {
		if m.shoudBeLocked && tofu.Status.Phase != tfv1beta1.PhaseDeleted {
			if err := r.lockGitSecretDeletion(ctx, m.name, m.namespace, finalizerKey); err != nil {
				return err
			}
		} else {
			if err := r.unlockGitSecretDeletion(ctx, m.name, m.namespace, finalizerKey); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r ReconcileTofu) lockGitSecretDeletion(ctx context.Context, name, namespace, finalizerKey string) error {
	secret, err := r.loadSecret(ctx, name, namespace)
	if err != nil {
		return err
	}
	if !controllerutil.ContainsFinalizer(secret, finalizerKey) {
		controllerutil.AddFinalizer(secret, finalizerKey)
		if err := r.Client.Update(ctx, secret); err != nil {
			return err
		}
	}
	return nil
}

func (r ReconcileTofu) unlockGitSecretDeletion(ctx context.Context, name, namespace, finalizerKey string) error {
	secret, err := r.loadSecret(ctx, name, namespace)
	if err != nil {
		return err
	}
	if controllerutil.ContainsFinalizer(secret, finalizerKey) {
		controllerutil.RemoveFinalizer(secret, finalizerKey)
		if err := r.Client.Update(ctx, secret); err != nil {
			return err
		}
	}
	return nil
}

func (r ReconcileTofu) update(ctx context.Context, tofu *tfv1beta1.Tofu) error {
	err := r.Client.Update(ctx, tofu)
	if err != nil {
		return fmt.Errorf("failed to update tofu resource: %s", err)
	}
	return nil
}

func (r ReconcileTofu) updateStatus(ctx context.Context, tofu *tfv1beta1.Tofu) error {
	err := r.Client.Status().Update(ctx, tofu)
	if err != nil {
		return fmt.Errorf("failed to update tofu status: %s", err)
	}
	return nil
}

func (r ReconcileTofu) updateStatusWithRetry(ctx context.Context, tofu *tfv1beta1.Tofu, desiredStatus *tfv1beta1.TofuStatus, logger logr.Logger) error {
	resourceNamespacedName := types.NamespacedName{Namespace: tofu.Namespace, Name: tofu.Name}
	var getResourceErr error
	var updateErr error
	for i := 0; i < 10; i++ {
		if i > 0 {
			n := math.Pow(2, float64(i+3))
			backoffTime := math.Ceil(.5 * (n - 1))
			time.Sleep(time.Duration(backoffTime) * time.Millisecond)
			tofu, getResourceErr = r.getTofuResource(ctx, resourceNamespacedName, 10, logger)
			if getResourceErr != nil {
				return fmt.Errorf("failed to get latest tofu while updating status: %s", getResourceErr)
			}
			if desiredStatus != nil {
				tofu.Status = *desiredStatus
			}
		}
		updateErr = r.Client.Status().Update(ctx, tofu)
		if updateErr != nil {
			logger.V(7).Info(fmt.Sprintf("Retrying to update status because an error has occurred while updating: %s", updateErr))
			continue
		}

		isUpdateConfirmed := false
		for j := 0; j < 10; j++ {
			tofu, updatedResourceErr := r.getTofuResource(ctx, resourceNamespacedName, 10, logger)
			if updatedResourceErr != nil {
				return fmt.Errorf("failed to get latest tofu while validating status: %s", updatedResourceErr)
			}

			if !tfv1beta1.TaskListsAreEqual(tofu.Status.PluginsStarted, desiredStatus.PluginsStarted) {
				logger.V(7).Info(fmt.Sprintf("Failed to confirm the status update because plugins did not equal. Have %s and Want %s", tofu.Status.PluginsStarted, desiredStatus.PluginsStarted))

			} else if stageItem := tofu.Status.Stage.IsEqual(desiredStatus.Stage); stageItem != "" {
				logger.V(7).Info(fmt.Sprintf("Failed to confirm the status update because stage item %s did not equal", stageItem))

			} else if tofu.Status.Phase != desiredStatus.Phase {
				logger.V(7).Info("Failed to confirm the status update because phase did not equal")

			} else if tofu.Status.PodNamePrefix != desiredStatus.PodNamePrefix {
				logger.V(7).Info("Failed to confirm the status update because podNamePrefix did not equal")

			} else {
				isUpdateConfirmed = true
			}

			if isUpdateConfirmed {
				break
			}

			logger.V(7).Info("Retrying to confirm the status update")
			n := math.Pow(2, float64(j+3))
			backoffTime := math.Ceil(.5 * (n - 1))
			time.Sleep(time.Duration(backoffTime) * time.Millisecond)
		}

		if isUpdateConfirmed {
			break
		}
		logger.V(7).Info("Retrying to update status because the update was not confirmed")

	}
	if updateErr != nil {
		return fmt.Errorf("failed to update tofu status: %s", updateErr)
	}
	return nil
}

func formatTofuJobSSHConfig(ctx context.Context, reqLogger logr.Logger, tofu *tfv1beta1.Tofu, k8sclient client.Client) (map[string][]byte, error) {
	data := make(map[string]string)
	dataAsByte := make(map[string][]byte)
	if tofu.Spec.SSHTunnel != nil {
		data["config"] = fmt.Sprintf("Host proxy\n"+
			"\tStrictHostKeyChecking no\n"+
			"\tUserKnownHostsFile=/dev/null\n"+
			"\tUser %s\n"+
			"\tHostname %s\n"+
			"\tIdentityFile ~/.ssh/proxy_key\n",
			tofu.Spec.SSHTunnel.User,
			tofu.Spec.SSHTunnel.Host)
		k := tofu.Spec.SSHTunnel.SSHKeySecretRef.Key
		if k == "" {
			k = "id_rsa"
		}
		ns := tofu.Spec.SSHTunnel.SSHKeySecretRef.Namespace
		if ns == "" {
			ns = tofu.Namespace
		}

		key, err := loadPassword(ctx, k8sclient, k, tofu.Spec.SSHTunnel.SSHKeySecretRef.Name, ns)
		if err != nil {
			return dataAsByte, err
		}
		data["proxy_key"] = key

	}

	for _, m := range tofu.Spec.SCMAuthMethods {
		if m.Git.SSH != nil {
			if m.Git.SSH.RequireProxy {
				data["config"] += fmt.Sprintf("\nHost %s\n"+
					"\tStrictHostKeyChecking no\n"+
					"\tUserKnownHostsFile=/dev/null\n"+
					"\tHostname %s\n"+
					"\tIdentityFile ~/.ssh/%s\n"+
					"\tProxyJump proxy",
					m.Host,
					m.Host,
					m.Host)
			} else {
				data["config"] += fmt.Sprintf("\nHost %s\n"+
					"\tStrictHostKeyChecking no\n"+
					"\tUserKnownHostsFile=/dev/null\n"+
					"\tHostname %s\n"+
					"\tIdentityFile ~/.ssh/%s\n",
					m.Host,
					m.Host,
					m.Host)
			}
			k := m.Git.SSH.SSHKeySecretRef.Key
			if k == "" {
				k = "id_rsa"
			}
			ns := m.Git.SSH.SSHKeySecretRef.Namespace
			if ns == "" {
				ns = tofu.Namespace
			}
			key, err := loadPassword(ctx, k8sclient, k, m.Git.SSH.SSHKeySecretRef.Name, ns)
			if err != nil {
				return dataAsByte, err
			}
			data[m.Host] = key
		}
	}

	for k, v := range data {
		dataAsByte[k] = []byte(v)
	}

	return dataAsByte, nil
}

func (r *ReconcileTofu) setupAndRun(ctx context.Context, tofu *tfv1beta1.Tofu, runOpts TaskOptions) error {
	reqLogger := r.Log.WithValues("Tofu", types.NamespacedName{Name: tofu.Name, Namespace: tofu.Namespace}.String())
	var err error

	reason := tofu.Status.Stage.Reason
	isNewGeneration := reason == "GENERATION_CHANGE" || reason == "TF_RESOURCE_DELETED"
	isFirstInstall := reason == "TF_RESOURCE_CREATED"
	isChanged := isNewGeneration || isFirstInstall

	scmMap := make(map[string]scmType)
	for _, v := range tofu.Spec.SCMAuthMethods {
		if v.Git != nil {
			scmMap[v.Host] = gitScmType
		}
	}

	if tofu.Spec.TofuModule.Inline != "" {
		runOpts.mainModulePluginData["inline-module.tf"] = tofu.Spec.TofuModule.Inline
	} else if tofu.Spec.TofuModule.ConfigMapSeclector_x != nil {
		b, err := json.Marshal(tofu.Spec.TofuModule.ConfigMapSeclector_x)
		if err != nil {
			return err
		}
		runOpts.mainModulePluginData[".__INFRAKUBE__ConfigMapModule.json"] = string(b)
	} else if tofu.Spec.TofuModule.ConfigMapSelector != nil {
		b, err := json.Marshal(tofu.Spec.TofuModule.ConfigMapSelector)
		if err != nil {
			return err
		}
		runOpts.mainModulePluginData[".__INFRAKUBE__ConfigMapModule.json"] = string(b)
	} else if tofu.Spec.TofuModule.Source != "" {
		runOpts.tfModuleParsed, err = getParsedAddress(tofu.Spec.TofuModule.Source, "", false, scmMap)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("no tofu module detected")
	}

	if isChanged {
		if err := r.updateSecretFinalizer(ctx, tofu); err != nil {
			reqLogger.V(3).Info("Could not update secret finalizer", "ERR", err.Error())
		}

		go r.reapPlugins(tofu, 0)

		runOpts.mainModulePluginData["default-tofu.sh"] = defaultInlineTofuTaskExecutionFile
		runOpts.mainModulePluginData["default-setup.sh"] = defaultInlineSetupTaskExecutionFile
		runOpts.mainModulePluginData["default-noop.sh"] = defaultInlineNoOpExecutionFile

		for _, taskOption := range tofu.Spec.TaskOptions {
			if inlineScript := taskOption.Script.Inline; inlineScript != "" {
				for _, affected := range taskOption.For {
					if affected.String() == "*" {
						continue
					}
					runOpts.mainModulePluginData[fmt.Sprintf("inline-%s.sh", affected)] = inlineScript
				}
			}
		}

		for _, m := range tofu.Spec.SCMAuthMethods {
			if m.Git.HTTPS != nil {
				if _, found := runOpts.secretData["gitAskpass"]; found {
					continue
				}
				tokenSecret := *m.Git.HTTPS.TokenSecretRef
				if tokenSecret.Key == "" {
					tokenSecret.Key = "token"
				}
				gitAskpass, err := r.createGitAskpass(ctx, tokenSecret)
				if err != nil {
					return err
				}
				runOpts.secretData["gitAskpass"] = gitAskpass
			}
		}

		sshConfigData, err := formatTofuJobSSHConfig(ctx, reqLogger, tofu, r.Client)
		if err != nil {
			r.Recorder.Event(tofu, "Warning", "SSHConfigError", fmt.Errorf("%v", err).Error())
			return fmt.Errorf("error setting up sshconfig: %v", err)
		}
		for k, v := range sshConfigData {
			runOpts.secretData[k] = v
		}

		resourceDownloadItems := []ParsedAddress{}
		if tofu.Spec.Setup != nil {
			for _, s := range tofu.Spec.Setup.ResourceDownloads {
				address := strings.TrimSpace(s.Address)
				parsedAddress, err := getParsedAddress(address, s.Path, s.UseAsVar, scmMap)
				if err != nil {
					return err
				}
				resourceDownloadItems = append(resourceDownloadItems, parsedAddress)
			}
		}
		b, err := json.Marshal(resourceDownloadItems)
		if err != nil {
			return err
		}
		resourceDownloads := string(b)

		runOpts.mainModulePluginData[".__INFRAKUBE__ResourceDownloads.json"] = resourceDownloads

		runOpts.mainModulePluginData["backend_override.tf"] = tofu.Spec.Backend
	}

	err = r.run(ctx, reqLogger, tofu, runOpts, isNewGeneration, isFirstInstall)
	if err != nil {
		return err
	}

	return nil
}

func (r ReconcileTofu) resourceExists(ctx context.Context, key types.NamespacedName, obj client.Object) (bool, error) {
	err := r.Client.Get(ctx, key, obj)
	if errors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (r ReconcileTofu) deleteAndCreate(ctx context.Context, tofu *tfv1beta1.Tofu, kind string, obj client.Object) error {
	lookupKey := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
	existing := obj.DeepCopyObject().(client.Object)
	err := r.Client.Get(ctx, lookupKey, existing)
	if err == nil {
		if delErr := r.Client.Delete(ctx, existing); delErr != nil && !errors.IsNotFound(delErr) {
			return delErr
		}
	} else if !errors.IsNotFound(err) {
		return err
	}
	controllerutil.SetControllerReference(tofu, obj, r.Scheme)
	err = r.Client.Create(ctx, obj)
	if err != nil {
		r.Recorder.Event(tofu, "Warning", fmt.Sprintf("%sCreateError", kind), fmt.Sprintf("Could not create %s %v", kind, err))
		return err
	}
	r.Recorder.Event(tofu, "Normal", "SuccessfulCreate", fmt.Sprintf("Created %s: '%s'", kind, obj.GetName()))
	return nil
}

func (r ReconcileTofu) createPVC(ctx context.Context, tofu *tfv1beta1.Tofu, runOpts TaskOptions) error {
	kind := "PersistentVolumeClaim"
	lookupKey := types.NamespacedName{
		Name:      runOpts.prefixedName,
		Namespace: runOpts.namespace,
	}
	if found, err := r.resourceExists(ctx, lookupKey, &corev1.PersistentVolumeClaim{}); err != nil {
		return nil
	} else if found {
		return nil
	}
	persistentVolumeSize := resource.MustParse("2Gi")
	if tofu.Spec.PersistentVolumeSize != nil {
		persistentVolumeSize = *tofu.Spec.PersistentVolumeSize
	}
	pvc := runOpts.generatePVC(persistentVolumeSize, tofu.Spec.StorageClassName)
	controllerutil.SetControllerReference(tofu, pvc, r.Scheme)

	err := r.Client.Create(ctx, pvc)
	if err != nil {
		r.Recorder.Event(tofu, "Warning", fmt.Sprintf("%sCreateError", kind), fmt.Sprintf("Could not create %s %v", kind, err))
		return err
	}
	r.Recorder.Event(tofu, "Normal", "SuccessfulCreate", fmt.Sprintf("Created %s: '%s'", kind, pvc.Name))
	return nil
}

func (r ReconcileTofu) createSecret(ctx context.Context, tofu *tfv1beta1.Tofu, name, namespace string, data map[string][]byte, recreate bool, labelsToOmit []string, runOpts TaskOptions) error {
	kind := "Secret"

	lbls := make(map[string]string)
	for key, value := range runOpts.resourceLabels {
		lbls[key] = value
	}
	for _, labelKey := range labelsToOmit {
		delete(lbls, labelKey)
	}

	secretObj := runOpts.generateSecret(name, namespace, data, lbls)
	controllerutil.SetControllerReference(tofu, secretObj, r.Scheme)

	if recreate {
		lookupKey := types.NamespacedName{Name: secretObj.Name, Namespace: secretObj.Namespace}
		existing := &corev1.Secret{}
		err := r.Client.Get(ctx, lookupKey, existing)
		if err == nil {
			if delErr := r.Client.Delete(ctx, existing); delErr != nil && !errors.IsNotFound(delErr) {
				return delErr
			}
		} else if !errors.IsNotFound(err) {
			return err
		}
	}

	err := r.Client.Create(ctx, secretObj)
	if err != nil {
		if !recreate && errors.IsAlreadyExists(err) {
			// acceptable since the resource exists and was not expected to be new
		} else {
			r.Recorder.Event(tofu, "Warning", fmt.Sprintf("%sCreateError", kind), fmt.Sprintf("Could not create %s %v", kind, err))
			return err
		}
	} else {
		r.Recorder.Event(tofu, "Normal", "SuccessfulCreate", fmt.Sprintf("Created %s: '%s'", kind, secretObj.Name))
	}
	return nil
}

func (r ReconcileTofu) createPod(ctx context.Context, tofu *tfv1beta1.Tofu, runOpts TaskOptions) error {
	kind := "Pod"

	pod, err := runOpts.generatePod()
	if err != nil {
		r.Recorder.Event(tofu, "Warning", fmt.Sprintf("%sCreateError", kind), fmt.Sprintf("%s", err))
		return err
	}

	controllerutil.SetControllerReference(tofu, pod, r.Scheme)

	err = r.Client.Create(ctx, pod)
	if err != nil {
		r.Recorder.Event(tofu, "Warning", fmt.Sprintf("%sCreateError", kind), fmt.Sprintf("Could not create %s %v", kind, err))
		return err
	}
	r.Recorder.Event(tofu, "Normal", "SuccessfulCreate", fmt.Sprintf("Created %s: '%s'", kind, pod.Name))
	return nil
}

func (r ReconcileTofu) createJob(ctx context.Context, tofu *tfv1beta1.Tofu, runOpts TaskOptions) error {
	kind := "Job"

	job := runOpts.generateJob()
	controllerutil.SetControllerReference(tofu, job, r.Scheme)

	err := r.Client.Create(ctx, job)
	if err != nil {
		r.Recorder.Event(tofu, "Warning", fmt.Sprintf("%sCreateError", kind), fmt.Sprintf("Could not create %s %v", kind, err))
		return err
	}
	r.Recorder.Event(tofu, "Normal", "SuccessfulCreate", fmt.Sprintf("Created %s: '%s'", kind, job.Name))
	return nil
}

func (r ReconcileTofu) run(ctx context.Context, reqLogger logr.Logger, tofu *tfv1beta1.Tofu, runOpts TaskOptions, isNewGeneration, isFirstInstall bool) (err error) {

	if isFirstInstall || isNewGeneration {
		if err := r.createEnvFromSources(ctx, tofu); err != nil {
			return err
		}

		if err := r.createPVC(ctx, tofu, runOpts); err != nil {
			return err
		}

		if err := r.createSecret(ctx, tofu, runOpts.versionedName, runOpts.namespace, runOpts.secretData, true, []string{}, runOpts); err != nil {
			return err
		}

		configMap := runOpts.generateConfigMap()
		if err := r.deleteAndCreate(ctx, tofu, "ConfigMap", configMap); err != nil {
			return err
		}

		roleBinding := runOpts.generateRoleBinding()
		if err := r.deleteAndCreate(ctx, tofu, "RoleBinding", roleBinding); err != nil {
			return err
		}

		role := runOpts.generateRole()
		if err := r.deleteAndCreate(ctx, tofu, "Role", role); err != nil {
			return err
		}

		if tofu.Spec.ServiceAccount == "" {
			sa := runOpts.generateServiceAccount()
			if err := r.deleteAndCreate(ctx, tofu, "ServiceAccount", sa); err != nil {
				return err
			}
		}

		labelsToOmit := []string{}
		if runOpts.stripGenerationLabelOnOutputsSecret {
			labelsToOmit = append(labelsToOmit, tofuLabelPrefix+"generation")
		}
		if err := r.createSecret(ctx, tofu, runOpts.outputsSecretName, runOpts.namespace, map[string][]byte{}, false, labelsToOmit, runOpts); err != nil {
			return err
		}

	} else {
		lookupKey := types.NamespacedName{
			Name:      runOpts.prefixedName,
			Namespace: runOpts.namespace,
		}

		if found, err := r.resourceExists(ctx, lookupKey, &corev1.PersistentVolumeClaim{}); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("could not find PersistentVolumeClaim '%s'", lookupKey)
		}

		lookupVersionedKey := types.NamespacedName{
			Name:      runOpts.versionedName,
			Namespace: runOpts.namespace,
		}

		if found, err := r.resourceExists(ctx, lookupVersionedKey, &corev1.ConfigMap{}); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("could not find ConfigMap '%s'", lookupVersionedKey)
		}

		if found, err := r.resourceExists(ctx, lookupVersionedKey, &corev1.Secret{}); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("could not find Secret '%s'", lookupVersionedKey)
		}

		if found, err := r.resourceExists(ctx, lookupVersionedKey, &rbacv1.RoleBinding{}); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("could not find RoleBinding '%s'", lookupVersionedKey)
		}

		if found, err := r.resourceExists(ctx, lookupVersionedKey, &rbacv1.Role{}); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("could not find Role '%s'", lookupVersionedKey)
		}

		serviceAccountLookupKey := types.NamespacedName{
			Name:      runOpts.serviceAccount,
			Namespace: runOpts.namespace,
		}
		if found, err := r.resourceExists(ctx, serviceAccountLookupKey, &corev1.ServiceAccount{}); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("could not find ServiceAccount '%s'", serviceAccountLookupKey)
		}
	}

	if err := r.createPod(ctx, tofu, runOpts); err != nil {
		return err
	}

	return nil
}

func (r ReconcileTofu) createGitAskpass(ctx context.Context, tokenSecret tfv1beta1.TokenSecretRef) ([]byte, error) {
	secret, err := r.loadSecret(ctx, tokenSecret.Name, tokenSecret.Namespace)
	if err != nil {
		return []byte{}, err
	}
	if key, ok := secret.Data[tokenSecret.Key]; !ok {
		return []byte{}, fmt.Errorf("secret '%s' did not contain '%s'", secret.Name, key)
	}
	s := heredoc.Docf(`
#!/bin/sh
exec echo "%s"
`, secret.Data[tokenSecret.Key])
	gitAskpass := []byte(s)
	return gitAskpass, nil
}

func (r ReconcileTofu) loadSecret(ctx context.Context, name, namespace string) (*corev1.Secret, error) {
	if namespace == "" {
		namespace = "default"
	}
	lookupKey := types.NamespacedName{Name: name, Namespace: namespace}
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, lookupKey, secret)
	if err != nil {
		return secret, err
	}
	return secret, nil
}

func (r ReconcileTofu) cacheNodeSelectors(ctx context.Context, logger logr.Logger) error {
	var affinity *corev1.Affinity
	var tolerations []corev1.Toleration
	var nodeSelector map[string]string
	if !r.InheritAffinity && !r.InheritNodeSelector && !r.InheritTolerations {
		return nil
	}
	foundAll := true
	_, found := r.Cache.Get(r.AffinityCacheKey)
	if r.InheritAffinity && !found {
		foundAll = false
	}
	_, found = r.Cache.Get(r.NodeSelectorCacheKey)
	if r.InheritNodeSelector && !found {
		foundAll = false
	}
	_, found = r.Cache.Get(r.TolerationsCacheKey)
	if r.InheritTolerations && !found {
		foundAll = false
	}
	if foundAll {
		return nil
	}
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		logger.Info("POD_NAMESPACE not found but required to get node selectors configs")
		return nil
	}
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		logger.Info("POD_NAME not found but required to get node selectors configs")
		return nil
	}
	podNamespacedName := types.NamespacedName{Namespace: podNamespace, Name: podName}
	pod := corev1.Pod{}
	err := r.Client.Get(ctx, podNamespacedName, &pod)
	if err != nil {
		logger.Info(fmt.Sprintf("Could not get pod '%s'", podNamespacedName.String()))
		return nil
	}
	if len(pod.ObjectMeta.OwnerReferences) != 1 {
		logger.Info(fmt.Sprintf("unexpected ownership for pod '%s'", podNamespacedName.String()))
		return nil
	}
	if pod.ObjectMeta.OwnerReferences[0].Kind != "ReplicaSet" {
		logger.Info(fmt.Sprintf("unexpected ownership kind for pod '%s'", podNamespacedName.String()))
		return nil
	}

	replicaSetName := pod.ObjectMeta.OwnerReferences[0].Name
	replicaSetNamespacedName := types.NamespacedName{Namespace: podNamespace, Name: replicaSetName}
	replicaSet := appsv1.ReplicaSet{}
	err = r.Client.Get(ctx, replicaSetNamespacedName, &replicaSet)
	if err != nil {
		logger.Info(fmt.Sprintf("Could not get replicaset '%s'", replicaSetNamespacedName.String()))
		return nil
	}
	if len(replicaSet.ObjectMeta.OwnerReferences) != 1 {
		logger.Info(fmt.Sprintf("unexpected ownership for replicaSet '%s'", replicaSetNamespacedName.String()))
		return nil
	}
	if replicaSet.ObjectMeta.OwnerReferences[0].Kind != "Deployment" {
		logger.Info(fmt.Sprintf("unexpected ownership kind for replicaSet '%s'", replicaSetNamespacedName.String()))
		return nil
	}

	deploymentName := replicaSet.ObjectMeta.OwnerReferences[0].Name
	deploymentNamespacedName := types.NamespacedName{Namespace: podNamespace, Name: deploymentName}
	deployment := appsv1.Deployment{}
	err = r.Client.Get(ctx, deploymentNamespacedName, &deployment)
	if err != nil {
		logger.Info(fmt.Sprintf("Could not get deployment '%s'", deploymentNamespacedName.String()))
		return nil
	}

	affinity = deployment.Spec.Template.Spec.Affinity
	tolerations = deployment.Spec.Template.Spec.Tolerations
	nodeSelector = deployment.Spec.Template.Spec.NodeSelector

	if r.InheritAffinity {
		r.Cache.Set(r.AffinityCacheKey, affinity, localcache.NoExpiration)
	}

	if r.InheritNodeSelector {
		r.Cache.Set(r.NodeSelectorCacheKey, nodeSelector, localcache.NoExpiration)
	}

	if r.InheritTolerations {
		r.Cache.Set(r.TolerationsCacheKey, tolerations, localcache.NoExpiration)
	}

	return nil
}
