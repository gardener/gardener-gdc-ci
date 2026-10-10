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

package gardener

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	authenticationv1alpha1 "github.com/gardener/gardener/pkg/apis/authentication/v1alpha1"
	gardencorev1 "github.com/gardener/gardener/pkg/apis/core/v1"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gdchkubernetes "github.com/gardener/gardener-gdc-ci/integration/pkg/kubernetes"
)

const (
	// default expiration time for a generated kubeconfig
	kubeconfigExpiration = 2 * time.Hour
	// ImagePullSecretName is the name of the secret containing the credentials for the Harbor registry.
	ImagePullSecretName = "harbor-registry-secret-name"
)

// NewShootClient creates a watch client to the shoot cluster
func NewShootClient(ctx context.Context, gardenClient client.WithWatch, shootKey client.ObjectKey) (*gdchkubernetes.Clients, error) {
	// Get the raw kubeconfig bytes
	kubeconfigBytes, err := GetShootKubeconfig(ctx, gardenClient, shootKey)
	if err != nil {
		return nil, err // Error is already well-formatted
	}
	// Create a rest.Config directly from kubeconfig bytes.
	clientConfig, err := clientcmd.NewClientConfigFromBytes(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create client config from shoot kubeconfig bytes: %w", err)
	}
	shootRESTConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config from client config: %w", err)
	}

	// Create the watch Client using the REST config.
	shootWatchClient, err := client.NewWithWatch(shootRESTConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("could not create shoot controller-runtime client: %w", err)
	}

	// Create the Clientset
	shootClientset, err := kubernetes.NewForConfig(shootRESTConfig)
	if err != nil {
		return nil, fmt.Errorf("could not create shoot controller-runtime client: %w", err)
	}

	return &gdchkubernetes.Clients{
		WatchClient: shootWatchClient,
		Client:      shootClientset,
		Config:      shootRESTConfig,
	}, nil
}

// CreateImagePullSecret creates a secret in the garden namespace that is used to pull images from the Harbor registry.
func CreateImagePullSecret(ctx context.Context, kubeClient client.WithWatch, credentialData []byte) error {
	if len(credentialData) == 0 {
		return fmt.Errorf("credential data is empty")
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ImagePullSecretName,
			Namespace: gardenv1beta1constants.GardenNamespace,
		},
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := controllerutil.CreateOrUpdate(ctx, kubeClient, secret, func() error {
			secret.Labels = map[string]string{
				gardenv1beta1constants.GardenRole: gardenv1beta1constants.GardenRoleHelmPullSecret,
			}
			secret.Type = corev1.SecretTypeDockerConfigJson
			secret.Data = map[string][]byte{
				corev1.DockerConfigJsonKey: credentialData,
			}
			return nil
		})
		return updateErr
	})

	if err != nil {
		return fmt.Errorf("failed to create or update image pull secret %q: %v", secret.Name, err)
	}
	return nil
}

