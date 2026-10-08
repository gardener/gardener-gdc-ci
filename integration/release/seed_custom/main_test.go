// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package seed_custom

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"strings"
	"testing"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/utils/ptr"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientretry "k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/helm"
)

var (
	gardenerArtifactsVersion     string
	mcmArtifactsVersion          string
	ccmArtifactsVersion          string
	virtualGardenProvider        string
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
)

const (
	waitForSeedCreationTimeout  = 60 * time.Minute
	gardenletValuesTemplatePath = "gardenlet-values.yaml"
	gdchCredentialsSecretName   = "gdch-cred"
)

type seedTemplateData struct {
	SeedName                 string
	GardenerVersion          string
	HarborInstanceURL        string
	CaData                   string
	Region                   string
	ManagedDNSDomainName     string
	SeedPodsCIDR             string
	SeedServicesCIDR         string
	GardenClusterURL         string
	TLSServerName            string
	GardenletChartURL        string
	GardenletImageRepository string
	DualzoneProviderConfig   string
	BackupBucketCredSecret   string
	CommitHash               string
}

type secretDef struct {
	name        string
	labels      map[string]string
	annotations map[string]string
	data        map[string][]byte
}

type image struct {
	Name       string `yaml:"name"`
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
}

func init() {
	flag.StringVar(&gardenerArtifactsVersion, "gardener-artifacts-version", "", "The version string for Gardener artifacts")
	flag.StringVar(&mcmArtifactsVersion, "mcm-artifacts-version", "", "The version string for machine-controller-manager-provider-gdc artifacts")
	flag.StringVar(&ccmArtifactsVersion, "ccm-artifacts-version", "", "The version string for cloud-provider-gdc artifacts")
	flag.StringVar(&virtualGardenProvider, "virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
}

// TestCreateSeedCluster is an integration test that orchestrates the creation and readiness
// of a Gardener Seed cluster on GDC staging user cluster (gardener-agtest).
func TestCreateSeedCluster(t *testing.T) {
	validateFlags(t)
	ctx := context.Background()

	t.Logf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releaseConfigData, err := releaseConfig.LoadReleaseTestConfig(
		*releaseConfigurationFilePath,
		gardenerArtifactsVersion,
		releaseConfig.WithVirtualGardenProvider(virtualGardenProvider),
	)
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releaseConfigData != nil && releaseConfigData.GDCClient != nil {
		defer releaseConfigData.GDCClient.Cleanup()
	}

	gardenClient := releaseConfigData.TestShoot.VirtualGarden.Client

	t.Log("Creating bootstrap token secret in Virtual Garden")
	if err := createBootstrapTokenSecret(ctx, gardenClient, gardenerArtifactsVersion); err != nil {
		t.Fatalf("failed to create bootstrap token secret: %v", err)
	}

	t.Log("Creating bootstrap RBAC roles and bindings on Virtual Garden")
	if err := createBootstrapRBAC(ctx, gardenClient); err != nil {
		t.Fatalf("failed to create bootstrap RBAC: %v", err)
	}

	t.Log("Creating image pull secret in virtual garden")
	if err := gardener.CreateImagePullSecret(ctx, gardenClient, releaseConfigData.ImagePullCredentials); err != nil {
		t.Fatalf("failed to create image pull secret: %v", err)
	}

	t.Log("Creating Gardener secrets")
	if err := createGardenerSecrets(ctx, gardenClient, releaseConfigData); err != nil {
		t.Fatalf("failed to create gardener secrets: %v", err)
	}

	if releaseConfigData.GDC == nil {
		t.Fatalf("gdc configuration in releaseConfig is nil")
	}
	t.Log("Deploying GDCH extension provider")
	if err := deployExtensionProvider(ctx, gardenClient, releaseConfig.CandidateRegistryURL(), releaseConfigData.Gardener); err != nil {
		t.Fatalf("failed to deploy gdch extension provider: %v", err)
	}

	t.Log("Deploying Gardenlet")
	if err := deployGardenlet(ctx, releaseConfigData); err != nil {
		t.Fatalf("failed to deploy gardenlet: %v", err)
	}

	t.Logf("Waiting for Seed %q to become ready", releaseConfigData.Seed.Name)
	if err := waitForSeedReady(ctx, gardenClient, client.ObjectKey{Name: releaseConfigData.Seed.Name}, waitForSeedCreationTimeout); err != nil {
		t.Fatalf("seed %q did not become ready: %v", releaseConfigData.Seed.Name, err)
	}
}

// deployExtensionProvider creates a new GDCH extension provider configuration and deploys it
// to the Garden cluster.
func deployExtensionProvider(ctx context.Context, gardenClient client.WithWatch, harborInstanceURL string, gardenerConfig *config.GardenerConfig) error {
	extensionConfig, err := newGdchExtensionProviderConfig(harborInstanceURL, gardenerConfig)
	if err != nil {
		return fmt.Errorf("failed to create gdch extension config: %w", err)
	}
	if err := gardener.DeployExtension(ctx, gardenClient, extensionConfig); err != nil {
		return fmt.Errorf("failed to deploy gdch extension: %w", err)
	}
	return nil
}

// validateFlags checks if all required command-line flags are set.
func validateFlags(t *testing.T) {
	if *releaseConfigurationFilePath == "" {
		t.Fatal("flag --release-configuration-file-path must be set")
	}
	requiredFlags := map[string]string{
		"gardener-artifacts-version": gardenerArtifactsVersion,
	}

	for name, value := range requiredFlags {
		if value == "" {
			t.Fatalf("--%s is a required flag", name)
		}
	}
}

// createBootstrapTokenSecret creates or updates a Kubernetes Secret containing a bootstrap token
// for the gardenlet to use for authenticating with the Virtual Garden.
func createBootstrapTokenSecret(ctx context.Context, gardenClient client.WithWatch, seedName string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-07401b",
			Namespace: "kube-system",
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, secret, func() error {
		secret.Type = "bootstrap.kubernetes.io/token"
		secret.StringData = map[string]string{
			"description":                    fmt.Sprintf("Token to be used by the gardenlet for Seed `%s`.", seedName),
			"token-id":                       "07401b",
			"token-secret":                   "f395accd246ae52d",
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
		}
		return nil
	})
	return err
}

