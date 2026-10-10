// Copyright 2025 Google LLC
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

package seed

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"
	"testing"

	"text/template"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/utils/ptr"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/helm"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/kubernetes"
)

var (
	gardenerArtifactsVersion     string
	mcmArtifactsVersion          string
	ccmArtifactsVersion          string
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
)

const (
	waitForSeedCreationTimeout          = 45 * time.Minute
	seedPrometheusPVCBindTimeout        = 2 * time.Minute
	seedPrometheusPVCPollInterval       = 2 * time.Second
	gardenletValuesTemplatePath         = "gardenlet-values.yaml"
	gdchCredentialsSecretName           = "gdch-cred"
	gardenNamespace                     = "garden"
	gardenerFastStorageClass            = "gardener.cloud-fast"
	seedPrometheusStorageSize           = "10Gi"
	prometheusPVCNameFormat             = "prometheus-db-prometheus-%s-0"
	kvcsiAnnotationsAllowlistConfigMap  = "kvcsi-annotations-allowlist"
	kvcsiAnnotationsAllowlistDataKey    = "allowlist"
	kubevirtCSIControllerDeploymentName = "kubevirt-csi-controller"
	extensionControlPlaneSeedMRName     = "extension-controlplane-seed"
	extensionControlPlaneShootMRName    = "extension-controlplane-shoot"
	shootCoreVPAMRName                  = "shoot-core-vpa"
	gardenerIgnoreAnnotationKey         = "resources.gardener.cloud/ignore"
	cdiUsePopulatorAnnotationKey        = "cdi.kubevirt.io/storage.usePopulator"
	cdiBindImmediateRequestedAnnotation = "cdi.kubevirt.io/storage.bind.immediate.requested"
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
	GardenletChartURL        string
	GardenletImageRepository string
	DualzoneProviderConfig   string
	BackupBucketCredSecret   string
	CommitHash               string
}

type LoginConfig struct {
	Spec struct {
		Cluster []struct {
			Name   string `yaml:"name"`
			Server string `yaml:"server"`
		} `yaml:"cluster"`
	} `yaml:"spec"`
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
}

