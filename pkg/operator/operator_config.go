// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	monitoringv1 "github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator/apis/monitoring/v1"
	"github.com/go-logr/logr"
	promcommonconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	prommodel "github.com/prometheus/common/model"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	discoverykube "github.com/prometheus/prometheus/discovery/kubernetes"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	yaml "gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Base resource names which may be used for multiple different resource kinds
// related to the given component.
const (
	NameOperatorConfig = "config"
	NameRuleEvaluator  = "rule-evaluator"
	NameCollector      = "collector"
	NameAlertmanager   = "alertmanager"
)

const (
	RulesSecretName              = "rules"
	CollectionSecretName         = "collection"
	AlertmanagerSecretName       = "alertmanager"
	AlertmanagerPublicSecretName = "alertmanager"
	AlertmanagerPublicSecretKey  = "alertmanager.yaml"
	rulesDir                     = "/etc/rules"
	secretsDir                   = "/etc/secrets"
	alertmanagerConfigKey        = "config.yaml"
)

// Collector Kubernetes Deployment extraction/detection.
const (
	CollectorPrometheusContainerName         = "prometheus"
	CollectorPrometheusContainerPortName     = "prom-metrics"
	CollectorConfigReloaderContainerPortName = "cfg-rel-metrics"
)

var alertmanagerNoOpConfig = `
receivers:
  - name: "noop"
route:
  receiver: "noop"
`

func rulesLabels() map[string]string {
	return map[string]string{
		LabelAppName:      NameRuleEvaluator,
		KubernetesAppName: RuleEvaluatorAppName,
	}
}

func alertmanagerLabels() map[string]string {
	return map[string]string{
		LabelAppName:      NameAlertmanager,
		KubernetesAppName: AlertmanagerAppName,
	}
}

func componentAnnotations() map[string]string {
	return map[string]string{
		AnnotationMetricName: componentName,
		// Allow cluster autoscaler to evict evaluator Pods even though the Pods
		// have an emptyDir volume mounted. This is okay since the node where the
		// Pod runs will be scaled down.
		ClusterAutoscalerSafeEvictionLabel: "true",
	}
}

// setupOperatorConfigControllers ensures a rule-evaluator
// deployment as part of managed collection.
func setupOperatorConfigControllers(op *Operator) error {
	// The singleton OperatorConfig is the request object we reconcile against.
	objRequest := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: op.opts.PublicNamespace,
			Name:      NameOperatorConfig,
		},
	}
	// Default OperatorConfig filter.
	objFilterOperatorConfig := namespacedNamePredicate{
		namespace: op.opts.PublicNamespace,
		name:      NameOperatorConfig,
	}
	// Rule-evaluator deployment filter.
	objFilterRuleEvaluator := namespacedNamePredicate{
		namespace: op.opts.OperatorNamespace,
		name:      NameRuleEvaluator,
	}
	// Rule-evaluator secret filter.
	objFilterRuleEvaluatorSecret := namespacedNamePredicate{
		namespace: op.opts.OperatorNamespace,
		name:      RulesSecretName,
	}
	// Rule-evaluator secret filter.
	objFilterAlertManagerSecret := namespacedNamePredicate{
		namespace: op.opts.OperatorNamespace,
		name:      AlertmanagerSecretName,
	}

	// Reconcile operator-managed resources.
	err := ctrl.NewControllerManagedBy(op.manager).
		Named("operator-config").
		// Filter events without changes for all watches.
		WithEventFilter(predicate.ResourceVersionChangedPredicate{}).
		For(
			&monitoringv1.OperatorConfig{},
			builder.WithPredicates(objFilterOperatorConfig),
		).
		Watches(
			&source.Kind{Type: &appsv1.Deployment{}},
			enqueueConst(objRequest),
			builder.WithPredicates(
				objFilterRuleEvaluator,
				predicate.GenerationChangedPredicate{},
			)).
		Watches(
			source.NewKindWithCache(&corev1.Secret{}, op.managedNamespacesCache),
			enqueueConst(objRequest),
			builder.WithPredicates(predicate.NewPredicateFuncs(secretFilter(op.opts.PublicNamespace))),
		).
		// Detect and undo changes to the secret.
		Watches(
			source.NewKindWithCache(&corev1.Secret{}, op.managedNamespacesCache),
			enqueueConst(objRequest),
			builder.WithPredicates(objFilterRuleEvaluatorSecret)).
		Watches(
			source.NewKindWithCache(&corev1.Secret{}, op.managedNamespacesCache),
			enqueueConst(objRequest),
			builder.WithPredicates(objFilterAlertManagerSecret)).
		Complete(newOperatorConfigReconciler(op.manager.GetClient(), op.opts))

	if err != nil {
		return fmt.Errorf("operator-config controller: %w", err)
	}
	return nil
}