// WaitForShootReconciliation waits for Shoot's reconcilation state to be 'Succeeded'
func WaitForShootReconciliation(ctx context.Context, k8sClient client.WithWatch, shootKey client.ObjectKey, timeout time.Duration) error {
	listOptions := []client.ListOption{
		client.InNamespace(shootKey.Namespace),
		client.MatchingFields{"metadata.name": shootKey.Name},
	}
	startWatchFunc := func() (watch.Interface, error) {
		return k8sClient.Watch(ctx, &gardencorev1beta1.ShootList{}, listOptions...)
	}

	isShootReadyFunc := func(shoot *gardencorev1beta1.Shoot) bool {
		if shoot.Generation != shoot.Status.ObservedGeneration {
			fmt.Printf("Shoot %q/%q: waiting for status to observe generation %d (current observed: %d)...\n",
				shoot.Namespace, shoot.Name, shoot.Generation, shoot.Status.ObservedGeneration)
			return false
		}

		op := shoot.Status.LastOperation
		if op == nil {
			fmt.Printf("Shoot %q/%q: last operation is nil, waiting...\n", shoot.Namespace, shoot.Name)
			return false
		}
		var errorMsgs []string
		for _, e := range shoot.Status.LastErrors {
			errorMsgs = append(errorMsgs, e.Description)
		}
		lastError := strings.Join(errorMsgs, " | ")
		fmt.Printf("Shoot Status %q: Progress %d, LastError: %q\n", op.State, op.Progress, lastError)

		if op.State == gardencorev1beta1.LastOperationStateFailed {
			if shoot.Annotations[gardenv1beta1constants.GardenerOperation] != gardenv1beta1constants.ShootOperationRetry {
				fmt.Printf("Shoot %q/%q is in Failed state, applying retry annotation\n", shoot.Namespace, shoot.Name)

				err := UpdateShoot(ctx, k8sClient, shootKey, func(s *gardencorev1beta1.Shoot) {
					if s.Annotations == nil {
						s.Annotations = make(map[string]string)
					}
					s.Annotations[gardenv1beta1constants.GardenerOperation] = gardenv1beta1constants.ShootOperationRetry
				})

				if err != nil {
					fmt.Printf("Failed to apply retry annotation via UpdateShoot: %v\n", err)
				}
			}
		}

		return op.State == gardencorev1beta1.LastOperationStateSucceeded
	}

	err := gdchkubernetes.WaitForCondition[*gardencorev1beta1.Shoot](
		ctx,
		timeout,
		startWatchFunc,
		isShootReadyFunc,
	)

	if err != nil {
		return fmt.Errorf("shoot '%q' did not get ready in time: %w", shootKey.Name, err)
	}
	return nil
}

// NewGardenClient creates a watch client to the Garden cluster
func NewGardenClient(gardenKubeconfigPath string) (client.WithWatch, error) {
	s := scheme.Scheme
	if err := gardencorev1beta1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("failed to add gardencorev1beta1 scheme: %w", err)
	}
	if err := gardencorev1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("failed to add gardencorev1 scheme: %w", err)
	}

	clients, err := gdchkubernetes.NewClients(gardenKubeconfigPath, gdchkubernetes.WithScheme(s))
	if err != nil {
		return nil, fmt.Errorf("failed to create client-go clients from kubeconfig %s: %w", gardenKubeconfigPath, err)
	}
	return clients.WatchClient, nil
}

// deployExtension is a helper function that deploys a Gardener extension with the given configuration
func DeployExtension(ctx context.Context, gardenClient client.WithWatch, config *ExtensionConfig) error {
	valuesBytes, err := json.Marshal(config.Values)
	if err != nil {
		return fmt.Errorf("failed to marshal values for %s extension: %v", config.Name, err)
	}

	// Create or Update ControllerDeployment
	controllerDeployment := &gardencorev1.ControllerDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: config.Name,
		},
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := controllerutil.CreateOrUpdate(ctx, gardenClient, controllerDeployment, func() error {
			controllerDeployment.Helm = &gardencorev1.HelmControllerDeployment{
				OCIRepository: &gardencorev1.OCIRepository{
					Ref: config.HelmChartRef,
					PullSecretRef: &corev1.LocalObjectReference{
						Name: config.ImagePullSecretName,
					},
				},
				Values: &apiextensionsv1.JSON{Raw: valuesBytes},
			}
			return nil
		})
		return updateErr
	})

	if err != nil {
		return fmt.Errorf("failed to create or update controller deployment for %s extension: %v", config.Name, err)
	}

	// Create or Update ControllerRegistration
	controllerRegistration := &gardencorev1beta1.ControllerRegistration{
		ObjectMeta: metav1.ObjectMeta{
			Name: config.Name,
		},
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := controllerutil.CreateOrUpdate(ctx, gardenClient, controllerRegistration, func() error {
			controllerRegistration.Spec = gardencorev1beta1.ControllerRegistrationSpec{
				Deployment: &gardencorev1beta1.ControllerRegistrationDeployment{
					DeploymentRefs: []gardencorev1beta1.DeploymentRef{
						{
							Name: config.Name,
						},
					},
					Policy: ptr.To(gardencorev1beta1.ControllerDeploymentPolicyAlways),
				},
				Resources: config.Resources,
			}
			return nil
		})
		return updateErr
	})

	if err != nil {
		return fmt.Errorf("failed to create or update controller registration for %s extension: %v", config.Name, err)
	}

	return nil
}

