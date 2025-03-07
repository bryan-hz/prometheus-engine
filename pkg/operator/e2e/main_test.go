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

// Package e2e contains tests that validate the behavior of gmp-operator against a cluster.
// To make tests simple and fast, the test suite runs the operator internally. The CRDs
// are expected to be installed out of band (along with the operator deployment itself in
// a real world setup).
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	gcm "cloud.google.com/go/monitoring/apiv3/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"google.golang.org/api/iterator"
	gcmpb "google.golang.org/genproto/googleapis/monitoring/v3"
	"google.golang.org/protobuf/types/known/timestamppb"
	arv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/cert"
	ctrl "sigs.k8s.io/controller-runtime"
	kyaml "sigs.k8s.io/yaml"

	// Blank import required to register GCP auth handlers to talk to GKE clusters.
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	"github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator"
	monitoringv1 "github.com/GoogleCloudPlatform/prometheus-engine/pkg/operator/apis/monitoring/v1"
)

var (
	kubeconfig        *rest.Config
	projectID         string
	cluster           string
	location          string
	skipGCM           bool
	gcpServiceAccount string
)

func TestMain(m *testing.M) {
	flag.StringVar(&projectID, "project-id", "", "The GCP project to write metrics to.")
	flag.StringVar(&cluster, "cluster", "", "The name of the Kubernetes cluster that's tested against.")
	flag.StringVar(&location, "location", "", "The location of the Kubernetes cluster that's tested against.")
	flag.BoolVar(&skipGCM, "skip-gcm", false, "Skip validating GCM ingested points.")
	flag.StringVar(&gcpServiceAccount, "gcp-service-account", "", "Path to GCP service account file for usage by deployed containers.")

	flag.Parse()

	var err error
	kubeconfig, err = ctrl.GetConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Loading kubeconfig failed:", err)
		os.Exit(1)
	}

	go func() {
		os.Exit(m.Run())
	}()

	// If the process gets terminated by the user, the Go test package
	// doesn't ensure that test cleanup functions are run.
	// Deleting all namespaces ensures we don't leave anything behind regardless.
	// Non-namespaced resources are owned by a namespace and thus cleaned up
	// by Kubernetes' garbage collection.
	term := make(chan os.Signal, 1)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	<-term
	if err := cleanupAllNamespaces(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "Cleaning up namespaces failed:", err)
		os.Exit(1)
	}
}

func TestCollector(t *testing.T) {
	tctx := newTestContext(t)

	// We could simply verify that the full collection chain works once. But validating
	// more fine-grained stages makes debugging a lot easier.
	t.Run("deployed", tctx.subtest(testCollectorDeployed))
	t.Run("self-podmonitoring", tctx.subtest(testCollectorSelfPodMonitoring))
	t.Run("self-clusterpodmonitoring", tctx.subtest(testCollectorSelfClusterPodMonitoring))
	t.Run("scrape-kubelet", tctx.subtest(testCollectorScrapeKubelet))
}

func TestRuleEvaluation(t *testing.T) {
	tctx := newTestContext(t)

	cert, key, err := cert.GenerateSelfSignedCertKey("test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("rule evaluator create alertmanager secrets", tctx.subtest(func(ctx context.Context, t *testContext) {
		testCreateAlertmanagerSecrets(ctx, t, cert, key)
	}))
	t.Run("rule evaluator operatorconfig", tctx.subtest(testRuleEvaluatorOperatorConfig))
	t.Run("rule evaluator secrets", tctx.subtest(func(ctx context.Context, t *testContext) {
		testRuleEvaluatorSecrets(ctx, t, cert, key)
	}))
	t.Run("rule evaluator config", tctx.subtest(testRuleEvaluatorConfig))
	t.Run("rule generation", tctx.subtest(testRulesGeneration))
	t.Run("rule evaluator deploy", tctx.subtest(testRuleEvaluatorDeployment))

	if !skipGCM {
		t.Log("Waiting rule results to become readable")
		t.Run("check rule metrics", tctx.subtest(testValidateRuleEvaluationMetrics))
	}
}

func TestAlertmanagerDefault(t *testing.T) {
	tctx := newTestContext(t)

	alertmanagerConfig := `
receivers:
  - name: "foobar"
route:
  receiver: "foobar"
`
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: operator.AlertmanagerPublicSecretName},
		Data: map[string][]byte{
			operator.AlertmanagerPublicSecretKey: []byte(alertmanagerConfig),
		},
	}
	t.Run("deployed", tctx.subtest(testAlertmanagerDeployed(nil)))
	t.Run("config set", tctx.subtest(testAlertmanagerConfig(secret, operator.AlertmanagerPublicSecretKey)))
}

func TestAlertmanagerCustom(t *testing.T) {
	tctx := newTestContext(t)

	alertmanagerConfig := `
receivers:
  - name: "foobar"
route:
  receiver: "foobar"
`
	spec := &monitoringv1.ManagedAlertmanagerSpec{
		ConfigSecret: &v1.SecretKeySelector{
			LocalObjectReference: v1.LocalObjectReference{
				"my-secret-name",
			},
			Key: "my-secret-key",
		},
	}
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secret-name"},
		Data: map[string][]byte{
			"my-secret-key": []byte(alertmanagerConfig),
		},
	}
	t.Run("deployed", tctx.subtest(testAlertmanagerDeployed(spec)))
	t.Run("config set", tctx.subtest(testAlertmanagerConfig(secret, "my-secret-key")))
}