// createBootstrapRBAC creates the ClusterRole and ClusterRoleBinding for seed bootstrapping
// on the Virtual Garden cluster, allowing gardenlet to submit CSRs and register the Seed.
func createBootstrapRBAC(ctx context.Context, gardenClient client.WithWatch) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gardener.cloud:system:seed-bootstrapper",
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, gardenClient, cr, func() error {
		cr.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"certificates.k8s.io"},
				Resources: []string{"certificatesigningrequests"},
				Verbs:     []string{"create", "get"},
			},
			{
				APIGroups: []string{"certificates.k8s.io"},
				Resources: []string{"certificatesigningrequests/seedclient"},
				Verbs:     []string{"create"},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create or update ClusterRole gardener.cloud:system:seed-bootstrapper: %w", err)
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gardener.cloud:system:seed-bootstrapper",
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, gardenClient, crb, func() error {
		crb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "gardener.cloud:system:seed-bootstrapper",
		}
		crb.Subjects = []rbacv1.Subject{
			{
				Kind:     "Group",
				Name:     "system:bootstrappers",
				APIGroup: "rbac.authorization.k8s.io",
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to create or update ClusterRoleBinding gardener.cloud:system:seed-bootstrapper: %w", err)
	}

	return nil
}

// createGardenerSecrets reads the service account key and generates various Kubernetes Secrets
// required by Gardener components, such as GDCH credentials and DNS domain secrets.
func createGardenerSecrets(ctx context.Context, gardenClient client.WithWatch, releaseConfigData *config.ReleaseTestConfig) error {
	if releaseConfigData.GDC == nil {
		return fmt.Errorf("gdc configuration in releaseConfigData is nil")
	}
	saJSON, err := config.AccessLatestSecret(ctx, "gdc-service-account")
	if err != nil {
		return fmt.Errorf("failed to get sa key: %w", err)
	}

	secrets, err := getSecretDefs(releaseConfigData, saJSON)
	if err != nil {
		return fmt.Errorf("failed to get secret definitions: %w", err)
	}

	for _, secretData := range secrets {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        secretData.name,
				Namespace:   "garden",
				Labels:      secretData.labels,
				Annotations: secretData.annotations,
			},
		}

		if _, err := controllerutil.CreateOrUpdate(ctx, gardenClient, secret, func() error {
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = secretData.data
			return nil
		}); err != nil {
			return fmt.Errorf("failed to create or update secret %s: %w", secretData.name, err)
		}
	}

	return nil
}