// DeployOperatorExtension deploys the extension provider as Extension on runtime cluster.
func DeployOperatorExtension(ctx context.Context, runtimeClusterClient client.WithWatch, config *ExtopConfig) error {
	admissionValuesBytes, err := json.Marshal(config.AdmissionHelmChartValues)
	if err != nil {
		return fmt.Errorf("failed to marshal admission helm chart values for %s extension: %v", config.Name, err)
	}
	extensionProviderHelmChartValuesBytes, err := json.Marshal(config.ExtensionProviderHelmChartValues)
	if err != nil {
		return fmt.Errorf("failed to marshal extension provider helm chart values for %s extension: %v", config.Name, err)
	}
	extensionProviderRuntimeValuesBytes, err := json.Marshal(config.ExtensionProviderRuntimeValues)
	if err != nil {
		return fmt.Errorf("failed to marshal extension provider runtime values for %s extension: %v", config.Name, err)
	}

	extopDeployment := &operatorv1alpha1.Extension{
		ObjectMeta: metav1.ObjectMeta{
			Name: config.Name,
		},
	}
	// Retry on optimistic concurrency conflicts (409) when gardener-operator concurrently updates Extension status/finalizers.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, updateErr := controllerutil.CreateOrUpdate(ctx, runtimeClusterClient, extopDeployment, func() error {
			extopDeployment.Spec = operatorv1alpha1.ExtensionSpec{
				Deployment: &operatorv1alpha1.Deployment{
					AdmissionDeployment: &operatorv1alpha1.AdmissionDeploymentSpec{
						RuntimeCluster: &operatorv1alpha1.DeploymentSpec{
							Helm: &operatorv1alpha1.ExtensionHelm{
								OCIRepository: &gardencorev1.OCIRepository{
									Ref: config.AdmissionRuntimeHelmChartRef,
									PullSecretRef: &corev1.LocalObjectReference{
										Name: config.ImagePullSecretName,
									},
								},
							},
						},
						Values: &apiextensionsv1.JSON{Raw: admissionValuesBytes},
						VirtualCluster: &operatorv1alpha1.DeploymentSpec{
							Helm: &operatorv1alpha1.ExtensionHelm{
								OCIRepository: &gardencorev1.OCIRepository{
									Ref: config.AdmissionApplicationHelmChartRef,
									PullSecretRef: &corev1.LocalObjectReference{
										Name: config.ImagePullSecretName,
									},
								},
							},
						},
					},
					ExtensionDeployment: &operatorv1alpha1.ExtensionDeploymentSpec{
						DeploymentSpec: operatorv1alpha1.DeploymentSpec{
							Helm: &operatorv1alpha1.ExtensionHelm{
								OCIRepository: &gardencorev1.OCIRepository{
									Ref: config.ExtensionProviderHelmChartRef,
									PullSecretRef: &corev1.LocalObjectReference{
										Name: config.ImagePullSecretName,
									},
								},
							},
						},
						InjectGardenKubeconfig: ptr.To(true),
						RuntimeClusterValues:   &apiextensionsv1.JSON{Raw: extensionProviderRuntimeValuesBytes},
						Values:                 &apiextensionsv1.JSON{Raw: extensionProviderHelmChartValuesBytes},
					},
				},
				Resources: []gardencorev1beta1.ControllerResource{
					{Kind: "Infrastructure", Type: "gdch", Primary: ptr.To(true)},
					{Kind: "ControlPlane", Type: "gdch", Primary: ptr.To(true)},
					{Kind: "Worker", Type: "gdch", Primary: ptr.To(true)},
					{Kind: "BackupEntry", Type: "gdch", Primary: ptr.To(true)},
					{Kind: "BackupBucket", Type: "gdch", Primary: ptr.To(true)},
					{Kind: "DNSRecord", Type: "gdch-dns", Primary: ptr.To(true)},
				},
			}
			return nil
		})
		return updateErr
	})
	if err != nil {
		return fmt.Errorf("failed to create or update extop deployment for %s extension: %v", config.Name, err)
	}
	return nil
}