// testRuleEvaluatorOperatorConfig ensures an OperatorConfig can be deployed
// that contains rule-evaluator configuration.
func testRuleEvaluatorOperatorConfig(ctx context.Context, t *testContext) {
	// Setup TLS secret selectors.
	certSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{
			Name: "alertmanager-tls",
		},
		Key: "cert",
	}

	keySecret := certSecret.DeepCopy()
	keySecret.Key = "key"

	opCfg := &monitoringv1.OperatorConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: operator.NameOperatorConfig,
		},
		Rules: monitoringv1.RuleEvaluatorSpec{
			ExternalLabels: map[string]string{
				"external_key": "external_val",
			},
			QueryProjectID: projectID,
			Alerting: monitoringv1.AlertingSpec{
				Alertmanagers: []monitoringv1.AlertmanagerEndpoints{
					{
						Name:       "test-am",
						Namespace:  t.namespace,
						Port:       intstr.IntOrString{IntVal: 19093},
						Timeout:    "30s",
						APIVersion: "v2",
						PathPrefix: "/test",
						Scheme:     "https",
						Authorization: &monitoringv1.Authorization{
							Type: "Bearer",
							Credentials: &v1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "alertmanager-authorization",
								},
								Key: "token",
							},
						},
						TLS: &monitoringv1.TLSConfig{
							Cert: &monitoringv1.SecretOrConfigMap{
								Secret: certSecret,
							},
							KeySecret: keySecret,
						},
					},
				},
			},
		},
	}
	if gcpServiceAccount != "" {
		opCfg.Rules.Credentials = &v1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "user-gcp-service-account",
			},
			Key: "key.json",
		}
	}
	_, err := t.operatorClient.MonitoringV1().OperatorConfigs(t.pubNamespace).Create(ctx, opCfg, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create rules operatorconfig: %s", err)
	}
}

func testCreateAlertmanagerSecrets(ctx context.Context, t *testContext, cert, key []byte) {
	secrets := []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "alertmanager-authorization",
			},
			Data: map[string][]byte{
				"token": []byte("auth-bearer-password"),
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "alertmanager-tls",
			},
			Data: map[string][]byte{
				"cert": cert,
				"key":  key,
			},
		},
	}

	for _, s := range secrets {
		if _, err := t.kubeClient.CoreV1().Secrets(t.pubNamespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create alertmanager secret: %s", err)
		}
	}
}

func testRuleEvaluatorSecrets(ctx context.Context, t *testContext, cert, key []byte) {
	// Verify contents but without the GCP SA credentials file to not leak secrets in tests logs.
	// Whether the contents were copied correctly is implicitly verified by the credentials working.
	want := map[string][]byte{
		fmt.Sprintf("secret_%s_alertmanager-tls_cert", t.pubNamespace):            cert,
		fmt.Sprintf("secret_%s_alertmanager-tls_key", t.pubNamespace):             key,
		fmt.Sprintf("secret_%s_alertmanager-authorization_token", t.pubNamespace): []byte("auth-bearer-password"),
	}
	err := wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
		secret, err := t.kubeClient.CoreV1().Secrets(t.namespace).Get(ctx, operator.RulesSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, errors.Wrap(err, "get secret")
		}
		delete(secret.Data, fmt.Sprintf("secret_%s_user-gcp-service-account_key.json", t.pubNamespace))

		if diff := cmp.Diff(want, secret.Data); diff != "" {
			return false, errors.Errorf("unexpected configuration (-want, +got): %s", diff)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("failed waiting for generated rule-evaluator config: %s", err)
	}

}

func testRuleEvaluatorConfig(ctx context.Context, t *testContext) {
	replace := func(s string) string {
		return strings.NewReplacer(
			"{namespace}", t.namespace, "{pubNamespace}", t.pubNamespace,
		).Replace(s)
	}

	want := map[string]string{
		"config.yaml": replace(`global:
    external_labels:
        external_key: external_val
alerting:
    alertmanagers:
        - authorization:
            type: Bearer
            credentials_file: /etc/secrets/secret_{pubNamespace}_alertmanager-authorization_token
          tls_config:
            cert_file: /etc/secrets/secret_{pubNamespace}_alertmanager-tls_cert
            key_file: /etc/secrets/secret_{pubNamespace}_alertmanager-tls_key
            insecure_skip_verify: false
          follow_redirects: true
          enable_http2: true
          scheme: https
          path_prefix: /test
          timeout: 30s
          api_version: v2
          relabel_configs:
            - source_labels: [__meta_kubernetes_endpoints_name]
              regex: test-am
              action: keep
            - source_labels: [__address__]
              regex: (.+):\d+
              target_label: __address__
              replacement: $1:19093
              action: replace
          kubernetes_sd_configs:
            - role: endpoints
              kubeconfig_file: ""
              follow_redirects: true
              enable_http2: true
              namespaces:
                own_namespace: false
                names:
                    - {namespace}
        - follow_redirects: true
          enable_http2: true
          scheme: http
          timeout: 10s
          api_version: v2
          relabel_configs:
            - source_labels: [__meta_kubernetes_endpoints_name]
              regex: alertmanager
              action: keep
            - source_labels: [__address__]
              regex: (.+):\d+
              target_label: __address__
              replacement: $1:9093
              action: replace
          kubernetes_sd_configs:
            - role: endpoints
              kubeconfig_file: ""
              follow_redirects: true
              enable_http2: true
              namespaces:
                own_namespace: false
                names:
                    - {namespace}
rule_files:
    - /etc/rules/*.yaml
`),
	}
	err := wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
		cm, err := t.kubeClient.CoreV1().ConfigMaps(t.namespace).Get(ctx, "rule-evaluator", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, errors.Wrap(err, "get configmap")
		}
		if diff := cmp.Diff(want, cm.Data); diff != "" {
			return false, errors.Errorf("unexpected configuration (-want, +got): %s", diff)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("failed waiting for generated rule-evaluator config: %s", err)
	}

}