// secretFilter filters by non-default Secrets in specified namespace.
func secretFilter(ns string) func(object client.Object) bool {
	return func(object client.Object) bool {
		if object.GetNamespace() == ns {
			return !strings.HasPrefix(object.GetName(), "default-token")
		}
		return false
	}
}

// operatorConfigReconciler reconciles the OperatorConfig CRD.
type operatorConfigReconciler struct {
	client client.Client
	opts   Options
}

// newOperatorConfigReconciler creates a new operatorConfigReconciler.
func newOperatorConfigReconciler(c client.Client, opts Options) *operatorConfigReconciler {
	return &operatorConfigReconciler{
		client: c,
		opts:   opts,
	}
}

// Reconcile ensures the OperatorConfig resource is reconciled.
func (r *operatorConfigReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger, _ := logr.FromContext(ctx)
	logger.WithValues("operatorconfig", req.NamespacedName).Info("reconciling operatorconfig")

	config := &monitoringv1.OperatorConfig{}

	// Fetch OperatorConfig.
	if err := r.client.Get(ctx, req.NamespacedName, config); apierrors.IsNotFound(err) {
		logger.Info("no operatorconfig created yet")
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("get operatorconfig for incoming: %q: %w", req.String(), err)
	}
	// Ensure the rule-evaluator config and grab any to-be-mirrored
	// secret data on the way.
	secretData, err := r.ensureRuleEvaluatorConfig(ctx, &config.Rules)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("ensure rule-evaluator config: %w", err)
	}

	if err := r.ensureAlertmanagerConfigSecret(ctx, config.ManagedAlertmanager); err != nil {
		return reconcile.Result{}, fmt.Errorf("ensure alertmanager config secret: %w", err)
	}

	// Mirror the fetched secret data to where the rule-evaluator can
	// mount and access.
	if err := r.ensureRuleEvaluatorSecrets(ctx, secretData); err != nil {
		return reconcile.Result{}, fmt.Errorf("ensure rule-evaluator secrets: %w", err)
	}

	// Ensure the rule-evaluator deployment and volume mounts.
	if err := r.ensureRuleEvaluatorDeployment(ctx, &config.Rules); err != nil {
		return reconcile.Result{}, fmt.Errorf("ensure rule-evaluator deploy: %w", err)
	}

	return reconcile.Result{}, nil
}

// ensureRuleEvaluatorConfig reconciles the config for rule-evaluator.
func (r *operatorConfigReconciler) ensureRuleEvaluatorConfig(ctx context.Context, spec *monitoringv1.RuleEvaluatorSpec) (map[string][]byte, error) {
	cfg, secretData, err := r.makeRuleEvaluatorConfig(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("make rule-evaluator configmap: %w", err)
	}

	// Upsert rule-evaluator config.
	if err := r.client.Update(ctx, cfg); apierrors.IsNotFound(err) {
		if err := r.client.Create(ctx, cfg); err != nil {
			return nil, fmt.Errorf("create rule-evaluator config: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("update rule-evaluator config: %w", err)
	}
	return secretData, nil
}

// makeRuleEvaluatorConfig creates the config for rule-evaluator.
// This is stored as a Secret rather than a ConfigMap as it could contain
// sensitive configuration information.
func (r *operatorConfigReconciler) makeRuleEvaluatorConfig(ctx context.Context, spec *monitoringv1.RuleEvaluatorSpec) (*corev1.ConfigMap, map[string][]byte, error) {
	amConfigs, secretData, err := r.makeAlertmanagerConfigs(ctx, &spec.Alerting)
	if err != nil {
		return nil, nil, fmt.Errorf("make alertmanager config: %w", err)
	}
	if spec.Credentials != nil {
		p := pathForSelector(r.opts.PublicNamespace, &monitoringv1.SecretOrConfigMap{Secret: spec.Credentials})
		b, err := getSecretKeyBytes(ctx, r.client, r.opts.PublicNamespace, spec.Credentials)
		if err != nil {
			return nil, nil, fmt.Errorf("get service account credentials: %w", err)
		}
		secretData[p] = b
	}

	cfg := &promconfig.Config{
		GlobalConfig: promconfig.GlobalConfig{
			ExternalLabels: labels.FromMap(spec.ExternalLabels),
		},
		AlertingConfig: promconfig.AlertingConfig{
			AlertmanagerConfigs: amConfigs,
		},
		RuleFiles: []string{path.Join(rulesDir, "*.yaml")},
	}
	cfgEncoded, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal Prometheus config: %w", err)
	}

	// Create rule-evaluator Secret.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameRuleEvaluator,
			Namespace: r.opts.OperatorNamespace,
		},
		Data: map[string]string{
			configFilename: string(cfgEncoded),
		},
	}
	return cm, secretData, nil
}