// TestCreateSeedCluster is an integration test that orchestrates the creation and readiness
// of a Gardener Seed cluster, including deploying necessary extensions and Gardenlet components.
func TestCreateSeedCluster(t *testing.T) {
	validateFlags(t)
	ctx := context.Background()

	t.Logf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releaseConfigData, err := releaseConfig.LoadReleaseTestConfig(*releaseConfigurationFilePath, gardenerArtifactsVersion)
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releaseConfigData != nil && releaseConfigData.GDCClient != nil {
		defer releaseConfigData.GDCClient.Cleanup()
	}

	gardenClient := releaseConfigData.TestShoot.VirtualGarden.Client

	t.Log("Creating bootstrap token secret")
	if err := createBootstrapTokenSecret(ctx, gardenClient, gardenerArtifactsVersion); err != nil {
		t.Fatalf("failed to create bootstrap token secret: %v", err)
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

	if err := recordSeedBucketNameOnHostShoot(ctx, gardenClient, releaseConfigData); err != nil {
		t.Fatalf("failed to record seed bucket name annotation on host shoot: %v", err)
	}
}

// recordSeedBucketNameOnHostShoot stores the Seed UID (which is the Seed BackupBucket and GDC Bucket name)
// on the host Shoot CR under SeedBucketNameAnnotation so teardown can resolve the bucket name from
// --gardener-artifacts-version via the host Shoot CR.
func recordSeedBucketNameOnHostShoot(ctx context.Context, gardenClient client.Client, releaseConfigData *config.ReleaseTestConfig) error {
	if releaseConfigData == nil || releaseConfigData.Seed == nil || releaseConfigData.Seed.HostCluster == nil || releaseConfigData.Seed.HostCluster.Shoot == nil {
		return nil
	}
	seed := &gardencorev1beta1.Seed{}
	if err := gardenClient.Get(ctx, client.ObjectKey{Name: releaseConfigData.Seed.Name}, seed); err != nil {
		return fmt.Errorf("failed to get Seed %q: %w", releaseConfigData.Seed.Name, err)
	}
	if seed.UID == "" {
		return nil
	}

	remoteShoot := releaseConfigData.Seed.HostCluster.Shoot
	hostShoot := &gardencorev1beta1.Shoot{}
	hostShootKey := client.ObjectKey{Name: remoteShoot.Name, Namespace: remoteShoot.Namespace}
	if err := remoteShoot.VirtualGarden.Client.Get(ctx, hostShootKey, hostShoot); err != nil {
		return fmt.Errorf("failed to get host Shoot %q: %w", hostShootKey, err)
	}
	patch := client.MergeFrom(hostShoot.DeepCopy())
	if hostShoot.Annotations == nil {
		hostShoot.Annotations = make(map[string]string)
	}
	hostShoot.Annotations[releaseConfig.SeedBucketNameAnnotation] = string(seed.UID)
	return remoteShoot.VirtualGarden.Client.Patch(ctx, hostShoot, patch)
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

// validateFlags checks if all required command-line flags are set, terminating the test
// if any essential flag is missing.
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
// for the gardenlet to use for authenticating with the Seed cluster. The token ID and secret
// are static for a consistent, identifiable bootstrap process.
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

// newGDCHConfig creates a GDCH configuration JSON byte slice using the provided
// cluster URL and CA data.
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

// getDualzoneBucketConfig returns the dualzone provider configuration and the corresponding
// backup bucket credential secret name based on the `dualZoneBucketLocation` parameter.
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

// newGdchExtensionProviderConfig creates an ExtensionConfig for the GDCH extension provider,
// including image overwrites based on the provided Harbor instance URL and Gardener configuration.
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

// extensionProviderImageOverwrite generates the image vector overwrite YAML for the extension provider.
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

// deployGardenlet generates Helm values and deploys the Gardenlet chart to the cluster that hosts the seed cluster.
// In this case, the host cluster is a remote shoot cluster.
// TODO(b/460465170) Update this function to support other host clusters types. e.g GDC VUC
func deployGardenlet(ctx context.Context, releaseConfigData *config.ReleaseTestConfig) error {
	values, seedData, err := prepareGardenletHelmValues(releaseConfigData)
	if err != nil {
		return fmt.Errorf("failed to prepare gardenlet helm values: %w", err)
	}
	remoteShootCluster := releaseConfigData.Seed.HostCluster.Shoot
	shootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
	shootKubeconfigPath, err := gardener.GetShootKubeconfigPath(ctx, remoteShootCluster.VirtualGarden.Client, shootKey)
	if err != nil {
		return fmt.Errorf("failed to get shoot kubeconfig path: %w", err)
	}

	// Suppress the host Shoot's VPA before deploying gardenlet so only the Seed's VPA manages VPA resources on this cluster.
	suppressRemoteShootTargetVPA(ctx, releaseConfigData)

	// Pre-create the 3 garden Prometheus PVCs (prometheus-db-prometheus-{aggregate,cache,seed}-0) with KubeVirt CDI
	// immediate-bind annotations before gardenlet deploys seed-prometheus. On freshly provisioned GDC Seed host Shoots,
	// prometheus-operator creates Filesystem PVCs without CDI annotations, which default to CDI's Volume Populator
	// (prime-*) flow under WaitForFirstConsumer and remain Pending. Pre-creating them with usePopulator="false" and
	// bind.immediate.requested="true" causes KubeVirt CDI to bind the volumes immediately so StatefulSets adopt them.
	ensureSeedGardenPrometheusPVCs(ctx, releaseConfigData)

	// Install helm chart
	opts := helm.InstallOptions{
		ChartPath:      seedData.GardenletChartURL,
		KubeconfigPath: shootKubeconfigPath,
		ReleaseName:    "gardenlet",
		Namespace:      "garden",
		Values:         values,
	}

	if _, err = helm.InstallOrUpgrade(opts); err != nil {
		return fmt.Errorf("failed to install or upgrade gardenlet: %w", err)
	}
	return nil
}

// ensureSeedGardenPrometheusPVCs ensures that when a GDC Shoot cluster (e.g., sh-grv-*-a) is registered as a Seed:
//  1. The host Seed's csi-driver-controller ConfigMap (kvcsi-annotations-allowlist) in shoot--<ns>--<name> allows
//     forwarding both cdi.kubevirt.io/storage.usePopulator and cdi.kubevirt.io/storage.bind.immediate.requested
//     to the underlying GDC infrastructure cluster, restarting csi-driver-controller if the allowlist was updated.
//  2. The 3 StatefulSet PVCs used by Gardener's seed-prometheus stack in namespace "garden"
//     (prometheus-db-prometheus-aggregate-0, prometheus-db-prometheus-cache-0, prometheus-db-prometheus-seed-0)
//     exist with cdi.kubevirt.io/storage.usePopulator="false" and cdi.kubevirt.io/storage.bind.immediate.requested="true"
//     and reach Phase=Bound before gardenlet is deployed.
//     Because Kubernetes StatefulSets match existing PVCs by deterministic name (<claimTemplate>-<statefulset>-<ordinal>)
//     and never overwrite existing PVCs, pre-creating and binding these PVCs guarantees immediate readiness on first boot
//     of a brand-new Seed host cluster without waiting for or getting stuck on CDI prime-* populator pods.
func ensureSeedGardenPrometheusPVCs(ctx context.Context, releaseConfigData *config.ReleaseTestConfig) {
	if releaseConfigData == nil || releaseConfigData.Seed == nil || releaseConfigData.Seed.HostCluster == nil || releaseConfigData.Seed.HostCluster.Shoot == nil {
		return
	}
	remoteShootCluster := releaseConfigData.Seed.HostCluster.Shoot
	if remoteShootCluster.VirtualGarden == nil {
		return
	}
	shootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}

	// Step 1: Ensure kvcsi-annotations-allowlist on the host VUC includes cdi.kubevirt.io/storage.bind.immediate.requested
	// so csi-driver-controller forwards the immediate-bind annotation from the Seed host Shoot to the infra cluster.
	if releaseConfigData.GDC != nil && releaseConfigData.GDCClient != nil {
		if userClusterClients, err := config.GetGDCUserClusterClients(releaseConfigData.GDC.UserClusters, releaseConfigData.GDCClient); err == nil {
			shootNamespace := fmt.Sprintf("shoot--%s--%s", shootKey.Namespace, shootKey.Name)
			for _, ucClient := range userClusterClients {
				cm := &corev1.ConfigMap{}
				if err := ucClient.Get(ctx, client.ObjectKey{Name: kvcsiAnnotationsAllowlistConfigMap, Namespace: shootNamespace}, cm); err == nil {
					// Always pause extension-controlplane-seed (which owns ConfigMap/kvcsi-annotations-allowlist via
					// resources.gardener.cloud/origin) so gardener-resource-manager never reverts the allowlist mid-run.
					for _, mrName := range []string{extensionControlPlaneSeedMRName, extensionControlPlaneShootMRName} {
						mr := &unstructured.Unstructured{}
						mr.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   "resources.gardener.cloud",
							Version: "v1alpha1",
							Kind:    "ManagedResource",
						})
						if err := ucClient.Get(ctx, client.ObjectKey{Name: mrName, Namespace: shootNamespace}, mr); err == nil {
							ann := mr.GetAnnotations()
							if ann[gardenerIgnoreAnnotationKey] != "true" {
								if ann == nil {
									ann = make(map[string]string)
								}
								ann[gardenerIgnoreAnnotationKey] = "true"
								mr.SetAnnotations(ann)
								_ = ucClient.Update(ctx, mr)
							}
						}
					}

					allowlist := cm.Data[kvcsiAnnotationsAllowlistDataKey]
					if !strings.Contains(allowlist, cdiBindImmediateRequestedAnnotation) {
						log.Printf("Updating %s/%s to allow %s...", shootNamespace, kvcsiAnnotationsAllowlistConfigMap, cdiBindImmediateRequestedAnnotation)
						if cm.Data == nil {
							cm.Data = make(map[string]string)
						}
						cm.Data[kvcsiAnnotationsAllowlistDataKey] = strings.TrimSpace(allowlist) + "\n" + cdiBindImmediateRequestedAnnotation + "\n"
						if err := ucClient.Update(ctx, cm); err == nil {
							// Restart kubevirt-csi-controller so it reloads the mounted allowlist ConfigMap immediately.
							deploy := &appsv1.Deployment{}
							deployKey := client.ObjectKey{Name: kubevirtCSIControllerDeploymentName, Namespace: shootNamespace}
							if err := ucClient.Get(ctx, deployKey, deploy); err == nil {
								patch := client.MergeFrom(deploy.DeepCopy())
								if deploy.Spec.Template.Annotations == nil {
									deploy.Spec.Template.Annotations = make(map[string]string)
								}
								deploy.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().UTC().Format(time.RFC3339)
								if err := ucClient.Patch(ctx, deploy, patch); err == nil {
									_ = kubernetes.WaitForDeploymentReady(ctx, ucClient, shootNamespace, kubevirtCSIControllerDeploymentName, seedPrometheusPVCBindTimeout)
								}
							}
						}
					}
				}
			}
		}
	}

	// Step 2: Connect to the Seed host Shoot cluster and pre-create (or recreate if Pending without annotations) the 3 garden Prometheus PVCs.
	shootClients, err := gardener.NewShootClient(ctx, remoteShootCluster.VirtualGarden.Client, shootKey)
	if err != nil || shootClients == nil || shootClients.WatchClient == nil {
		log.Printf("Warning: unable to connect to Seed host Shoot %s for Prometheus PVC pre-creation: %v", shootKey, err)
		return
	}
	targetClient := shootClients.WatchClient

	gardenNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: gardenNamespace,
		},
	}
	_ = targetClient.Create(ctx, gardenNS)

	fsMode := corev1.PersistentVolumeFilesystem
	instances := []string{"aggregate", "cache", "seed"}
	for _, instance := range instances {
		pvcName := fmt.Sprintf(prometheusPVCNameFormat, instance)
		pvcKey := client.ObjectKey{Name: pvcName, Namespace: gardenNamespace}
		existing := &corev1.PersistentVolumeClaim{}
		err := targetClient.Get(ctx, pvcKey, existing)
		if err == nil {
			if existing.Status.Phase == corev1.ClaimBound {
				continue
			}
			// csi-driver-gdch only copies Shoot PVC annotations to the infra PVC at CreateVolume time.
			// If an existing PVC is still Pending and lacks the CDI immediate-bind annotations, delete and recreate it.
			if existing.Annotations[cdiUsePopulatorAnnotationKey] != "false" ||
				existing.Annotations[cdiBindImmediateRequestedAnnotation] != "true" {
				log.Printf("Recreating Pending unannotated PVC %s/%s with CDI immediate-bind annotations...", gardenNamespace, pvcName)
				_ = targetClient.Delete(ctx, existing)
				_ = wait.PollUntilContextTimeout(ctx, seedPrometheusPVCPollInterval, seedPrometheusPVCBindTimeout, true, func(ctx context.Context) (bool, error) {
					check := &corev1.PersistentVolumeClaim{}
					if getErr := targetClient.Get(ctx, pvcKey, check); errors.IsNotFound(getErr) {
						return true, nil
					}
					return false, nil
				})
				err = errors.NewNotFound(corev1.Resource("persistentvolumeclaims"), pvcName)
			}
		}
		if errors.IsNotFound(err) {
			log.Printf("Pre-creating annotated Seed Prometheus PVC %s/%s...", gardenNamespace, pvcName)
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: gardenNamespace,
					Annotations: map[string]string{
						cdiUsePopulatorAnnotationKey:        "false",
						cdiBindImmediateRequestedAnnotation: "true",
					},
					Labels: map[string]string{
						"app.kubernetes.io/instance":   instance,
						"app.kubernetes.io/managed-by": "prometheus-operator",
						"app.kubernetes.io/name":       "prometheus",
						"operator.prometheus.io/name":  instance,
						"operator.prometheus.io/shard": "0",
						"prometheus":                   instance,
					},
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(seedPrometheusStorageSize),
						},
					},
					StorageClassName: ptr.To(gardenerFastStorageClass),
					VolumeMode:       &fsMode,
				},
			}
			if createErr := targetClient.Create(ctx, pvc); createErr != nil && !errors.IsAlreadyExists(createErr) {
				log.Printf("Warning: failed to pre-create PVC %s/%s: %v", gardenNamespace, pvcName, createErr)
			}
		}
	}

	// Step 3: Poll until all 3 garden Prometheus PVCs reach Bound before deploying gardenlet.
	if err := wait.PollUntilContextTimeout(ctx, seedPrometheusPVCPollInterval, seedPrometheusPVCBindTimeout, true, func(ctx context.Context) (bool, error) {
		for _, instance := range instances {
			pvcName := fmt.Sprintf(prometheusPVCNameFormat, instance)
			pvc := &corev1.PersistentVolumeClaim{}
			if err := targetClient.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: gardenNamespace}, pvc); err != nil {
				return false, nil
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		log.Printf("Warning: not all Seed Prometheus PVCs in %s bound within %s: %v", gardenNamespace, seedPrometheusPVCBindTimeout, err)
	} else {
		log.Printf("All 3 Seed Prometheus PVCs in %s are Bound.", gardenNamespace)
	}
}