func testRuleEvaluatorDeployment(ctx context.Context, t *testContext) {
	err := wait.Poll(1*time.Second, 1*time.Minute, func() (bool, error) {
		deploy, err := t.kubeClient.AppsV1().Deployments(t.namespace).Get(ctx, "rule-evaluator", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, errors.Wrap(err, "get deployment")
		}
		// When not using GCM, we check the available replicas rather than ready ones
		// as the rule-evaluator's readyness probe does check for connectivity to GCM.
		if skipGCM {
			// TODO(pintohutch): stub CTS API during e2e tests to remove
			// this conditional.
			if *deploy.Spec.Replicas != deploy.Status.UpdatedReplicas {
				return false, nil
			}
		} else if *deploy.Spec.Replicas != deploy.Status.ReadyReplicas {
			return false, nil
		}

		// Assert we have the expected annotations.
		wantedAnnotations := map[string]string{
			"components.gke.io/component-name":               "managed_prometheus",
			"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
		}
		if diff := cmp.Diff(wantedAnnotations, deploy.Spec.Template.Annotations); diff != "" {
			return false, errors.Errorf("unexpected annotations (-want, +got): %s", diff)
		}

		for _, c := range deploy.Spec.Template.Spec.Containers {
			if c.Name != "evaluator" {
				continue
			}
			// We're mainly interested in the dynamic flags but checking the entire set including
			// the static ones is ultimately simpler.
			wantArgs := []string{
				fmt.Sprintf("--export.label.project-id=%q", projectID),
				fmt.Sprintf("--export.label.location=%q", location),
				fmt.Sprintf("--export.label.cluster=%q", cluster),
				fmt.Sprintf("--query.project-id=%q", projectID),
			}
			if gcpServiceAccount != "" {
				filepath := fmt.Sprintf("/etc/secrets/secret_%s_user-gcp-service-account_key.json", t.pubNamespace)
				wantArgs = append(wantArgs,
					fmt.Sprintf("--export.credentials-file=%q", filepath),
					fmt.Sprintf("--query.credentials-file=%q", filepath),
				)
			}

			if diff := cmp.Diff(strings.Join(wantArgs, " "), getEnvVar(c.Env, "EXTRA_ARGS")); diff != "" {
				return false, errors.Errorf("unexpected flags (-want, +got): %s", diff)
			}
			return true, nil
		}
		return false, errors.New("no container with name evaluator found")
	})
	if err != nil {
		t.Fatalf("failed waiting for generated rule-evaluator deployment: %s", err)
	}
}