// ensureRuleEvaluatorSecrets reconciles the Secrets for rule-evaluator.
func (r *operatorConfigReconciler) ensureRuleEvaluatorSecrets(ctx context.Context, data map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        RulesSecretName,
			Namespace:   r.opts.OperatorNamespace,
			Annotations: componentAnnotations(),
			Labels:      rulesLabels(),
		},
		Data: make(map[string][]byte),
	}
	for f, b := range data {
		secret.Data[f] = b
	}

	if err := r.client.Update(ctx, secret); apierrors.IsNotFound(err) {
		if err := r.client.Create(ctx, secret); err != nil {
			return fmt.Errorf("create rule-evaluator secrets: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("update rule-evaluator secrets: %w", err)
	}
	return nil
}

// ensureAlertmanagerConfigSecret copies the managed Alertmanager config secret from gmp-public
func (r *operatorConfigReconciler) ensureAlertmanagerConfigSecret(ctx context.Context, spec *monitoringv1.ManagedAlertmanagerSpec) error {
	logger, _ := logr.FromContext(ctx)
	pubNamespace := r.opts.PublicNamespace

	// This is the default, no-op secret config. If we find a user-defined config,
	// we will overwrite the default data with the user's data.
	// If we don't find a user config, we will still proceed with ensuring this
	// default secret exists (so that the alertmanager pod doesn't crash due to no
	// config found). This flow also handles user deletion/disabling of managed AM.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        AlertmanagerSecretName,
			Namespace:   r.opts.OperatorNamespace,
			Annotations: componentAnnotations(),
			Labels:      alertmanagerLabels(),
		},
		Data: map[string][]byte{alertmanagerConfigKey: []byte(alertmanagerNoOpConfig)},
	}

	// Set defaults on public namespace secret.
	var sel = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: AlertmanagerPublicSecretName,
		},
		Key: AlertmanagerPublicSecretKey,
	}
	// Overwrite defaults if specified.
	if spec != nil && spec.ConfigSecret != nil {
		sel.Name = spec.ConfigSecret.Name
		sel.Key = spec.ConfigSecret.Key
	}

	// Try and read the secret for use.
	b, err := getSecretKeyBytes(ctx, r.client, pubNamespace, sel)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		// If the config secret is not found, it may have been manually deleted
		// (ie, to disable managed AM), so we will continue with restoring the no-op config
		// so that the managed AM pod doesn't crash loop.
		logger.Info(fmt.Sprintf("alertmanager config secret not found in namespace %s: %s", pubNamespace, err.Error()))
	} else {
		secret.Data[alertmanagerConfigKey] = b
	}

	if err := r.client.Update(ctx, secret); apierrors.IsNotFound(err) {
		if err := r.client.Create(ctx, secret); err != nil {
			return fmt.Errorf("create alertmanager config secret: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("update alertmanager config secret: %w", err)
	}

	return nil
}

// ensureRuleEvaluatorDeployment reconciles the Deployment for rule-evaluator.
func (r *operatorConfigReconciler) ensureRuleEvaluatorDeployment(ctx context.Context, spec *monitoringv1.RuleEvaluatorSpec) error {
	logger, _ := logr.FromContext(ctx)

	var deploy appsv1.Deployment
	err := r.client.Get(ctx, client.ObjectKey{Namespace: r.opts.OperatorNamespace, Name: NameRuleEvaluator}, &deploy)
	// Some users deliberately not want to run the rule-evaluator. Only emit a warning but don't cause
	// retries as this logic gets re-triggered anyway if the Deployment is created later.
	if apierrors.IsNotFound(err) {
		logger.Error(err, "rule-evaluator Deployment does not exist")
		return nil
	}
	if err != nil {
		return err
	}

	var projectID, location, cluster = resolveLabels(r.opts, spec.ExternalLabels)

	flags := []string{
		fmt.Sprintf("--export.label.project-id=%q", projectID),
		fmt.Sprintf("--export.label.location=%q", location),
		fmt.Sprintf("--export.label.cluster=%q", cluster),
	}
	// If no explicit project ID is set, use the one provided to the operator.
	// On GKE the rule-evaluator can also auto-detect the cluster's project
	// but this won't work in other Kubernetes environments.
	queryProjectID := projectID
	if spec.QueryProjectID != "" {
		queryProjectID = spec.QueryProjectID
	}
	flags = append(flags, fmt.Sprintf("--query.project-id=%q", queryProjectID))

	if spec.Credentials != nil {
		p := path.Join(secretsDir, pathForSelector(r.opts.PublicNamespace, &monitoringv1.SecretOrConfigMap{Secret: spec.Credentials}))
		flags = append(flags, fmt.Sprintf("--export.credentials-file=%q", p))
		flags = append(flags, fmt.Sprintf("--query.credentials-file=%q", p))
	}
	if spec.GeneratorURL != "" {
		flags = append(flags, fmt.Sprintf("--query.generator-url=%q", spec.GeneratorURL))
	}

	// Set EXTRA_ARGS envvar in evaluator container.
	for i, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name != "evaluator" {
			continue
		}
		var repl []corev1.EnvVar

		for _, ev := range c.Env {
			if ev.Name != "EXTRA_ARGS" {
				repl = append(repl, ev)
			}
		}
		repl = append(repl, corev1.EnvVar{Name: "EXTRA_ARGS", Value: strings.Join(flags, " ")})

		deploy.Spec.Template.Spec.Containers[i].Env = repl
	}

	// Upsert rule-evaluator Deployment.
	return r.client.Update(ctx, &deploy)
}

