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

package garden_custom

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdch"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/helm"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/kubernetes"
)

const (
	extensionProviderName            = "provider-gdch"
	admissionGdchRuntimeChart        = "admission-gdch-runtime-helm"
	admissionGdchApplicationChart    = "admission-gdch-application-helm"
	extensionAdmissionGdchRepository = "gardener-extension-admission-gdch"
	virtualGardenProviderGDC         = "gdc"
	virtualGardenProviderGKE         = "gke"
)

var (
	gardenerArtifactsVersion     string
	mcmArtifactsVersion          string
	ccmArtifactsVersion          string
	virtualGardenProvider        string
	disableVirtualGardenBackup   string
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
)

// init registers the command-line flags.
func init() {
	flag.StringVar(&gardenerArtifactsVersion, "gardener-artifacts-version", "", "The version string for Gardener artifacts")
	flag.StringVar(&mcmArtifactsVersion, "mcm-artifacts-version", "", "The version string for machine-controller-manager-provider-gdc artifacts")
	flag.StringVar(&ccmArtifactsVersion, "ccm-artifacts-version", "", "The version string for cloud-provider-gdc artifacts")
	flag.StringVar(&virtualGardenProvider, "virtual-garden-provider", virtualGardenProviderGKE, "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
	flag.StringVar(&disableVirtualGardenBackup, "disable-virtual-garden-backup", "false", "disable etcd main backup for the virtual garden during testing ('true' or 'false')")
}

// TestCreateGardenCluster tests the creation of a seed cluster.
func TestCreateGardenCluster(t *testing.T) {
	validateFlags(t)
	ctx := context.Background()

	t.Logf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releaseConfigData, err := releaseConfig.LoadReleaseTestConfig(
		*releaseConfigurationFilePath,
		gardenerArtifactsVersion,
		releaseConfig.WithClusterAllocation(),
		releaseConfig.WithVirtualGardenProvider(virtualGardenProvider),
		releaseConfig.WithOnlyGarden(),
	)
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releaseConfigData != nil && releaseConfigData.GDCClient != nil {
		defer releaseConfigData.GDCClient.Cleanup()
	}

	runtimeClient := releaseConfigData.RuntimeClusterClient
	runtimeClusterName := releaseConfigData.RuntimeClusterName

	// 1. Fail if a Garden resource already exists on the runtime cluster
	t.Logf("Checking if Garden resource %q already exists on runtime cluster", runtimeClusterName)
	existingGarden := &unstructured.Unstructured{}
	existingGarden.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.gardener.cloud",
		Version: "v1alpha1",
		Kind:    "Garden",
	})
	err = runtimeClient.Get(ctx, client.ObjectKey{Name: runtimeClusterName}, existingGarden)
	if err == nil {
		t.Fatalf("Garden cluster resource %q already exists on runtime cluster. Please clean it up or let teardown run.", runtimeClusterName)
	} else if !apierrors.IsNotFound(err) && !strings.Contains(err.Error(), "no matches for kind") {
		t.Fatalf("failed to check for existing Garden resource: %v", err)
	}

	// 2. Ensure "garden" namespace exists and create image pull secret
	t.Log("Ensuring 'garden' namespace exists in runtime cluster")
	gardenNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "garden",
		},
	}
	if err := runtimeClient.Create(ctx, gardenNS); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Logf("Note: could not create 'garden' namespace (may already exist): %v", err)
	}
	t.Log("Creating image pull secret in runtime cluster")
	if err := gardener.CreateImagePullSecret(ctx, runtimeClient, releaseConfigData.ImagePullCredentials); err != nil {
		t.Fatalf("failed to create image pull secret: %v", err)
	}

	t.Log("Ensuring gardener-operator is installed on runtime cluster")
	if err := installGardenerOperator(ctx, t, runtimeClient, releaseConfigData); err != nil {
		t.Fatalf("failed to install gardener-operator: %v", err)
	}

	// 3. Deploy operator extension
	extopConfig, err := newGdchExtopConfig(releaseConfig.CandidateRegistryURL(), gardenerArtifactsVersion, releaseConfigData.Gardener)
	if err != nil {
		t.Fatalf("failed to create extop config: %v", err)
	}
	t.Log("Deploying operator extension on runtime cluster")
	if err := gardener.DeployOperatorExtension(ctx, runtimeClient, extopConfig); err != nil {
		t.Fatalf("failed to deploy gdch extension: %v", err)
	}
	t.Logf("Successfully deployed extension on runtime cluster.")

	// 4. Resolve Pod and Service CIDRs dynamically from the runtime cluster
	t.Log("Resolving Pod and Service subnets from runtime cluster")
	podCIDR, svcCIDR := releaseConfig.ResolveClusterCIDRs(ctx, runtimeClient)
	t.Logf("Resolved pod CIDR: %s", podCIDR)
	t.Logf("Resolved service CIDR: %s", svcCIDR)

	// Determine the selected cluster's zone
	var zone string
	for _, uc := range releaseConfigData.GDC.UserClusters {
		if uc.Name == runtimeClusterName {
			zone = uc.Zone
			break
		}
	}
	if zone == "" {
		zone = releaseConfigData.GDC.Zones[0]
	}
	t.Logf("Using zone %s for runtime cluster %s", zone, runtimeClusterName)

	// 5. Deploy Garden custom resource
	t.Logf("Deploying Garden custom resource %q", runtimeClusterName)
	garden := &unstructured.Unstructured{}
	garden.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "operator.gardener.cloud",
		Version: "v1alpha1",
		Kind:    "Garden",
	})
	garden.SetName(runtimeClusterName)

	spec := map[string]interface{}{
		"runtimeCluster": map[string]interface{}{
			"provider": map[string]interface{}{
				"region": releaseConfigData.GDC.Region,
			},
			"ingress": map[string]interface{}{
				"controller": map[string]interface{}{
					"kind": "nginx",
				},
				"domains": []interface{}{
					map[string]interface{}{
						"name": fmt.Sprintf("ingress.runtime-garden.%s.%s.%s.%s",
							runtimeClusterName,
							releaseConfigData.GDC.Project,
							zone,
							releaseConfigData.GDC.ManagedDNSDomainName),
						"provider": "primary",
					},
				},
			},
			"networking": map[string]interface{}{
				"pods": []interface{}{
					podCIDR,
				},
				"services": []interface{}{
					svcCIDR,
				},
			},
			"settings": map[string]interface{}{
				"topologyAwareRouting": map[string]interface{}{
					"enabled": false,
				},
				"verticalPodAutoscaler": map[string]interface{}{
					"enabled": true,
				},
			},
		},
		"virtualCluster": map[string]interface{}{
			"dns": map[string]interface{}{
				"domains": []interface{}{
					map[string]interface{}{
						"name": fmt.Sprintf("virtual-garden.%s.%s.%s.%s",
							runtimeClusterName,
							releaseConfigData.GDC.Project,
							zone,
							releaseConfigData.GDC.ManagedDNSDomainName),
						"provider": "primary",
					},
				},
			},
			"etcd": func() map[string]interface{} {
				etcdConfig := map[string]interface{}{
					"main": map[string]interface{}{},
					"events": map[string]interface{}{
						"storage": map[string]interface{}{
							"capacity": "10Gi",
						},
					},
				}
				if disableVirtualGardenBackup != "true" {
					etcdConfig["main"].(map[string]interface{})["backup"] = map[string]interface{}{
						"provider": "gdch",
						"secretRef": map[string]interface{}{
							"name": "virtual-garden-etcd-main-backup-gdch",
						},
					}
				}
				return etcdConfig
			}(),
			"gardener": map[string]interface{}{
				"clusterIdentity": "local",
			},
			"kubernetes": map[string]interface{}{
				"kubeAPIServer": map[string]interface{}{
					"eventTTL": "1h0m0s",
					"logging": map[string]interface{}{
						"verbosity": 2,
					},
					"requests": map[string]interface{}{
						"maxMutatingInflight":    200,
						"maxNonMutatingInflight": 400,
					},
				},
				"kubeControllerManager": map[string]interface{}{
					"certificateSigningDuration": "48h0m0s",
				},
				"version": releaseConfigData.Gardener.KubernetesVersion,
			},
			"maintenance": map[string]interface{}{
				"timeWindow": map[string]interface{}{
					"begin": "220000+0100",
					"end":   "230000+0100",
				},
			},
			"networking": map[string]interface{}{
				"services": []interface{}{
					"100.64.0.0/13",
				},
			},
		},
	}
	garden.Object["spec"] = spec

	if err := runtimeClient.Create(ctx, garden); err != nil {
		t.Fatalf("failed to create Garden custom resource: %v", err)
	}
	t.Logf("Garden custom resource %q created successfully", runtimeClusterName)

	// 5.1 Annotate the Garden resource with the artifacts version so that teardown can match it
	t.Logf("Annotating Garden resource %q with artifacts version %q", runtimeClusterName, gardenerArtifactsVersion)
	patch := client.MergeFrom(garden.DeepCopyObject().(client.Object))
	annotations := garden.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[releaseConfig.CommitHashAnnotation] = gardenerArtifactsVersion
	garden.SetAnnotations(annotations)
	if err := runtimeClient.Patch(ctx, garden, patch); err != nil {
		t.Fatalf("failed to annotate Garden custom resource: %v", err)
	}

	// 5.2 Deploy Ingress ProjectNetworkPolicy on the management cluster to allow traffic to the virtual garden ingress gateway
	t.Logf("Deploying Ingress ProjectNetworkPolicy for user cluster %q", runtimeClusterName)
	mgmtClient, ok := releaseConfigData.ManagementClients[zone]
	if !ok {
		t.Fatalf("failed to get management client for zone %s to deploy ProjectNetworkPolicy", zone)
	}
	pnp := &unstructured.Unstructured{}
	pnp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.gdc.goog",
		Version: "v1",
		Kind:    "ProjectNetworkPolicy",
	})
	pnp.SetName(fmt.Sprintf("allow-ingress-to-%s-vuc", runtimeClusterName))
	pnp.SetNamespace(releaseConfigData.GDC.Project)

	pnpSpec := map[string]interface{}{
		"policyType": "Ingress",
		"subject": map[string]interface{}{
			"subjectType": "UserWorkload",
			"userWorkloadSelector": map[string]interface{}{
				"labelSelector": map[string]interface{}{
					"clusters": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"kubernetes.io/metadata.name": runtimeClusterName,
						},
					},
					"namespaces": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"istio": "ingressgateway",
						},
					},
				},
			},
		},
		"ingress": []interface{}{
			map[string]interface{}{},
		},
	}
	pnp.Object["spec"] = pnpSpec

	if err := mgmtClient.Create(ctx, pnp); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("failed to create ProjectNetworkPolicy: %v", err)
	}
	t.Logf("ProjectNetworkPolicy %s created successfully on management cluster", pnp.GetName())

	// 5.3 Wait for the Garden CR to become ready and successfully reconciled
	t.Logf("Waiting for Garden CR %q to be reconciled successfully...", runtimeClusterName)
	pollErr := wait.PollUntilContextTimeout(ctx, 10*time.Second, 35*time.Minute, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "operator.gardener.cloud",
			Version: "v1alpha1",
			Kind:    "Garden",
		})
		if err := runtimeClient.Get(ctx, client.ObjectKey{Name: runtimeClusterName}, g); err != nil {
			t.Logf("Failed to get Garden CR: %v", err)
			return false, nil
		}

		lastOperation, found, err := unstructured.NestedMap(g.Object, "status", "lastOperation")
		if err != nil || !found || lastOperation == nil {
			t.Log("Garden CR status.lastOperation not populated yet...")
			return false, nil
		}

		state, found, err := unstructured.NestedString(lastOperation, "state")
		if err != nil || !found {
			t.Log("Garden CR status.lastOperation.state not found...")
			return false, nil
		}

		progress, _, _ := unstructured.NestedInt64(lastOperation, "progress")
		desc, _, _ := unstructured.NestedString(lastOperation, "description")
		if desc != "" {
			t.Logf("Garden CR Reconciliation State: %q, Progress: %d%%, Description: %s", state, progress, desc)
		} else {
			t.Logf("Garden CR Reconciliation State: %q, Progress: %d%%", state, progress)
		}

		if state == "Error" {
			lastErrors, foundErrors, _ := unstructured.NestedSlice(g.Object, "status", "lastErrors")
			if foundErrors && len(lastErrors) > 0 {
				for i, le := range lastErrors {
					if errMap, ok := le.(map[string]interface{}); ok {
						if errDesc, ok := errMap["description"].(string); ok {
							t.Logf("  [LastError %d] %s", i+1, errDesc)
						}
					}
				}
			}

			conditions, foundConds, _ := unstructured.NestedSlice(g.Object, "status", "conditions")
			if foundConds && len(conditions) > 0 {
				for _, cond := range conditions {
					if condMap, ok := cond.(map[string]interface{}); ok {
						condType, _ := condMap["type"].(string)
						condStatus, _ := condMap["status"].(string)
						condMsg, _ := condMap["message"].(string)
						if condStatus != "True" && condMsg != "" {
							t.Logf("  [Condition %s=%s] %s", condType, condStatus, condMsg)
						}
					}
				}
			}
		}

		if state == "Failed" {
			return false, fmt.Errorf("Garden CR reconciliation failed: %s", desc)
		}

		return state == "Succeeded", nil
	})
	if pollErr != nil {
		t.Fatalf("Garden CR did not reconcile successfully in time: %v", pollErr)
	}
	t.Log("Garden CR reconciled successfully!")

	// 5.4 Reconcile ResourceRecordSet in Global API with the Virtual Garden LoadBalancer IP
	if virtualGardenProvider == virtualGardenProviderGDC {
		if err := reconcileVirtualGardenDNSRecord(ctx, t, releaseConfigData, runtimeClusterName, zone); err != nil {
			t.Fatalf("failed to reconcile Virtual Garden DNS ResourceRecordSet: %v", err)
		}
	}

	// 6. Verify Virtual Garden API health (timeout 20 minutes)
	t.Log("Waiting for Virtual Garden API to become healthy...")
	var virtualGardenConfig *config.VirtualGardenConfig
	pollErr = wait.PollUntilContextTimeout(ctx, 10*time.Second, 20*time.Minute, true, func(ctx context.Context) (bool, error) {
		var err error
		virtualGardenConfig, err = config.GetGardenClient(ctx, runtimeClient)
		if err != nil {
			t.Logf("Virtual Garden API not ready yet: %v", err)
			return false, nil
		}
		return true, nil
	})
	if pollErr != nil {
		t.Fatalf("Virtual Garden API did not become healthy in time: %v", pollErr)
	}
	t.Logf("Virtual Garden API is healthy. Host: %s", virtualGardenConfig.Host)

	// 7. Verify Virtual Garden API accessibility via DNS (if DNS domain is enabled)
	if releaseConfigData.GDC != nil && releaseConfigData.GDC.ManagedDNSDomainName != "" {
		t.Log("Verifying Virtual Garden API accessibility via public DNS...")
		dnsClient, err := getOriginalGardenClient(ctx, runtimeClient)
		if err != nil {
			t.Fatalf("failed to create DNS-based garden client: %v", err)
		}

		pollErr = wait.PollUntilContextTimeout(ctx, 10*time.Second, 1*time.Minute, true, func(ctx context.Context) (bool, error) {
			nsList := &corev1.NamespaceList{}
			if err := dnsClient.List(ctx, nsList, client.Limit(1)); err != nil {
				t.Logf("Virtual Garden API not accessible via DNS yet: %v", err)
				return false, nil
			}
			return true, nil
		})
		if pollErr != nil {
			t.Logf("Warning: Virtual Garden API did not become accessible via DNS in 1 minute: %v. Falling back to direct Host IP verification...", pollErr)

			// Build fallback direct Host IP client
			restConfigDirect, err := getOriginalGardenRestConfig(ctx, runtimeClient)
			if err != nil {
				t.Fatalf("failed to build rest config for fallback client: %v", err)
			}

			origHost := restConfigDirect.Host
			if !strings.HasPrefix(origHost, "http://") && !strings.HasPrefix(origHost, "https://") {
				origHost = "https://" + origHost
			}
			u, err := url.Parse(origHost)
			if err != nil {
				t.Fatalf("failed to parse original host %s: %v", restConfigDirect.Host, err)
			}
			restConfigDirect.TLSClientConfig.ServerName = u.Hostname()
			restConfigDirect.Host = virtualGardenConfig.Host

			directClient, err := client.New(restConfigDirect, client.Options{Scheme: scheme.Scheme})
			if err != nil {
				t.Fatalf("failed to create direct Host IP client: %v", err)
			}

			t.Logf("Verifying accessibility via direct Host IP %s...", virtualGardenConfig.Host)
			nsList := &corev1.NamespaceList{}
			if err := directClient.List(ctx, nsList, client.Limit(1)); err != nil {
				t.Fatalf("Virtual Garden API is not accessible via direct Host IP: %v", err)
			}
			t.Log("Virtual Garden API is fully functional and accessible via direct Host IP!")
		} else {
			t.Log("Virtual Garden API is fully accessible via public DNS!")
		}
	}
}