// TestWebhookCABundleInjection checks whether a CABundle is injected into the expected
// webhook configurations.
func TestWebhookCABundleInjection(t *testing.T) {
	tctx := newTestContext(t)

	var (
		whConfigName = fmt.Sprintf("gmp-operator.%s.monitoring.googleapis.com", tctx.namespace)
		policy       = arv1.Ignore // Prevent collisions with other test or real usage
		sideEffects  = arv1.SideEffectClassNone
		url          = "https://0.1.2.3/"
	)

	// Create webhook configs. The operator must populate their caBundles.
	vwc := &arv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            whConfigName,
			OwnerReferences: tctx.ownerReferences,
		},
		Webhooks: []arv1.ValidatingWebhook{
			{
				Name:                    "wh1.monitoring.googleapis.com",
				ClientConfig:            arv1.WebhookClientConfig{URL: &url},
				FailurePolicy:           &policy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			}, {
				Name:                    "wh2.monitoring.googleapis.com",
				ClientConfig:            arv1.WebhookClientConfig{URL: &url},
				FailurePolicy:           &policy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			},
		},
	}
	_, err := tctx.kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(context.Background(), vwc, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mwc := &arv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:            whConfigName,
			OwnerReferences: tctx.ownerReferences,
		},
		Webhooks: []arv1.MutatingWebhook{
			{
				Name:                    "wh1.monitoring.googleapis.com",
				ClientConfig:            arv1.WebhookClientConfig{URL: &url},
				FailurePolicy:           &policy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			}, {
				Name:                    "wh2.monitoring.googleapis.com",
				ClientConfig:            arv1.WebhookClientConfig{URL: &url},
				FailurePolicy:           &policy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
			},
		},
	}
	_, err = tctx.kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(context.Background(), mwc, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for caBundle injection.
	err = wait.Poll(3*time.Second, 2*time.Minute, func() (bool, error) {
		vwc, err := tctx.kubeClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), whConfigName, metav1.GetOptions{})
		if err != nil {
			return false, errors.Errorf("get validatingwebhook configuration: %s", err)
		}
		if len(vwc.Webhooks) != 2 {
			return false, errors.Errorf("expected 2 webhooks but got %d", len(vwc.Webhooks))
		}
		for _, wh := range vwc.Webhooks {
			if len(wh.ClientConfig.CABundle) == 0 {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("waiting for ValidatingWebhook CA bundle failed: %s", err)
	}

	err = wait.Poll(3*time.Second, 2*time.Minute, func() (bool, error) {
		mwc, err := tctx.kubeClient.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(context.Background(), whConfigName, metav1.GetOptions{})
		if err != nil {
			return false, errors.Errorf("get mutatingwebhook configuration: %s", err)
		}
		if len(mwc.Webhooks) != 2 {
			return false, errors.Errorf("expected 2 webhooks but got %d", len(vwc.Webhooks))
		}
		for _, wh := range mwc.Webhooks {
			if len(wh.ClientConfig.CABundle) == 0 {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("waiting for MutatingWebhook CA bundle failed: %s", err)
	}
}

// testCollectorDeployed does a high-level verification on whether the
// collector is deployed to the cluster.
func testCollectorDeployed(ctx context.Context, t *testContext) {
	// Create initial OperatorConfig to trigger deployment of resources.
	opCfg := &monitoringv1.OperatorConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: operator.NameOperatorConfig,
		},
		Collection: monitoringv1.CollectionSpec{
			ExternalLabels: map[string]string{
				"external_key": "external_val",
			},
			Filter: monitoringv1.ExportFilters{
				MatchOneOf: []string{
					"{job='foo'}",
					"{__name__=~'up'}",
				},
			},
			KubeletScraping: &monitoringv1.KubeletScraping{
				Interval: "5s",
			},
		},
	}
	if gcpServiceAccount != "" {
		opCfg.Collection.Credentials = &v1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: "user-gcp-service-account",
			},
			Key: "key.json",
		}
	}
	_, err := t.operatorClient.MonitoringV1().OperatorConfigs(t.pubNamespace).Create(ctx, opCfg, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create rules operatorconfig: %s", err)
	}

	err = wait.Poll(3*time.Second, 3*time.Minute, func() (bool, error) {
		ds, err := t.kubeClient.AppsV1().DaemonSets(t.namespace).Get(ctx, operator.NameCollector, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			t.Log(errors.Errorf("getting collector DaemonSet failed: %s", err))
			return false, errors.Errorf("getting collector DaemonSet failed: %s", err)
		}
		// At first creation the DaemonSet may appear with 0 desired replicas. This should
		// change shortly after.
		if ds.Status.DesiredNumberScheduled == 0 {
			return false, nil
		}

		// TODO(pintohutch): run all tests without skipGCM by providing boilerplate
		// credentials for use in local testing and CI.
		//
		// This is necessary for any e2e tests that don't have access to GCP
		// credentials. We were getting away with this by running on networks
		// with access to the GCE metadata server IP to supply them:
		// https://github.com/googleapis/google-cloud-go/blob/56d81f123b5b4491aaf294042340c35ffcb224a7/compute/metadata/metadata.go#L39
		// However, running without this access (e.g. on Github Actions) causes
		// a failure from:
		// https://cs.opensource.google/go/x/oauth2/+/master:google/default.go;l=155;drc=9780585627b5122c8cc9c6a378ac9861507e7551
		if !skipGCM {
			if ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
				return false, nil
			}
		}

		// Assert we have the expected annotations.
		wantedAnnotations := map[string]string{
			"components.gke.io/component-name":               "managed_prometheus",
			"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
		}
		if diff := cmp.Diff(wantedAnnotations, ds.Spec.Template.Annotations); diff != "" {
			return false, errors.Errorf("unexpected annotations (-want, +got): %s", diff)
		}

		for _, c := range ds.Spec.Template.Spec.Containers {
			if c.Name != "prometheus" {
				continue
			}

			// We're mainly interested in the dynamic flags but checking the entire set including
			// the static ones is ultimately simpler.
			wantArgs := []string{
				fmt.Sprintf("--export.label.project-id=%q", projectID),
				fmt.Sprintf("--export.label.location=%q", location),
				fmt.Sprintf("--export.label.cluster=%q", cluster),
				`--export.match="{job='foo'}"`,
				`--export.match="{__name__=~'up'}"`,
			}
			if gcpServiceAccount != "" {
				wantArgs = append(wantArgs, fmt.Sprintf(`--export.credentials-file="/etc/secrets/secret_%s_user-gcp-service-account_key.json"`, t.pubNamespace))
			}

			if diff := cmp.Diff(strings.Join(wantArgs, " "), getEnvVar(c.Env, "EXTRA_ARGS")); diff != "" {
				t.Log(errors.Errorf("unexpected flags (-want, +got): %s", diff))
				return false, errors.Errorf("unexpected flags (-want, +got): %s", diff)
			}
			return true, nil
		}
		t.Log(errors.New("no container with name prometheus found"))
		return false, errors.New("no container with name prometheus found")
	})
	if err != nil {
		t.Fatalf("Waiting for DaemonSet deployment failed: %s", err)
	}
}