// getSecretDefs constructs a slice of `secretDef` containing the definitions for various
// Kubernetes Secrets, including GDCH credentials and domain-related secrets.
func getSecretDefs(releaseConfigData *config.ReleaseTestConfig, saJSON []byte) ([]secretDef, error) {
	orgGlobalURL := releaseConfigData.GDC.GlobalAPIURL
	orgInfraMpURL := releaseConfigData.GDC.ManagementAPIURL
	managedDNSDomainName := releaseConfigData.GDC.ManagedDNSDomainName
	caData := []byte(releaseConfigData.GDC.CAData)
	dualzone := releaseConfigData.GDC.DualZoneBucketLocation

	zonalGDCHConfigBytes, err := newGDCHConfig(orgInfraMpURL, caData)
	if err != nil {
		return nil, err
	}

	globalGDCHConfigBytes, err := newGDCHConfig(orgGlobalURL, caData)
	if err != nil {
		return nil, err
	}

	secrets := []secretDef{
		{
			name: releaseConfig.GDCCredentialsSecretName,
			data: map[string][]byte{
				"serviceaccount.json": saJSON,
				"gdch-config":         globalGDCHConfigBytes,
			},
		},
		{
			name: "internal-domain",
			labels: map[string]string{
				"app":                 "gardener",
				"gardener.cloud/role": "internal-domain",
			},
			annotations: map[string]string{
				"dns.gardener.cloud/domain":   managedDNSDomainName,
				"dns.gardener.cloud/provider": "gdch-dns",
			},
			data: map[string][]byte{
				"serviceaccount.json": saJSON,
				"gdch-config":         globalGDCHConfigBytes,
			},
		},
		{
			name: "default-domain",
			labels: map[string]string{
				"app":                 "gardener",
				"gardener.cloud/role": "default-domain",
			},
			annotations: map[string]string{
				"dns.gardener.cloud/domain":   managedDNSDomainName,
				"dns.gardener.cloud/provider": "gdch-dns",
			},
			data: map[string][]byte{
				"serviceaccount.json": saJSON,
				"gdch-config":         globalGDCHConfigBytes,
			},
		},
	}

	if dualzone != "" {
		secrets = append(secrets, secretDef{
			name: "gdch-backup-cred-dualzone",
			data: map[string][]byte{
				"serviceaccount.json": saJSON,
				"gdch-config":         globalGDCHConfigBytes,
			},
		})
	} else {
		secrets = append(secrets, secretDef{
			name: "gdch-backup-cred",
			data: map[string][]byte{
				"serviceaccount.json": saJSON,
				"gdch-config":         zonalGDCHConfigBytes,
			},
		})
	}

	return secrets, nil
}

func newGDCHConfig(orgClusterURL string, caData []byte) ([]byte, error) {
	config := map[string]interface{}{
		"orgClusterURL": orgClusterURL,
		"caData":        string(caData),
		"isLancer":      true,
	}
	bytes, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal gdch config: %w", err)
	}
	return bytes, nil
}

func getDualzoneBucketConfig(dualZoneBucketLocation string) (string, string) {
	if dualZoneBucketLocation != "" {
		return fmt.Sprintf(`
          apiVersion: gdch.provider.extensions.gardener.gdc.goog/v1alpha1
          kind: BackupBucketConfig
          dualZoneBucketLocation: %s`, dualZoneBucketLocation), "gdch-backup-cred-dualzone"
	}
	return "", "gdch-backup-cred"
}

type imageOverwriteOptions struct {
	HarborInstanceURL string
	GardenerConfig    *config.GardenerConfig
}