// suppressRemoteShootTargetVPA disables the host Shoot's target VPA bindings on a Shooted Seed.
// Pre-provisioned Seed host Shoots have Shoot VPA enabled alongside the Seed's own VPA; across Seed
// teardowns, the host VUC's long-running vpa-recommender retains a stale CRD discovery cache for
// monitoring.coreos.com/v1.Prometheus and overwrites garden/prometheus-seed with ConfigUnsupported=True
// (blocking SeedSystemComponentsHealthy) while also conflicting on gardener.cloud:vpa:target:status-actor during Seed deletion.
func suppressRemoteShootTargetVPA(ctx context.Context, releaseConfigData *config.ReleaseTestConfig) {
	if releaseConfigData == nil || releaseConfigData.Seed == nil || releaseConfigData.Seed.HostCluster == nil || releaseConfigData.Seed.HostCluster.Shoot == nil {
		return
	}
	remoteShootCluster := releaseConfigData.Seed.HostCluster.Shoot
	if remoteShootCluster.VirtualGarden == nil {
		return
	}
	shootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}

	// Pause shoot-core-vpa ManagedResource reconciliation on the host VUC.
	if releaseConfigData != nil && releaseConfigData.GDC != nil && releaseConfigData.GDCClient != nil {
		if userClusterClients, err := config.GetGDCUserClusterClients(releaseConfigData.GDC.UserClusters, releaseConfigData.GDCClient); err == nil {
			shootNamespace := fmt.Sprintf("shoot--%s--%s", shootKey.Namespace, shootKey.Name)
			for _, ucClient := range userClusterClients {
				mr := &unstructured.Unstructured{}
				mr.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "resources.gardener.cloud",
					Version: "v1alpha1",
					Kind:    "ManagedResource",
				})
				if err := ucClient.Get(ctx, client.ObjectKey{Name: shootCoreVPAMRName, Namespace: shootNamespace}, mr); err == nil {
					ann := mr.GetAnnotations()
					if ann == nil {
						ann = make(map[string]string)
					}
					ann[gardenerIgnoreAnnotationKey] = "true"
					mr.SetAnnotations(ann)
					_ = ucClient.Update(ctx, mr)
				}
			}
		}
	}

	shootClients, err := gardener.NewShootClient(ctx, remoteShootCluster.VirtualGarden.Client, shootKey)
	if err != nil || shootClients == nil || shootClients.WatchClient == nil {
		return
	}
	targetClient := shootClients.WatchClient

	// Suspend target resource-manager RBAC and remove host Shoot VPA target bindings/webhook on the Seed host cluster.
	crb := &unstructured.Unstructured{}
	crb.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "rbac.authorization.k8s.io",
		Version: "v1",
		Kind:    "ClusterRoleBinding",
	})
	if err := targetClient.Get(ctx, client.ObjectKey{Name: "gardener.cloud:target:resource-manager"}, crb); err == nil {
		unstructured.RemoveNestedField(crb.Object, "subjects")
		_ = targetClient.Update(ctx, crb)
	}

	targetVPACRBs := []string{
		"gardener.cloud:vpa:target:actor",
		"gardener.cloud:vpa:target:admission-controller",
		"gardener.cloud:vpa:target:checkpoint-actor",
		"gardener.cloud:vpa:target:evictioner",
		"gardener.cloud:vpa:target:metrics-reader",
		"gardener.cloud:vpa:target:status-actor",
		"gardener.cloud:vpa:target:target-reader",
		"gardener.cloud:vpa:target:vpa-updater-in-place-binding",
	}
	for _, name := range targetVPACRBs {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "rbac.authorization.k8s.io",
			Version: "v1",
			Kind:    "ClusterRoleBinding",
		})
		obj.SetName(name)
		_ = targetClient.Delete(ctx, obj)
	}

	mwc := &unstructured.Unstructured{}
	mwc.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "admissionregistration.k8s.io",
		Version: "v1",
		Kind:    "MutatingWebhookConfiguration",
	})
	mwc.SetName("vpa-webhook-config-target")
	_ = targetClient.Delete(ctx, mwc)
}