// testCollectorSelfPodMonitoring sets up pod monitoring of the collector itself
// and waits for samples to become available in Cloud Monitoring.
func testCollectorSelfPodMonitoring(ctx context.Context, t *testContext) {
	// The operator should configure the collector to scrape itself and its metrics
	// should show up in Cloud Monitoring shortly after.
	podmon := &monitoringv1.PodMonitoring{
		ObjectMeta: metav1.ObjectMeta{
			Name: "collector-podmon",
		},
		Spec: monitoringv1.PodMonitoringSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					operator.LabelAppName: operator.NameCollector,
				},
			},
			Endpoints: []monitoringv1.ScrapeEndpoint{
				{Port: intstr.FromString("prom-metrics"), Interval: "5s"},
				{Port: intstr.FromString("cfg-rel-metrics"), Interval: "5s"},
			},
		},
	}

	_, err := t.operatorClient.MonitoringV1().PodMonitorings(t.namespace).Create(ctx, podmon, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create collector PodMonitoring: %s", err)
	}
	t.Log("Waiting for PodMonitoring collector-podmon to be processed")

	var resVer = ""
	err = wait.Poll(time.Second, 1*time.Minute, func() (bool, error) {
		pm, err := t.operatorClient.MonitoringV1().PodMonitorings(t.namespace).Get(ctx, "collector-podmon", metav1.GetOptions{})
		if err != nil {
			return false, errors.Errorf("getting PodMonitoring failed: %s", err)
		}
		// Ensure no status update cycles.
		// This is not a perfect check as it's possible the get call returns before the operator
		// would sync again, however it can serve as a valuable guardrail in case sporadic test
		// failures start happening due to update cycles.
		if size := len(pm.Status.Conditions); size == 1 {
			if resVer == "" {
				resVer = pm.ResourceVersion
				return false, nil
			}
			success := pm.Status.Conditions[0].Type == monitoringv1.ConfigurationCreateSuccess
			steadyVer := resVer == pm.ResourceVersion
			return success && steadyVer, nil
		} else if size > 1 {
			return false, errors.Errorf("status conditions should be of length 1, but got: %d", size)
		}
		return false, nil
	})
	if err != nil {
		t.Errorf("unable to validate PodMonitoring status: %s", err)
	}

	if !skipGCM {
		t.Log("Waiting for up metrics for collector targets")
		validateCollectorUpMetrics(ctx, t, "collector-podmon")
	}
}

// testCollectorSelfClusterPodMonitoring sets up pod monitoring of the collector itself
// and waits for samples to become available in Cloud Monitoring.
func testCollectorSelfClusterPodMonitoring(ctx context.Context, t *testContext) {
	// The operator should configure the collector to scrape itself and its metrics
	// should show up in Cloud Monitoring shortly after.
	podmon := &monitoringv1.ClusterPodMonitoring{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "collector-cmon",
			OwnerReferences: t.ownerReferences,
		},
		Spec: monitoringv1.ClusterPodMonitoringSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					operator.LabelAppName: operator.NameCollector,
				},
			},
			Endpoints: []monitoringv1.ScrapeEndpoint{
				{Port: intstr.FromString("prom-metrics"), Interval: "5s"},
				{Port: intstr.FromString("cfg-rel-metrics"), Interval: "5s"},
			},
		},
	}

	_, err := t.operatorClient.MonitoringV1().ClusterPodMonitorings().Create(ctx, podmon, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create collector ClusterPodMonitoring: %s", err)
	}
	t.Log("Waiting for PodMonitoring collector-podmon to be processed")

	var resVer = ""
	err = wait.Poll(time.Second, 1*time.Minute, func() (bool, error) {
		pm, err := t.operatorClient.MonitoringV1().ClusterPodMonitorings().Get(ctx, "collector-cmon", metav1.GetOptions{})
		if err != nil {
			return false, errors.Errorf("getting ClusterPodMonitoring failed: %s", err)
		}
		// Ensure no status update cycles.
		// This is not a perfect check as it's possible the get call returns before the operator
		// would sync again, however it can serve as a valuable guardrail in case sporadic test
		// failures start happening due to update cycles.
		if size := len(pm.Status.Conditions); size == 1 {
			if resVer == "" {
				resVer = pm.ResourceVersion
				return false, nil
			}
			success := pm.Status.Conditions[0].Type == monitoringv1.ConfigurationCreateSuccess
			steadyVer := resVer == pm.ResourceVersion
			return success && steadyVer, nil
		} else if size > 1 {
			return false, errors.Errorf("status conditions should be of length 1, but got: %d", size)
		}
		return false, nil
	})
	if err != nil {
		t.Errorf("unable to validate ClusterPodMonitoring status: %s", err)
	}

	if !skipGCM {
		t.Log("Waiting for up metrics for collector targets")
		validateCollectorUpMetrics(ctx, t, "collector-cmon")
	}
}