func newGdchExtensionProviderConfig(harborInstanceURL string, gardenerConfig *config.GardenerConfig) (*gardener.ExtensionConfig, error) {
	imageOverwrite, err := extensionProviderImageOverwrite(imageOverwriteOptions{
		HarborInstanceURL: harborInstanceURL,
		GardenerConfig:    gardenerConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate image overwrite: %w", err)
	}
	extensionProviderChartVersion := strings.TrimPrefix(gardenerArtifactsVersion, "v")

	return &gardener.ExtensionConfig{
		Name:                "provider-gdch",
		HelmChartRef:        ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchHelmChartName, extensionProviderChartVersion)),
		ImagePullSecretName: gardener.ImagePullSecretName,
		Resources: []gardencorev1beta1.ControllerResource{
			{
				Kind:    "Infrastructure",
				Type:    "gdch",
				Primary: ptr.To(true),
			},
			{
				Kind:    "ControlPlane",
				Type:    "gdch",
				Primary: ptr.To(true),
			},
			{
				Kind:    "Worker",
				Type:    "gdch",
				Primary: ptr.To(true),
			},
			{
				Kind:    "DNSRecord",
				Type:    "gdch-dns",
				Primary: ptr.To(true),
			},
			{
				Kind:    "BackupBucket",
				Type:    "gdch",
				Primary: ptr.To(true),
			},
			{
				Kind:    "BackupEntry",
				Type:    "gdch",
				Primary: ptr.To(true),
			},
		},
		Values: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchImageName),
				"tag":        gardenerArtifactsVersion,
				"pullPolicy": "Always",
			},
			"imageVectorOverwrite": imageOverwrite,
		},
	}, nil
}

func extensionProviderImageOverwrite(opts imageOverwriteOptions) (string, error) {
	if opts.GardenerConfig == nil || opts.GardenerConfig.CSIImages == nil {
		return "", fmt.Errorf("gardener CSIImages configuration is missing")
	}
	csiImages := opts.GardenerConfig.CSIImages
	mcmTag := mcmArtifactsVersion
	if mcmTag == "" {
		mcmTag = gardenerArtifactsVersion
	}
	ccmTag := ccmArtifactsVersion
	if ccmTag == "" {
		ccmTag = gardenerArtifactsVersion
	}
	images := []image{
		{"machine-controller-manager-provider-gdch", fmt.Sprintf("%s/%s", opts.HarborInstanceURL, releaseConfig.MachineControllerManagerProviderImageName), mcmTag},
		{"cloud-controller-manager", fmt.Sprintf("%s/%s", opts.HarborInstanceURL, releaseConfig.CloudControllerManagerImageName), ccmTag},
		{"csi-driver", csiImages.CSIDriver.Repository, csiImages.CSIDriver.Tag},
		{"csi-provisioner", csiImages.CSIProvisioner.Repository, csiImages.CSIProvisioner.Tag},
		{"csi-attacher", csiImages.CSIAttacher.Repository, csiImages.CSIAttacher.Tag},
		{"csi-liveness-probe", csiImages.CSILivenessProbe.Repository, csiImages.CSILivenessProbe.Tag},
		{"csi-node-driver-registrar", csiImages.CSINodeDriverRegistrar.Repository, csiImages.CSINodeDriverRegistrar.Tag},
		{"csi-snapshotter", csiImages.CSISnapshotter.Repository, csiImages.CSISnapshotter.Tag},
		{"csi-resizer", csiImages.CSIResizer.Repository, csiImages.CSIResizer.Tag},
		{"csi-snapshot-controller", csiImages.CSISnapshotController.Repository, csiImages.CSISnapshotController.Tag},
	}

	imageVector := struct {
		Images []image `yaml:"images"`
	}{
		Images: images,
	}

	yamlBytes, err := yaml.Marshal(imageVector)
	if err != nil {
		return "", fmt.Errorf("failed to marshal image vector: %w", err)
	}
	return string(yamlBytes), nil
}

var getUserClusterKubeconfigPath = func(clusterName string) string {
	return fmt.Sprintf("/tmp/%s-kubeconfig", clusterName)
}