// makeAlertmanagerConfigs creates the alertmanager_config entries as described in
// https://prometheus.io/docs/prometheus/latest/configuration/configuration/#alertmanager_config.
func (r *operatorConfigReconciler) makeAlertmanagerConfigs(ctx context.Context, spec *monitoringv1.AlertingSpec) (promconfig.AlertmanagerConfigs, map[string][]byte, error) {
	var (
		err        error
		configs    promconfig.AlertmanagerConfigs
		secretData = make(map[string][]byte)
	)

	amNamespacedName := types.NamespacedName{
		Namespace: r.opts.OperatorNamespace,
		Name:      NameAlertmanager,
	}
	// If the default Alertmanager exists, append it to the list of spec.Alertmanagers.
	var amSvc corev1.Service
	if resourceErr := r.client.Get(ctx, amNamespacedName, &amSvc); resourceErr == nil {
		// Alertmanager service should have one port defined, otherwise ignore.
		if ports := amSvc.Spec.Ports; len(ports) > 0 {
			// Assume first port on service is the correct endpoint.
			port := ports[0].Port
			svcDNSName := fmt.Sprintf("%s.%s:%d", amSvc.Name, amSvc.Namespace, port)
			cfg := promconfig.DefaultAlertmanagerConfig
			cfg.ServiceDiscoveryConfigs = discovery.Configs{
				discovery.StaticConfig{
					&targetgroup.Group{
						Targets: []model.LabelSet{{model.AddressLabel: model.LabelValue(svcDNSName)}},
					},
				},
			}
			configs = append(configs, &cfg)
		}
	}

	for _, am := range spec.Alertmanagers {
		// The upstream struct is lacking the omitempty field on the API version. Thus it looks
		// like we explicitly set it to empty (invalid) even if left empty after marshalling.
		// Thus we initialize the config with defaulting. Similar applies for the embedded HTTPConfig.
		cfg := promconfig.DefaultAlertmanagerConfig

		if am.PathPrefix != "" {
			cfg.PathPrefix = am.PathPrefix
		}
		if am.Scheme != "" {
			cfg.Scheme = am.Scheme
		}
		if am.APIVersion != "" {
			cfg.APIVersion = promconfig.AlertmanagerAPIVersion(am.APIVersion)
		}

		// Timeout, APIVersion, PathPrefix, and Scheme all resort to defaults if left unspecified.
		if am.Timeout != "" {
			cfg.Timeout, err = prommodel.ParseDuration(am.Timeout)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid timeout: %w", err)
			}
		}
		// Authorization.
		if am.Authorization != nil {
			cfg.HTTPClientConfig.Authorization = &promcommonconfig.Authorization{
				Type: am.Authorization.Type,
			}
			if c := am.Authorization.Credentials; c != nil {
				b, err := getSecretKeyBytes(ctx, r.client, r.opts.PublicNamespace, c)
				if err != nil {
					return nil, nil, err
				}
				p := pathForSelector(r.opts.PublicNamespace, &monitoringv1.SecretOrConfigMap{Secret: c})

				secretData[p] = b
				cfg.HTTPClientConfig.Authorization.CredentialsFile = path.Join(secretsDir, p)
			}
		}
		// TLS config.
		if am.TLS != nil {
			tlsCfg := promcommonconfig.TLSConfig{
				InsecureSkipVerify: am.TLS.InsecureSkipVerify,
				ServerName:         am.TLS.ServerName,
			}
			if am.TLS.CA != nil {
				p := pathForSelector(r.opts.PublicNamespace, am.TLS.CA)
				b, err := getSecretOrConfigMapBytes(ctx, r.client, r.opts.PublicNamespace, am.TLS.CA)
				if err != nil {
					return nil, nil, err
				}
				secretData[p] = b
				tlsCfg.CAFile = path.Join(secretsDir, p)
			}
			if am.TLS.Cert != nil {
				p := pathForSelector(r.opts.PublicNamespace, am.TLS.Cert)
				b, err := getSecretOrConfigMapBytes(ctx, r.client, r.opts.PublicNamespace, am.TLS.Cert)
				if err != nil {
					return nil, nil, err
				}
				secretData[p] = b
				tlsCfg.CertFile = path.Join(secretsDir, p)
			}
			if am.TLS.KeySecret != nil {
				p := pathForSelector(r.opts.PublicNamespace, &monitoringv1.SecretOrConfigMap{Secret: am.TLS.KeySecret})
				b, err := getSecretKeyBytes(ctx, r.client, r.opts.PublicNamespace, am.TLS.KeySecret)
				if err != nil {
					return nil, nil, err
				}
				secretData[p] = b
				tlsCfg.KeyFile = path.Join(secretsDir, p)
			}

			cfg.HTTPClientConfig.TLSConfig = tlsCfg
		}

		// Configure discovery of AM endpoints via Kubernetes API.
		cfg.ServiceDiscoveryConfigs = discovery.Configs{
			&discoverykube.SDConfig{
				// Must instantiate a default client config explicitly as the follow_redirects
				// field lacks the omitempty tag. Thus it looks like we explicitly set it to false
				// even if left empty after marshalling.
				HTTPClientConfig: promcommonconfig.DefaultHTTPClientConfig,
				Role:             discoverykube.RoleEndpoint,
				NamespaceDiscovery: discoverykube.NamespaceDiscovery{
					Names: []string{am.Namespace},
				},
			},
		}
		svcNameRE, err := relabel.NewRegexp(am.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot build regex from service name %q: %w", am.Name, err)
		}
		cfg.RelabelConfigs = append(cfg.RelabelConfigs, &relabel.Config{
			Action:       relabel.Keep,
			SourceLabels: prommodel.LabelNames{"__meta_kubernetes_endpoints_name"},
			Regex:        svcNameRE,
		})
		if am.Port.StrVal != "" {
			re, err := relabel.NewRegexp(am.Port.String())
			if err != nil {
				return nil, nil, fmt.Errorf("cannot build regex from port %q: %w", am.Port, err)
			}
			cfg.RelabelConfigs = append(cfg.RelabelConfigs, &relabel.Config{
				Action:       relabel.Keep,
				SourceLabels: prommodel.LabelNames{"__meta_kubernetes_endpoint_port_name"},
				Regex:        re,
			})
		} else if am.Port.IntVal != 0 {
			// The endpoints object does not provide a meta label for the port number. If the endpoint
			// is backed by a pod we can inspect the pod port number label, but to make it work in general
			// we simply override the port in the address label.
			// If the endpoints has multiple ports, this will create duplicate targets but they will be
			// deduplicated by the discovery engine.
			re, err := relabel.NewRegexp(`(.+):\d+`)
			if err != nil {
				return nil, nil, fmt.Errorf("building address regex failed: %w", err)
			}
			cfg.RelabelConfigs = append(cfg.RelabelConfigs, &relabel.Config{
				Action:       relabel.Replace,
				SourceLabels: prommodel.LabelNames{"__address__"},
				Regex:        re,
				TargetLabel:  "__address__",
				Replacement:  fmt.Sprintf("$1:%d", am.Port.IntVal),
			})
		}

		// TODO(pintohutch): add support for basic_auth, oauth2, proxy_url, follow_redirects.

		// Append to alertmanagers config array.
		configs = append(configs, &cfg)
	}

	return configs, secretData, nil
}