// validateCollectorUpMetrics checks whether the scrape-time up metrics for all collector
// pods can be queried from GCM.
func validateCollectorUpMetrics(ctx context.Context, t *testContext, job string) {
	// The project, location, and cluster name in which we look for the metric data must
	// be provided by the user. Check this only in this test so tests that don't need these
	// flags can still be run without them.
	// They can be configured on the operator but our current test setup (targeting GKE)
	// relies on the operator inferring them from the environment.
	if projectID == "" {
		t.Fatalf("no project specified (--project-id flag)")
	}
	if location == "" {
		t.Fatalf("no location specified (--location flag)")
	}
	if cluster == "" {
		t.Fatalf("no cluster name specified (--cluster flag)")
	}

	// Wait for metric data to show up in Cloud Monitoring.
	metricClient, err := gcm.NewMetricClient(ctx)
	if err != nil {
		t.Fatalf("Create GCM metric client: %s", err)
	}
	defer metricClient.Close()

	pods, err := t.kubeClient.CoreV1().Pods(t.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", operator.LabelAppName, operator.NameCollector),
	})
	if err != nil {
		t.Fatalf("List collector pods: %s", err)
	}

	// See whether the `up` metric is written for each pod/port combination. It is set to 1 by
	// Prometheus on successful scraping of the target. Thereby we validate service discovery
	// configuration, config reload handling, as well as data export are correct.
	//
	// Make a single query for each pod/port combo as this is simpler than untangling the result
	// of a single query.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	for _, pod := range pods.Items {
		for _, port := range []string{"prom-metrics", "cfg-rel-metrics"} {
			t.Logf("Poll up metric for pod %q and port %q", pod.Name, port)

			err = wait.PollImmediateUntil(3*time.Second, func() (bool, error) {
				now := time.Now()

				// Validate the majority of labels being set correctly by filtering along them.
				iter := metricClient.ListTimeSeries(ctx, &gcmpb.ListTimeSeriesRequest{
					Name: fmt.Sprintf("projects/%s", projectID),
					Filter: fmt.Sprintf(`
				resource.type = "prometheus_target" AND
				resource.labels.project_id = "%s" AND
				resource.label.location = "%s" AND
				resource.labels.cluster = "%s" AND
				resource.labels.namespace = "%s" AND
				resource.labels.job = "%s" AND
				resource.labels.instance = "%s:%s" AND
				metric.type = "prometheus.googleapis.com/up/gauge" AND
				metric.labels.external_key = "external_val"
				`,
						projectID, location, cluster, t.namespace, job, pod.Spec.NodeName, port,
					),
					Interval: &gcmpb.TimeInterval{
						EndTime:   timestamppb.New(now),
						StartTime: timestamppb.New(now.Add(-10 * time.Second)),
					},
				})
				series, err := iter.Next()
				if err == iterator.Done {
					t.Logf("No data, retrying...")
					return false, nil
				} else if err != nil {
					return false, errors.Wrap(err, "querying metrics failed")
				}
				if v := series.Points[len(series.Points)-1].Value.GetDoubleValue(); v != 1 {
					t.Logf("Up still %v, retrying...", v)
					return false, nil
				}
				// We expect exactly one result.
				series, err = iter.Next()
				if err != iterator.Done {
					return false, errors.Errorf("expected iterator to be done but got error %q and series %v", err, series)
				}
				return true, nil
			}, ctx.Done())
			if err != nil {
				t.Fatalf("Waiting for collector metrics to appear in Cloud Monitoring failed: %s", err)
			}
		}
	}
}

// testCollectorScrapeKubelet verifies that kubelet metric endpoints are successfully scraped.
func testCollectorScrapeKubelet(ctx context.Context, t *testContext) {
	if skipGCM {
		t.Log("Not validating scraping of kubelets when --skip-gcm is set")
		return
	}
	if projectID == "" {
		t.Fatalf("no project specified (--project-id flag)")
	}
	if location == "" {
		t.Fatalf("no location specified (--location flag)")
	}
	if cluster == "" {
		t.Fatalf("no cluster name specified (--cluster flag)")
	}

	// Wait for metric data to show up in Cloud Monitoring.
	metricClient, err := gcm.NewMetricClient(ctx)
	if err != nil {
		t.Fatalf("Create GCM metric client: %s", err)
	}
	defer metricClient.Close()

	nodes, err := t.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List nodes: %s", err)
	}

	// See whether the `up` metric for both kubelet endpoints is 1 for each node on which
	// a collector pod is running.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	for _, node := range nodes.Items {
		for _, port := range []string{"metrics", "cadvisor"} {
			t.Logf("Poll up metric for kubelet on node %q and port %q", node.Name, port)

			err = wait.PollImmediateUntil(3*time.Second, func() (bool, error) {
				now := time.Now()

				// Validate the majority of labels being set correctly by filtering along them.
				iter := metricClient.ListTimeSeries(ctx, &gcmpb.ListTimeSeriesRequest{
					Name: fmt.Sprintf("projects/%s", projectID),
					Filter: fmt.Sprintf(`
				resource.type = "prometheus_target" AND
				resource.labels.project_id = "%s" AND
				resource.label.location = "%s" AND
				resource.labels.cluster = "%s" AND
				resource.labels.job = "kubelet" AND
				resource.labels.instance = "%s:%s" AND
				metric.type = "prometheus.googleapis.com/up/gauge" AND
				metric.labels.node = "%s"
				metric.labels.external_key = "external_val"
				`,
						projectID, location, cluster, node.Name, port, node.Name,
					),
					Interval: &gcmpb.TimeInterval{
						EndTime:   timestamppb.New(now),
						StartTime: timestamppb.New(now.Add(-10 * time.Second)),
					},
				})
				series, err := iter.Next()
				if err == iterator.Done {
					t.Logf("No data, retrying...")
					return false, nil
				} else if err != nil {
					return false, errors.Wrap(err, "querying metrics failed")
				}
				if v := series.Points[len(series.Points)-1].Value.GetDoubleValue(); v != 1 {
					t.Logf("Up still %v, retrying...", v)
					return false, nil
				}
				// We expect exactly one result.
				series, err = iter.Next()
				if err != iterator.Done {
					return false, errors.Errorf("expected iterator to be done but got error %q and series %v", err, series)
				}
				return true, nil
			}, ctx.Done())
			if err != nil {
				t.Fatalf("Waiting for collector metrics to appear in Cloud Monitoring failed: %s", err)
			}
		}
	}
}