// deployGardenlet generates Helm values and deploys the Gardenlet chart to the cluster that hosts the seed cluster.
func deployGardenlet(ctx context.Context, releaseConfigData *config.ReleaseTestConfig) error {
	values, seedData, err := prepareGardenletHelmValues(ctx, releaseConfigData)
	if err != nil {
		return fmt.Errorf("failed to prepare gardenlet helm values: %w", err)
	}

	var targetClient client.Client
	var kubeconfigPath string

	if releaseConfigData.GDC != nil && releaseConfigData.GDC.UseUserClusterAsSeed {
		targetClient = releaseConfigData.RuntimeClusterClient
		if targetClient == nil && releaseConfigData.Seed != nil && releaseConfigData.Seed.HostCluster != nil {
			targetClient = releaseConfigData.Seed.HostCluster.UserClusterClient
		}
		clusterName := releaseConfigData.RuntimeClusterName
		if clusterName == "" && releaseConfigData.Seed != nil && releaseConfigData.Seed.HostCluster != nil {
			clusterName = releaseConfigData.Seed.HostCluster.UserClusterName
		}
		kubeconfigPath = getUserClusterKubeconfigPath(clusterName)
	} else {
		remoteShootCluster := releaseConfigData.Seed.HostCluster.Shoot
		shootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
		shootKubeconfigPath, err := gardener.GetShootKubeconfigPath(ctx, remoteShootCluster.VirtualGarden.Client, shootKey)
		if err != nil {
			return fmt.Errorf("failed to get shoot kubeconfig path: %w", err)
		}
		kubeconfigPath = shootKubeconfigPath
	}

	// Install helm chart
	opts := helm.InstallOptions{
		ChartPath:      seedData.GardenletChartURL,
		KubeconfigPath: kubeconfigPath,
		ReleaseName:    "gardenlet",
		Namespace:      "garden",
		Values:         values,
	}

	if _, err = helm.InstallOrUpgrade(opts); err != nil {
		return fmt.Errorf("failed to install or upgrade gardenlet: %w", err)
	}

	if releaseConfigData.GDC != nil && releaseConfigData.GDC.UseUserClusterAsSeed && targetClient != nil {
		log.Printf("Patching gardenlet HostAliases workaround inside user cluster")
		if err := patchGardenletHostAlias(ctx, targetClient, releaseConfigData); err != nil {
			return fmt.Errorf("failed to patch gardenlet host alias: %w", err)
		}
	}
	return nil
}