// prepareGardenletHelmValues creates the configuration data and generates the Helm values
// required for deploying the Gardenlet chart. It reads from a template file to construct the final values.
func prepareGardenletHelmValues(releaseConfigData *config.ReleaseTestConfig) (map[string]interface{}, *seedTemplateData, error) {
	caData := releaseConfigData.TestShoot.VirtualGarden.CAData
	gardenClusterURL := releaseConfigData.TestShoot.VirtualGarden.Host
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
		GardenletChartURL:        fmt.Sprintf("oci://%s/private-cloud/gardenlet:%s", releaseConfig.CandidateRegistryURL(), strings.TrimPrefix(releaseConfigData.Gardener.GardenerVersion, "v")),
		GardenletImageRepository: "europe-docker.pkg.dev/gardener-project/releases/gardener/gardenlet",
		DualzoneProviderConfig:   dualzoneProviderConfig,
		BackupBucketCredSecret:   backupBucketCredSecret,
		CommitHash:               gardenerArtifactsVersion,
	}

	// Generate values from template
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

// waitForSeedReady polls the Seed object until all required conditions (SeedSystemComponentsHealthy,
// SeedExtensionsReady, SeedBackupBucketsReady, and GardenletReady) are true, indicating that the
// Seed is fully operational.
const stabilizationDuration = 5 * time.Minute