// GetShoot retrieves a Shoot object from the Kubernetes cluster.
func GetShoot(ctx context.Context, gardenClient client.WithWatch, objKey client.ObjectKey) (*gardencorev1beta1.Shoot, error) {
	shoot := &gardencorev1beta1.Shoot{}
	if err := gardenClient.Get(ctx, objKey, shoot); err != nil {
		return nil, fmt.Errorf("failed to get shoot Obj %s/%s: %v", objKey.Namespace, objKey.Name, err)
	}
	return shoot, nil
}

// UpdateShoot updates a Shoot resource. It uses controllerutil.CreateOrUpdate to handle potential
// conflicts with other controllers by retrying the operation.
func UpdateShoot(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey, mutate func(*gardencorev1beta1.Shoot)) error {
	shoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shootKey.Name,
			Namespace: shootKey.Namespace,
		},
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, shoot, func() error {
			mutate(shoot)
			return nil
		})
		return err
	})
}

// GetShootKubeconfig requests the admin kubeconfig for a Shoot and returns it as raw bytes.
var GetShootKubeconfig = func(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey) ([]byte, error) {
	// Create a token request with 2-hour expiration
	tokenRequest := &authenticationv1alpha1.AdminKubeconfigRequest{
		Spec: authenticationv1alpha1.AdminKubeconfigRequestSpec{
			ExpirationSeconds: ptr.To(int64(kubeconfigExpiration.Seconds())),
		},
	}

	shoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: shootKey.Namespace,
			Name:      shootKey.Name,
		},
	}

	// Use the SubResource client to make the adminkubeconfig request
	err := gardenClient.SubResource("adminkubeconfig").Create(ctx, shoot, tokenRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to request adminkubeconfig for shoot %s/%s: %w", shootKey.Namespace, shootKey.Name, err)
	}

	// Get the kubeconfig from the token request status
	kubeconfigBytes := tokenRequest.Status.Kubeconfig
	if len(kubeconfigBytes) == 0 {
		return nil, fmt.Errorf("adminkubeconfig response for shoot %s/%s contained an empty token", shootKey.Namespace, shootKey.Name)
	}

	return kubeconfigBytes, nil
}

// GetShootParentSubnetName returns the name of the parent subnet for a given commit hash.
func GetShootParentSubnetName(commitHash string) string {
	return fmt.Sprintf("release-subnet-%s", commitHash)
}

// GetShootKubeconfigPath retrieves the kubeconfig for a given Shoot cluster,
// writes it to a temporary file, and returns the path to the file.
func GetShootKubeconfigPath(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey) (string, error) {
	kubeconfigData, err := GetShootKubeconfig(ctx, gardenClient, shootKey)
	if err != nil {
		return "", fmt.Errorf("failed to get kubeconfig for shoot %s/%s: %w", shootKey.Namespace, shootKey.Name, err)
	}
	return writeToFile(fmt.Sprintf("%s-kubeconfig-*", shootKey.Name), kubeconfigData)
}

// writeToFile writes the given data to a temporary file with the given pattern
// and returns the path to that file. The file is closed after writing, and if a write error occurs,
// the file is removed.
func writeToFile(pattern string, data []byte) (string, error) {
	tmpFile, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to write to temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("failed to close temp file %s: %w", tmpFile.Name(), err)
	}

	return tmpFile.Name(), nil
}