// getSecretOrConfigMapBytes is a helper function to conditionally fetch
// the secret or configmap selector payloads.
func getSecretOrConfigMapBytes(ctx context.Context, kClient client.Reader, namespace string, scm *monitoringv1.SecretOrConfigMap) ([]byte, error) {
	var (
		b   []byte
		err error
	)
	if secret := scm.Secret; secret != nil {
		b, err = getSecretKeyBytes(ctx, kClient, namespace, secret)
		if err != nil {
			return b, err
		}
	} else if cm := scm.ConfigMap; cm != nil {
		b, err = getConfigMapKeyBytes(ctx, kClient, namespace, cm)
		if err != nil {
			return b, err
		}
	}
	return b, nil
}

// getSecretKeyBytes processes the given NamespacedSecretKeySelector and returns the referenced data.
func getSecretKeyBytes(ctx context.Context, kClient client.Reader, namespace string, sel *corev1.SecretKeySelector) ([]byte, error) {
	var (
		secret = &corev1.Secret{}
		nn     = types.NamespacedName{
			Namespace: namespace,
			Name:      sel.Name,
		}
		bytes []byte
	)
	err := kClient.Get(ctx, nn, secret)
	if err != nil {
		return bytes, fmt.Errorf("unable to get secret %q: %w", sel.Name, err)
	}
	bytes, ok := secret.Data[sel.Key]
	if !ok {
		return bytes, fmt.Errorf("key %q in secret %q not found", sel.Key, sel.Name)
	}

	return bytes, nil
}