func waitForSeedReady(ctx context.Context, gardenClient client.WithWatch, seedKey client.ObjectKey, timeout time.Duration) error {
	var healthySince *time.Time

	return wait.PollUntilContextTimeout(ctx, time.Second*5, timeout, true, func(ctx context.Context) (bool, error) {
		seed := &gardencorev1beta1.Seed{}
		if err := gardenClient.Get(ctx, seedKey, seed); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		// SeedGardenletReady is a constant for a condition type indicating that Gardenlet is ready.
		// It is not defined in the gardencorev1beta1 API but still expected in seed readiness checks.
		const seedGardenletReady gardencorev1beta1.ConditionType = "GardenletReady"

		expectedConditions := map[gardencorev1beta1.ConditionType]struct{}{
			gardencorev1beta1.SeedSystemComponentsHealthy: {},
			gardencorev1beta1.SeedExtensionsReady:         {},
			gardencorev1beta1.SeedBackupBucketsReady:      {},
			seedGardenletReady:                            {},
		}

		for _, condition := range seed.Status.Conditions {
			if condition.Status == gardencorev1beta1.ConditionTrue {
				delete(expectedConditions, condition.Type)
			}
		}

		if len(expectedConditions) == 0 {
			now := time.Now()
			if healthySince == nil {
				healthySince = &now
				log.Printf("Seed %s has become healthy. Starting %s stabilization verification...", seedKey.Name, stabilizationDuration)
				return false, nil
			}

			if time.Since(*healthySince) >= stabilizationDuration {
				log.Printf("Seed %s remained healthy for the stabilization duration. Proceeding...", seedKey.Name)
				return true, nil
			}
			return false, nil
		}

		if healthySince != nil {
			log.Printf("Seed %s became unhealthy during stabilization. Resetting timer.", seedKey.Name)
			healthySince = nil
		}

		return false, nil
	})
}