// prepareGardenletHelmValues creates the configuration data and generates the Helm values
// required for deploying the Gardenlet chart.
func prepareGardenletHelmValues(ctx context.Context, releaseConfigData *config.ReleaseTestConfig) (map[string]interface{}, *seedTemplateData, error) {
	caData := releaseConfigData.TestShoot.VirtualGarden.CAData
	gardenClusterURL := releaseConfigData.TestShoot.VirtualGarden.Host
	tlsServerName := ""

	// Parse host to extract SNI server name
	if gardenClusterURL != "" {
		origHost := gardenClusterURL
		if !strings.HasPrefix(origHost, "http://") && !strings.HasPrefix(origHost, "https://") {
			origHost = "https://" + origHost
		}
		if u, err := url.Parse(origHost); err == nil {
			tlsServerName = u.Hostname()
		}
	}

	if releaseConfigData.GDC != nil && releaseConfigData.GDC.UseUserClusterAsSeed {
		var zone string
		for _, uc := range releaseConfigData.GDC.UserClusters {
			if uc.Name == releaseConfigData.RuntimeClusterName {
				zone = uc.Zone
				break
			}
		}
		if zone != "" && releaseConfigData.GDC.ManagedDNSDomainName != "" {
			gardenClusterURL = fmt.Sprintf("https://api.virtual-garden.%s.%s.%s.%s",
				releaseConfigData.RuntimeClusterName,
				releaseConfigData.GDC.Project,
				zone,
				releaseConfigData.GDC.ManagedDNSDomainName)
			tlsServerName = fmt.Sprintf("api.virtual-garden.%s.%s.%s.%s",
				releaseConfigData.RuntimeClusterName,
				releaseConfigData.GDC.Project,
				zone,
				releaseConfigData.GDC.ManagedDNSDomainName)
		}
	}

	dualzoneProviderConfig, backupBucketCredSecret := getDualzoneBucketConfig(releaseConfigData.GDC.DualZoneBucketLocation)
	seedData := &seedTemplateData{
		SeedName:                 releaseConfigData.Seed.Name,
		GardenerVersion:          releaseConfigData.Gardener.GardenerVersion,
		HarborInstanceURL:        releaseConfigData.GDC.HarborRegistryURL,
		CaData:                   base64.StdEncoding.EncodeToString(caData),
		Region:                   releaseConfigData.GDC.Region,
		ManagedDNSDomainName:     releaseConfigData.GDC.ManagedDNSDomainName,
		SeedPodsCIDR:             releaseConfigData.Seed.Network.PodsCIDR,
		SeedServicesCIDR:         releaseConfigData.Seed.Network.ServicesCIDR,
		GardenClusterURL:         gardenClusterURL,
		TLSServerName:            tlsServerName,
		GardenletChartURL:        fmt.Sprintf("oci://%s/private-cloud/gardenlet:%s", releaseConfig.CandidateRegistryURL(), strings.TrimPrefix(releaseConfigData.Gardener.GardenerVersion, "v")),
		GardenletImageRepository: "europe-docker.pkg.dev/gardener-project/releases/gardener/gardenlet",
		DualzoneProviderConfig:   dualzoneProviderConfig,
		BackupBucketCredSecret:   backupBucketCredSecret,
		CommitHash:               gardenerArtifactsVersion,
	}

	tmpl, err := template.ParseFiles(gardenletValuesTemplatePath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse gardenlet values template: %w", err)
	}

	var valuesBuffer bytes.Buffer
	if err := tmpl.Execute(&valuesBuffer, seedData); err != nil {
		return nil, nil, fmt.Errorf("failed to execute gardenlet values template: %w", err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(valuesBuffer.Bytes(), &values); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal gardenlet values: %w", err)
	}

	return values, seedData, nil
}

const stabilizationDuration = 5 * time.Minute

// waitForSeedReady polls the Seed object until all required conditions (SeedSystemComponentsHealthy,
// SeedExtensionsReady, SeedBackupBucketsReady, and GardenletReady) are true for the stabilization duration.
func waitForSeedReady(ctx context.Context, gardenClient client.WithWatch, seedKey client.ObjectKey, timeout time.Duration) error {
	var (
		healthySince *time.Time
		lastLogTime  time.Time
	)

	return wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		seed := &gardencorev1beta1.Seed{}
		if err := gardenClient.Get(ctx, seedKey, seed); err != nil {
			if errors.IsNotFound(err) {
				if time.Since(lastLogTime) > 30*time.Second {
					log.Printf("Waiting for Seed %s to be created in Virtual Garden...", seedKey.Name)
					lastLogTime = time.Now()
				}
				return false, nil
			}
			log.Printf("Error getting Seed %s: %v", seedKey.Name, err)
			return false, nil
		}

		const seedGardenletReady gardencorev1beta1.ConditionType = "GardenletReady"

		expectedConditions := map[gardencorev1beta1.ConditionType]struct{}{
			gardencorev1beta1.SeedSystemComponentsHealthy: {},
			gardencorev1beta1.SeedExtensionsReady:         {},
			gardencorev1beta1.SeedBackupBucketsReady:      {},
			seedGardenletReady:                            {},
		}

		conditionStatusSummary := []string{}
		for _, condition := range seed.Status.Conditions {
			if _, expected := expectedConditions[condition.Type]; expected {
				conditionStatusSummary = append(conditionStatusSummary, fmt.Sprintf("%s=%s (Reason: %s, Message: %s)", condition.Type, condition.Status, condition.Reason, condition.Message))
				if condition.Status == gardencorev1beta1.ConditionTrue {
					delete(expectedConditions, condition.Type)
				}
			}
		}

		if time.Since(lastLogTime) > 30*time.Second {
			log.Printf("Seed %s condition statuses: %s | Unmet: %v", seedKey.Name, strings.Join(conditionStatusSummary, "; "), expectedConditions)
			lastLogTime = time.Now()
		}

		if len(expectedConditions) == 0 {
			now := time.Now()
			if healthySince == nil {
				healthySince = &now
				log.Printf("Seed %s has become healthy. Starting %s stabilization verification...", seedKey.Name, stabilizationDuration)
				return false, nil
			}

			if time.Since(*healthySince) >= stabilizationDuration {
				log.Printf("Seed %s remained healthy for %s. Seed is fully ready!", seedKey.Name, stabilizationDuration)
				return true, nil
			}
			remaining := stabilizationDuration - time.Since(*healthySince)
			if time.Since(lastLogTime) > 30*time.Second {
				log.Printf("Seed %s is healthy. Stabilization progress: %s remaining...", seedKey.Name, remaining.Round(time.Second))
				lastLogTime = time.Now()
			}
			return false, nil
		}

		if healthySince != nil {
			log.Printf("Seed %s became unhealthy during stabilization (unmet: %v). Resetting timer.", seedKey.Name, expectedConditions)
			healthySince = nil
		}

		return false, nil
	})
}