// validateFlags checks if all the required command-line flags are provided.
func validateFlags(t *testing.T) {
	if *releaseConfigurationFilePath == "" {
		t.Fatal("flag --release-configuration-file-path must be set")
	}

	if gardenerArtifactsVersion == "" {
		t.Fatal("--gardener-artifacts-version is a required flag")
	}
}

type imageOverwriteOptions struct {
	HarborInstanceURL string
	ImageTag          string
	MCMImageTag       string
	CCMImageTag       string
	GardenerConfig    *config.GardenerConfig
}

// newGdchExtopConfig creates a new ExtopConfig for the GDCH extension provider.
func newGdchExtopConfig(harborInstanceURL, imageTag string, gardenerConfig *config.GardenerConfig) (*gardener.ExtopConfig, error) {
	imageOverwrite, err := extensionProviderImageOverwrite(imageOverwriteOptions{
		HarborInstanceURL: harborInstanceURL,
		ImageTag:          imageTag,
		MCMImageTag:       mcmArtifactsVersion,
		CCMImageTag:       ccmArtifactsVersion,
		GardenerConfig:    gardenerConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create image vector: %w", err)
	}
	chartVersion := strings.TrimPrefix(gardenerArtifactsVersion, "v")

	return &gardener.ExtopConfig{
		Name:                             extensionProviderName,
		AdmissionRuntimeHelmChartRef:     ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, admissionGdchRuntimeChart, chartVersion)),
		AdmissionApplicationHelmChartRef: ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, admissionGdchApplicationChart, chartVersion)),
		AdmissionHelmChartValues: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, extensionAdmissionGdchRepository),
				"tag":        imageTag,
			},
			"replicaCount": 1,
		},
		ExtensionProviderHelmChartRef: ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchHelmChartName, chartVersion)),
		ExtensionProviderRuntimeValues: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchImageName),
				"tag":        imageTag,
				"pullPolicy": "Always",
			},
		},
		ExtensionProviderHelmChartValues: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchImageName),
				"tag":        imageTag,
				"pullPolicy": "Always",
			},
			"imageVectorOverwrite": imageOverwrite,
		},
		ImagePullSecretName: gardener.ImagePullSecretName,
	}, nil
}