func testRulesGeneration(ctx context.Context, t *testContext) {
	replace := strings.NewReplacer(
		"{project_id}", projectID,
		"{cluster}", cluster,
		"{location}", location,
		"{namespace}", t.namespace,
	).Replace

	// Create multiple rules in the cluster and expect their scoped equivalents
	// to be present in the generated rule file.
	content := replace(`
apiVersion: monitoring.googleapis.com/v1alpha1
kind: GlobalRules
metadata:
  name: global-rules
spec:
  groups:
  - name: group-1
    rules:
    - record: bar
      expr: avg(up)
      labels:
        flavor: test
`)
	var globalRules monitoringv1.GlobalRules
	if err := kyaml.Unmarshal([]byte(content), &globalRules); err != nil {
		t.Fatal(err)
	}
	globalRules.OwnerReferences = t.ownerReferences

	if _, err := t.operatorClient.MonitoringV1().GlobalRules().Create(context.TODO(), &globalRules, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	content = replace(`
apiVersion: monitoring.googleapis.com/v1alpha1
kind: ClusterRules
metadata:
  name: {namespace}-cluster-rules
spec:
  groups:
  - name: group-1
    rules:
    - record: foo
      expr: sum(up)
      labels:
        flavor: test
`)
	var clusterRules monitoringv1.ClusterRules
	if err := kyaml.Unmarshal([]byte(content), &clusterRules); err != nil {
		t.Fatal(err)
	}
	clusterRules.OwnerReferences = t.ownerReferences

	if _, err := t.operatorClient.MonitoringV1().ClusterRules().Create(context.TODO(), &clusterRules, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// TODO(freinartz): Instantiate structs directly rather than templating strings.
	content = `
apiVersion: monitoring.googleapis.com/v1alpha1
kind: Rules
metadata:
  name: rules
spec:
  groups:
  - name: group-1
    rules:
    - alert: Bar
      expr: avg(down) > 1
      annotations:
        description: "bar avg down"
      labels:
        flavor: test
    - record: always_one
      expr: vector(1)
`
	var rules monitoringv1.Rules
	if err := kyaml.Unmarshal([]byte(content), &rules); err != nil {
		t.Fatal(err)
	}
	if _, err := t.operatorClient.MonitoringV1().Rules(t.namespace).Create(context.TODO(), &rules, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		replace("globalrules__global-rules.yaml"): replace(`groups:
    - name: group-1
      rules:
        - record: bar
          expr: avg(up)
          labels:
            flavor: test
`),
		replace("clusterrules__{namespace}-cluster-rules.yaml"): replace(`groups:
    - name: group-1
      rules:
        - record: foo
          expr: sum(up{cluster="{cluster}",location="{location}",project_id="{project_id}"})
          labels:
            cluster: {cluster}
            flavor: test
            location: {location}
            project_id: {project_id}
`),
		replace("rules__{namespace}__rules.yaml"): replace(`groups:
    - name: group-1
      rules:
        - alert: Bar
          expr: avg(down{cluster="{cluster}",location="{location}",namespace="{namespace}",project_id="{project_id}"}) > 1
          labels:
            cluster: {cluster}
            flavor: test
            location: {location}
            namespace: {namespace}
            project_id: {project_id}
          annotations:
            description: bar avg down
        - record: always_one
          expr: vector(1)
          labels:
            cluster: {cluster}
            location: {location}
            namespace: {namespace}
            project_id: {project_id}
`),
	}

	var diff string

	err := wait.Poll(1*time.Second, time.Minute, func() (bool, error) {
		cm, err := t.kubeClient.CoreV1().ConfigMaps(t.namespace).Get(context.TODO(), "rules-generated", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, errors.Wrap(err, "get ConfigMap")
		}
		// The operator observes Rules across all namespaces. For the purpose of this test we drop
		// all outputs from the result that aren't in the expected set.
		for name := range cm.Data {
			if _, ok := want[name]; !ok {
				delete(cm.Data, name)
			}
		}
		diff = cmp.Diff(want, cm.Data)
		return diff == "", nil
	})
	if err != nil {
		t.Errorf("diff (-want, +got): %s", diff)
		t.Fatalf("failed waiting for generated rules: %s", err)
	}
}

func testAlertmanagerDeployed(spec *monitoringv1.ManagedAlertmanagerSpec) func(context.Context, *testContext) {
	return func(ctx context.Context, t *testContext) {
		opCfg := &monitoringv1.OperatorConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: operator.NameOperatorConfig,
			},
			Collection: monitoringv1.CollectionSpec{
				ExternalLabels: map[string]string{
					"external_key": "external_val",
				},
				Filter: monitoringv1.ExportFilters{
					MatchOneOf: []string{
						"{job='foo'}",
						"{__name__=~'up'}",
					},
				},
				KubeletScraping: &monitoringv1.KubeletScraping{
					Interval: "5s",
				},
			},
			ManagedAlertmanager: spec,
		}
		if gcpServiceAccount != "" {
			opCfg.Collection.Credentials = &v1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "user-gcp-service-account",
				},
				Key: "key.json",
			}
		}
		_, err := t.operatorClient.MonitoringV1().OperatorConfigs(t.pubNamespace).Create(ctx, opCfg, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create rules operatorconfig: %s", err)
		}

		err = wait.Poll(time.Second, 1*time.Minute, func() (bool, error) {
			ss, err := t.kubeClient.AppsV1().StatefulSets(t.namespace).Get(ctx, operator.NameAlertmanager, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			} else if err != nil {
				t.Log(errors.Errorf("getting alertmanager StatefulSet failed: %s", err))
				return false, errors.Errorf("getting alertmanager StatefulSet failed: %s", err)
			}

			// Assert we have the expected annotations.
			wantedAnnotations := map[string]string{
				"components.gke.io/component-name":               "managed_prometheus",
				"cluster-autoscaler.kubernetes.io/safe-to-evict": "true",
			}
			if diff := cmp.Diff(wantedAnnotations, ss.Spec.Template.Annotations); diff != "" {
				return false, errors.Errorf("unexpected annotations (-want, +got): %s", diff)
			}

			return true, nil
		})
		if err != nil {
			t.Errorf("unable to get alertmanager statefulset: %s", err)
		}
	}
}