// getConfigMapKeyBytes processes the given NamespacedConfigMapKeySelector and returns the referenced data.
func getConfigMapKeyBytes(ctx context.Context, kClient client.Reader, namespace string, sel *corev1.ConfigMapKeySelector) ([]byte, error) {
	var (
		cm = &corev1.ConfigMap{}
		nn = types.NamespacedName{
			Namespace: namespace,
			Name:      sel.Name,
		}
		b []byte
	)
	err := kClient.Get(ctx, nn, cm)
	if err != nil {
		return b, fmt.Errorf("unable to get secret %q: %w", sel.Name, err)
	}
	// Check 'data' first, then 'binaryData'.
	if s, ok := cm.Data[sel.Key]; ok {
		return []byte(s), nil
	} else if b, ok := cm.BinaryData[sel.Key]; ok {
		return b, nil
	} else {
		return b, fmt.Errorf("key %q in secret %q not found", sel.Key, sel.Name)
	}
}

// pathForSelector cretes the filepath for the provided NamespacedSecretOrConfigMap.
// This can be used to avoid naming collisions of like-keys across K8s resources.
func pathForSelector(namespace string, scm *monitoringv1.SecretOrConfigMap) string {
	if scm == nil {
		return ""
	}
	if scm.ConfigMap != nil {
		return fmt.Sprintf("%s_%s_%s_%s", "configmap", namespace, scm.ConfigMap.Name, scm.ConfigMap.Key)
	}
	if scm.Secret != nil {
		return fmt.Sprintf("%s_%s_%s_%s", "secret", namespace, scm.Secret.Name, scm.Secret.Key)
	}
	return ""
}