// extensionProviderImageOverwrite generates the image vector overwrite YAML for the extension provider.
func extensionProviderImageOverwrite(opts imageOverwriteOptions) (string, error) {
	type image struct {
		Name       string `yaml:"name"`
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
	}

	if opts.GardenerConfig == nil || opts.GardenerConfig.CSIImages == nil {
		return "", fmt.Errorf("gardener CSIImages configuration is missing")
	}
	csiImages := opts.GardenerConfig.CSIImages
	mcmTag := opts.MCMImageTag
	if mcmTag == "" {
		mcmTag = opts.ImageTag
	}
	ccmTag := opts.CCMImageTag
	if ccmTag == "" {
		ccmTag = opts.ImageTag
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

func getOperatorChartPath(releaseConfigData *config.ReleaseTestConfig) (string, error) {
	if releaseConfigData.Gardener != nil && releaseConfigData.Gardener.OperatorChartRepository != "" {
		repo := releaseConfigData.Gardener.OperatorChartRepository
		if strings.HasPrefix(repo, "oci://") || strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") {
			// Check if a version tag is already specified in the URL (excluding the scheme colon)
			if lastColon := strings.LastIndex(repo, ":"); lastColon > 5 {
				return repo, nil
			}
			if strings.HasPrefix(repo, "oci://") && releaseConfigData.Gardener.GardenerVersion != "" {
				version := strings.TrimPrefix(releaseConfigData.Gardener.GardenerVersion, "v")
				return fmt.Sprintf("%s:%s", repo, version), nil
			}
			return repo, nil
		}
		return repo, nil
	}

	paths := []string{
		"charts/operator",
		"../garden/charts/operator",
		"../../release/garden/charts/operator",
		"/charts/operator",
		".",
		"/",
	}
	for _, p := range paths {
		if fi, err := os.Stat(filepath.Join(p, "Chart.yaml")); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("could not locate gardener-operator helm chart (Chart.yaml not found in any of: %v)", paths)
}

func getRuntimeClusterKubeconfig(releaseConfigData *config.ReleaseTestConfig) (string, error) {
	if releaseConfigData.GDC != nil && releaseConfigData.GDCClient != nil && len(releaseConfigData.GDC.UserClusters) > 0 {
		for _, u := range releaseConfigData.GDC.UserClusters {
			if u.Name == releaseConfigData.RuntimeClusterName {
				return gdch.GetUserClusterKubeconfig(releaseConfigData.GDCClient, u.Zone, u.Project, u.Name)
			}
		}
	}
	return "", fmt.Errorf("could not resolve kubeconfig path for runtime cluster %s", releaseConfigData.RuntimeClusterName)
}

func installGardenerOperator(ctx context.Context, t *testing.T, runtimeClient client.WithWatch, releaseConfigData *config.ReleaseTestConfig) error {
	t.Log("Installing or upgrading gardener-operator Helm chart on runtime cluster...")

	chartPath, err := getOperatorChartPath(releaseConfigData)
	if err != nil {
		return fmt.Errorf("failed to locate operator chart: %w", err)
	}

	if releaseConfigData.GDC == nil || releaseConfigData.GDC.HarborRegistryURL == "" {
		return fmt.Errorf("GDC.HarborRegistryURL is required in release configuration to install gardener-operator")
	}
	harborRegistry := releaseConfigData.GDC.HarborRegistryURL
	if releaseConfigData.Gardener == nil || releaseConfigData.Gardener.GardenerVersion == "" {
		return fmt.Errorf("Gardener.GardenerVersion is required in release configuration to install gardener-operator")
	}
	gardenerVer := releaseConfigData.Gardener.GardenerVersion

	values := map[string]interface{}{
		"replicaCount":                  1,
		"serviceAccountName":            "gardener-operator",
		"invalidateServiceAccountToken": true,
		"image": map[string]interface{}{
			"repository": fmt.Sprintf("%s/private-cloud/operator", harborRegistry),
			"tag":        gardenerVer,
			"pullPolicy": "Always",
		},
		"imagePullSecrets": []map[string]interface{}{
			{
				"name": gardener.ImagePullSecretName,
			},
		},
	}

	if releaseConfigData.GDC != nil && len(releaseConfigData.GDC.UserClusters) > 0 {
		values["additionalVolumes"] = []interface{}{
			map[string]interface{}{
				"name": "gdc-certs",
				"configMap": map[string]interface{}{
					"name": "trust-store-root-ext",
				},
			},
		}
		values["additionalVolumeMounts"] = []interface{}{
			map[string]interface{}{
				"name":      "gdc-certs",
				"mountPath": "/etc/ssl/certs/gdc-ca.crt",
				"subPath":   "ca.crt",
				"readOnly":  true,
			},
		}
	}

	kubeconfigPath, err := getRuntimeClusterKubeconfig(releaseConfigData)
	if err != nil {
		return fmt.Errorf("failed to get runtime cluster kubeconfig: %w", err)
	}

	opts := helm.InstallOptions{
		ChartPath:      chartPath,
		KubeconfigPath: kubeconfigPath,
		ReleaseName:    "gardener-operator",
		Namespace:      "garden",
		Values:         values,
	}

	if _, err := helm.InstallOrUpgrade(opts); err != nil {
		return fmt.Errorf("failed to install gardener-operator helm chart: %w", err)
	}

	t.Log("Waiting for gardener-operator deployment to become ready in namespace garden...")
	if err := kubernetes.WaitForDeploymentReady(ctx, runtimeClient, "garden", "gardener-operator", 3*time.Minute); err != nil {
		return fmt.Errorf("gardener-operator deployment not ready within 3 minutes: %w", err)
	}
	t.Log("gardener-operator installed and ready.")
	return nil
}

func getOriginalGardenRestConfig(ctx context.Context, userClusterClient client.Client) (*rest.Config, error) {
	secret := &corev1.Secret{}
	err := userClusterClient.Get(
		ctx,
		client.ObjectKey{
			Namespace: "garden",
			Name:      "gardener",
		},
		secret,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get gardener secret: %w", err)
	}

	kubeconfigData, exists := secret.Data["kubeconfig"]
	if !exists {
		return nil, fmt.Errorf("kubeconfig not found in secret data")
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from kubeconfig content: %w", err)
	}
	return restConfig, nil
}

func getOriginalGardenClient(ctx context.Context, userClusterClient client.Client) (client.Client, error) {
	restConfig, err := getOriginalGardenRestConfig(ctx, userClusterClient)
	if err != nil {
		return nil, err
	}

	s := scheme.Scheme
	gardenClient, err := client.New(restConfig, client.Options{Scheme: s})
	if err != nil {
		return nil, fmt.Errorf("failed to create original garden client: %w", err)
	}
	return gardenClient, nil
}

// canReconcileGlobalDNSRecord checks if all prerequisites for GDC Global API DNS reconciliation are met.
func canReconcileGlobalDNSRecord(cfg *config.ReleaseTestConfig) bool {
	return cfg != nil &&
		cfg.GlobalAPIClient != nil &&
		cfg.GDC != nil &&
		cfg.GDC.ManagedDNSDomainName != ""
}

// reconcileVirtualGardenDNSRecord ensures the ResourceRecordSet on the Global API cluster
// points to the live external LoadBalancer IP allocated to virtual-garden-istio-ingress/istio-ingressgateway.
func reconcileVirtualGardenDNSRecord(ctx context.Context, t *testing.T, releaseConfigData *config.ReleaseTestConfig, runtimeClusterName, zone string) error {
	t.Helper()
	if !canReconcileGlobalDNSRecord(releaseConfigData) {
		t.Log("Skipping ResourceRecordSet creation: GlobalAPIClient, GDC config, or ManagedDNSDomainName is nil/empty")
		return nil
	}
	runtimeClient := releaseConfigData.RuntimeClusterClient
	if runtimeClient == nil {
		return fmt.Errorf("releaseConfigData.RuntimeClusterClient is nil")
	}
	if zone == "" && len(releaseConfigData.GDC.Zones) > 0 {
		zone = releaseConfigData.GDC.Zones[0]
	}

	// 1. Wait for istio-ingressgateway LoadBalancer service to get an external IP
	t.Log("Waiting for virtual-garden-istio-ingress/istio-ingressgateway external LoadBalancer IP...")
	var ingressIP string
	pollErr := wait.PollUntilContextTimeout(ctx, 10*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		svc := &corev1.Service{}
		if err := runtimeClient.Get(ctx, client.ObjectKey{
			Namespace: "virtual-garden-istio-ingress",
			Name:      "istio-ingressgateway",
		}, svc); err != nil {
			t.Logf("Waiting for istio-ingressgateway service: %v", err)
			return false, nil
		}
		if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
			ingressIP = svc.Status.LoadBalancer.Ingress[0].IP
			return true, nil
		}
		t.Log("istio-ingressgateway does not have status.loadBalancer.ingress IP yet...")
		return false, nil
	})
	if pollErr != nil {
		return fmt.Errorf("failed to get istio-ingressgateway external IP: %w", pollErr)
	}
	t.Logf("Found virtual-garden istio-ingressgateway external IP: %s", ingressIP)

	// 2. Discover ManagedDNSZone
	zoneName := "public-sap-gardener-zone"
	zoneList := &unstructured.UnstructuredList{}
	zoneList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.global.gdc.goog",
		Version: "v1",
		Kind:    "ManagedDNSZoneList",
	})
	if err := releaseConfigData.GlobalAPIClient.List(ctx, zoneList, client.InNamespace(releaseConfigData.GDC.Project)); err == nil && len(zoneList.Items) > 0 {
		for _, z := range zoneList.Items {
			dnsName, _, _ := unstructured.NestedString(z.Object, "spec", "dnsName")
			if strings.TrimSuffix(dnsName, ".") == strings.TrimSuffix(releaseConfigData.GDC.ManagedDNSDomainName, ".") {
				zoneName = z.GetName()
				t.Logf("Discovered ManagedDNSZone %q for domain %q", zoneName, dnsName)
				break
			}
		}
	}

	// 3. Construct ResourceRecordSet details
	dnsRecordName := fmt.Sprintf("%s-%s-%s-virtual-garden", runtimeClusterName, releaseConfigData.GDC.Project, zone)
	fqdn := fmt.Sprintf("api.virtual-garden.%s.%s.%s.%s", runtimeClusterName, releaseConfigData.GDC.Project, zone, releaseConfigData.GDC.ManagedDNSDomainName)

	rrs := &unstructured.Unstructured{}
	rrs.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "networking.global.gdc.goog",
		Version: "v1",
		Kind:    "ResourceRecordSet",
	})
	rrsKey := client.ObjectKey{
		Namespace: releaseConfigData.GDC.Project,
		Name:      dnsRecordName,
	}

	t.Logf("Creating/Updating ResourceRecordSet %q in namespace %q (FQDN: %s -> %s)...", dnsRecordName, releaseConfigData.GDC.Project, fqdn, ingressIP)
	err := releaseConfigData.GlobalAPIClient.Get(ctx, rrsKey, rrs)
	if err != nil {
		if apierrors.IsNotFound(err) {
			rrs.SetName(dnsRecordName)
			rrs.SetNamespace(releaseConfigData.GDC.Project)
			rrs.Object["spec"] = map[string]interface{}{
				"dnsZone":    zoneName,
				"name":       fqdn,
				"type":       "A",
				"rrData":     []interface{}{ingressIP},
				"ttlSeconds": int64(60),
			}
			if createErr := releaseConfigData.GlobalAPIClient.Create(ctx, rrs); createErr != nil {
				return fmt.Errorf("failed to create ResourceRecordSet %q: %w", dnsRecordName, createErr)
			}
			t.Logf("Successfully created ResourceRecordSet %q", dnsRecordName)
		} else {
			return fmt.Errorf("failed to check existing ResourceRecordSet %q: %w", dnsRecordName, err)
		}
	} else {
		spec, found, _ := unstructured.NestedMap(rrs.Object, "spec")
		if !found || spec == nil {
			spec = make(map[string]interface{})
		}
		spec["dnsZone"] = zoneName
		spec["name"] = fqdn
		spec["type"] = "A"
		spec["rrData"] = []interface{}{ingressIP}
		spec["ttlSeconds"] = int64(60)
		rrs.Object["spec"] = spec
		if updateErr := releaseConfigData.GlobalAPIClient.Update(ctx, rrs); updateErr != nil {
			return fmt.Errorf("failed to update ResourceRecordSet %q: %w", dnsRecordName, updateErr)
		}
		t.Logf("Successfully updated ResourceRecordSet %q with IP %s", dnsRecordName, ingressIP)
	}

	return nil
}