func patchGardenletHostAlias(ctx context.Context, targetClient client.Client, releaseConfigData *config.ReleaseTestConfig) error {
	var hostname string

	if releaseConfigData.GDC != nil && releaseConfigData.GDC.UseUserClusterAsSeed {
		var zone string
		for _, uc := range releaseConfigData.GDC.UserClusters {
			if uc.Name == releaseConfigData.RuntimeClusterName {
				zone = uc.Zone
				break
			}
		}
		if zone != "" && releaseConfigData.GDC.ManagedDNSDomainName != "" {
			hostname = fmt.Sprintf("api.virtual-garden.%s.%s.%s.%s",
				releaseConfigData.RuntimeClusterName,
				releaseConfigData.GDC.Project,
				zone,
				releaseConfigData.GDC.ManagedDNSDomainName)
		}
	}

	if hostname == "" {
		u, err := url.Parse(releaseConfigData.TestShoot.VirtualGarden.Host)
		if err != nil {
			return fmt.Errorf("failed to parse host URL %q: %w", releaseConfigData.TestShoot.VirtualGarden.Host, err)
		}
		hostname = u.Hostname()
	}

	// Try istio-ingressgateway (standard service created by gardener-operator),
	// falling back to istio-ingressgateway-internal if present.
	svc := &corev1.Service{}
	if err := targetClient.Get(ctx, client.ObjectKey{Namespace: "virtual-garden-istio-ingress", Name: "istio-ingressgateway"}, svc); err != nil {
		if err2 := targetClient.Get(ctx, client.ObjectKey{Namespace: "virtual-garden-istio-ingress", Name: "istio-ingressgateway-internal"}, svc); err2 != nil {
			return fmt.Errorf("failed to get ingress gateway service (tried istio-ingressgateway and istio-ingressgateway-internal): %w", err)
		}
	}
	clusterIP := svc.Spec.ClusterIP
	if clusterIP == "" {
		return fmt.Errorf("ingress gateway service %s has empty ClusterIP", svc.Name)
	}

	// Retry on conflict to avoid race conditions with Helm controller updates
	err := clientretry.RetryOnConflict(clientretry.DefaultBackoff, func() error {
		deploy := &appsv1.Deployment{}
		if err := targetClient.Get(ctx, client.ObjectKey{Namespace: "garden", Name: "gardenlet"}, deploy); err != nil {
			return err
		}

		// Update hostAliases in Pod template
		deploy.Spec.Template.Spec.HostAliases = []corev1.HostAlias{
			{
				IP:        clusterIP,
				Hostnames: []string{hostname},
			},
		}

		return targetClient.Update(ctx, deploy)
	})

	if err != nil {
		return fmt.Errorf("failed to patch gardenlet deployment with HostAlias: %w", err)
	}
	log.Printf("Successfully patched gardenlet deployment with HostAlias mapping %s -> %s (using service %s)", hostname, clusterIP, svc.Name)
	return nil
}