func validateRules(rules *monitoringv1.RuleEvaluatorSpec) error {
	if rules.GeneratorURL != "" {
		if _, err := url.Parse(rules.GeneratorURL); err != nil {
			return fmt.Errorf("failed to parse generator URL: %w", err)
		}
	}

	if err := validateSecretKeySelector(rules.Credentials); err != nil {
		return fmt.Errorf("invalid credentials: %w", err)
	}
	for i, alertManagerEndpoint := range rules.Alerting.Alertmanagers {
		if err := validateAlertManagerEndpoint(&alertManagerEndpoint); err != nil {
			return fmt.Errorf("invalid alert manager endpoint `%s` (index %d): %w", alertManagerEndpoint.Name, i, err)
		}
	}
	return nil
}

func validateAlertManagerEndpoint(alertManagerEndpoint *monitoringv1.AlertmanagerEndpoints) error {
	if alertManagerEndpoint.Authorization != nil {
		if err := validateSecretKeySelector(alertManagerEndpoint.Authorization.Credentials); err != nil {
			return fmt.Errorf("invalid authorization credentials: %w", err)
		}
	}
	if alertManagerEndpoint.TLS != nil {
		if err := validateSecretKeySelector(alertManagerEndpoint.TLS.KeySecret); err != nil {
			return fmt.Errorf("invalid TLS key: %w", err)
		}
		if err := validateSecretOrConfigMap(alertManagerEndpoint.TLS.CA); err != nil {
			return fmt.Errorf("invalid TLS CA: %w", err)
		}
		if err := validateSecretOrConfigMap(alertManagerEndpoint.TLS.Cert); err != nil {
			return fmt.Errorf("invalid TLS Cert: %w", err)
		}
	}
	return nil
}

func validateSecretKeySelector(secretKeySelector *corev1.SecretKeySelector) error {
	if secretKeySelector == nil {
		return nil
	}
	if secretKeySelector.LocalObjectReference.Name == "" {
		return errors.New("missing secret key selector name")
	}
	return nil
}

func validateSecretOrConfigMap(secretOrConfigMap *monitoringv1.SecretOrConfigMap) error {
	if secretOrConfigMap == nil {
		return nil
	}
	if secretOrConfigMap.Secret != nil {
		if err := validateSecretKeySelector(secretOrConfigMap.Secret); err != nil {
			return err
		}
		if secretOrConfigMap.ConfigMap != nil {
			return errors.New("SecretOrConfigMap fields are mutually exclusive")
		}
	}
	return nil
}

type operatorConfigValidator struct {
	namespace string
}

func (v *operatorConfigValidator) ValidateCreate(ctx context.Context, o runtime.Object) error {
	oc := o.(*monitoringv1.OperatorConfig)

	if oc.Namespace != v.namespace || oc.Name != NameOperatorConfig {
		return fmt.Errorf("OperatorConfig must be in namespace %q with name %q", v.namespace, NameOperatorConfig)
	}
	if _, err := makeKubeletScrapeConfigs(oc.Collection.KubeletScraping); err != nil {
		return fmt.Errorf("failed to create kubelet scrape config: %w", err)
	}

	if err := validateSecretKeySelector(oc.Collection.Credentials); err != nil {
		return fmt.Errorf("invalid collection credentials: %w", err)
	}
	if oc.ManagedAlertmanager != nil {
		if err := validateSecretKeySelector(oc.ManagedAlertmanager.ConfigSecret); err != nil {
			return fmt.Errorf("invalid managed alert manager config secret: %w", err)
		}
	}
	if err := validateRules(&oc.Rules); err != nil {
		return fmt.Errorf("invalid rules config: %w", err)
	}
	return nil
}

func (v *operatorConfigValidator) ValidateUpdate(ctx context.Context, _, o runtime.Object) error {
	return v.ValidateCreate(ctx, o)
}

func (v *operatorConfigValidator) ValidateDelete(ctx context.Context, o runtime.Object) error {
	return nil
}