func testAlertmanagerConfig(pub *corev1.Secret, key string) func(context.Context, *testContext) {
	return func(ctx context.Context, t *testContext) {
		_, err := t.kubeClient.CoreV1().Secrets(t.pubNamespace).Create(ctx, pub, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("unable to create alertmanager config secret: %s", err)
		}

		err = wait.Poll(3*time.Second, 3*time.Minute, func() (bool, error) {
			secret, err := t.kubeClient.CoreV1().Secrets(t.namespace).Get(ctx, operator.NameAlertmanager, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil
			} else if err != nil {
				t.Log(errors.Errorf("getting alertmanager secret failed: %s", err))
				return false, errors.Errorf("getting alertmanager secret failed: %s", err)
			}

			bytes, ok := secret.Data["config.yaml"]
			if !ok {
				t.Log(errors.Errorf("getting alertmanager secret data in config.yaml failed"))
				return false, errors.Errorf("getting alertmanager secret data in config.yaml failed")
			}

			// Grab data from public secret and compare.
			data := pub.Data[key]
			if diff := cmp.Diff(data, bytes); diff != "" {
				return false, errors.Errorf("unexpected configuration (-want, +got): %s", diff)
			}
			return true, nil
		})
		if err != nil {
			t.Errorf("unable to get alertmanager config: %s", err)
		}
	}
}

func testValidateRuleEvaluationMetrics(ctx context.Context, t *testContext) {
	// The project, location and cluster name in which we look for the metric data must
	// be provided by the user. Check this only in this test so tests that don't need these
	// flags can still be run without them.
	if projectID == "" {
		t.Fatalf("no project specified (--project-id flag)")
	}
	if location == "" {
		t.Fatalf("no location specified (--location flag)")
	}
	if cluster == "" {
		t.Fatalf("no cluster name specified (--cluster flag)")
	}

	// Wait for metric data to show up in Cloud Monitoring.
	metricClient, err := gcm.NewMetricClient(ctx)
	if err != nil {
		t.Fatalf("Create GCM metric client: %s", err)
	}
	defer metricClient.Close()

	err = wait.Poll(1*time.Second, 3*time.Minute, func() (bool, error) {
		now := time.Now()

		// Validate the majority of labels being set correctly by filtering along them.
		iter := metricClient.ListTimeSeries(ctx, &gcmpb.ListTimeSeriesRequest{
			Name: fmt.Sprintf("projects/%s", projectID),
			Filter: fmt.Sprintf(`
				resource.type = "prometheus_target" AND
				resource.labels.project_id = "%s" AND
				resource.labels.location = "%s" AND
				resource.labels.cluster = "%s" AND
				resource.labels.namespace = "%s" AND
				metric.type = "prometheus.googleapis.com/always_one/gauge"
				`,
				projectID, location, cluster, t.namespace,
			),
			Interval: &gcmpb.TimeInterval{
				EndTime:   timestamppb.New(now),
				StartTime: timestamppb.New(now.Add(-10 * time.Second)),
			},
		})
		series, err := iter.Next()
		if err == iterator.Done {
			t.Logf("No data, retrying...")
			return false, nil
		} else if err != nil {
			return false, errors.Wrap(err, "querying metrics failed")
		}
		if len(series.Points) == 0 {
			return false, errors.New("unexpected zero points in result series")
		}
		// We expect exactly one result.
		series, err = iter.Next()
		if err != iterator.Done {
			return false, errors.Errorf("expected iterator to be done but got error %q and series %v", err, series)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Waiting for rule metrics to appear in Cloud Monitoring failed: %s", err)
	}
}

func getEnvVar(evs []corev1.EnvVar, key string) string {
	for _, ev := range evs {
		if ev.Name == key {
			return ev.Value
		}
	}
	return ""
}
