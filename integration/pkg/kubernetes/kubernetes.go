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

package kubernetes

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	clientwatch "k8s.io/client-go/tools/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Clients struct {
	// WatchClient is the high-level controller-runtime client for CRUD and Watch.
	WatchClient client.WithWatch
	// Client is the standard client-go clientset for special actions like Exec.
	Client *kubernetes.Clientset
	// Config is the REST configuration, needed for the SPDY executor.
	Config *rest.Config
}

type newClientsOptions struct {
	Scheme *runtime.Scheme
}

// newClientsOption is a function that configures the Clients.
type newClientsOption func(*newClientsOptions)

// WithScheme provides an option to set a custom scheme for the clients.
func WithScheme(scheme *runtime.Scheme) newClientsOption {
	return func(opts *newClientsOptions) {
		opts.Scheme = scheme
	}
}

func NewClients(kubeconfigPath string, opts ...newClientsOption) (*Clients, error) {
	options := &newClientsOptions{
		Scheme: scheme.Scheme,
	}

	for _, opt := range opts {
		opt(options)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("could not build config: %w", err)
	}

	watchClient, err := client.NewWithWatch(config, client.Options{Scheme: options.Scheme})
	if err != nil {
		return nil, fmt.Errorf("could not create controller-runtime client: %w", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("could not create client-go clientset: %w", err)
	}

	return &Clients{
		WatchClient: watchClient,
		Client:      client,
		Config:      config,
	}, nil
}

func CreateNamespace(ctx context.Context, k8sclient client.WithWatch, name string) error {
	namespaceObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	if err := k8sclient.Create(ctx, namespaceObj); err != nil {
		return fmt.Errorf("failed to create Namespace: %w", err)
	}
	return nil
}

func CleanupResources(t *testing.T, k8sclient client.WithWatch, namespace string) {
	t.Helper()

	ctx := context.Background()
	namespaceObj := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	t.Logf("Cleaning up resources in namespace %s", namespace)
	if err := k8sclient.Delete(ctx, namespaceObj); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("Failed to cascade deleting resources in namespace \"%s\": %v", namespace, err)
	} else {
		t.Logf("Cascade deleted all the resources in namespace \"%s\"", namespace)
	}
}

func WaitForPodReady(ctx context.Context, k8sclient client.WithWatch, namespace, podName string, timeout time.Duration) error {
	podList := &corev1.PodList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": podName},
	}

	err := WaitForCondition[*corev1.Pod](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, podList, listOptions...)
		},
		func(pod *corev1.Pod) bool {
			if pod.Status.Phase != corev1.PodRunning {
				return false
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true
				}
			}
			return false
		},
	)
	if err != nil {
		return fmt.Errorf("Pod %s was not Ready: %v", podName, err)
	}
	return nil
}

// WaitForDeploymentReady waits for a Deployment's 'Available' condition to be 'True'.
func WaitForDeploymentReady(ctx context.Context, k8sclient client.WithWatch, namespace, deploymentName string, timeout time.Duration) error {
	deploymentList := &appsv1.DeploymentList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": deploymentName},
	}

	err := WaitForCondition[*appsv1.Deployment](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, deploymentList, listOptions...)
		},
		func(deployment *appsv1.Deployment) bool {
			// First, ensure the controller has observed the latest generation of the spec
			if deployment.Status.ObservedGeneration < deployment.Generation {
				return false
			}

			// Check if all replicas are updated and available
			replicas := ptr.Deref(deployment.Spec.Replicas, 1)
			if deployment.Status.UpdatedReplicas == replicas &&
				deployment.Status.AvailableReplicas == replicas {
				return true
			}

			return false
		},
	)
	if err != nil {
		return fmt.Errorf("deployment %s was not ready: %w", deploymentName, err)
	}
	return nil
}

// WaitForPVCBound waits for PVC status to be 'Bound'
func WaitForPVCBound(ctx context.Context, k8sclient client.WithWatch, namespace, pvcName string, timeout time.Duration) error {
	pvcList := &corev1.PersistentVolumeClaimList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": pvcName},
	}
	err := WaitForCondition[*corev1.PersistentVolumeClaim](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, pvcList, listOptions...)
		},
		func(pvc *corev1.PersistentVolumeClaim) bool {
			return pvc.Status.Phase == corev1.ClaimBound
		},
	)
	if err != nil {
		return fmt.Errorf("PVC %s did not bind: %v", pvcName, err)
	}
	return nil
}

func WaitForPVCResized(ctx context.Context, k8sclient client.WithWatch, namespace, pvcName string, newSize resource.Quantity, timeout time.Duration) error {
	pvcList := &corev1.PersistentVolumeClaimList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": pvcName},
	}
	err := WaitForCondition[*corev1.PersistentVolumeClaim](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, pvcList, listOptions...)
		},
		func(pvc *corev1.PersistentVolumeClaim) bool {
			currentSize, ok := pvc.Status.Capacity[corev1.ResourceStorage]
			if !ok {
				return false
			}
			if currentSize.Cmp(newSize) >= 0 {
				return true
			}
			return false
		},
	)
	if err != nil {
		return fmt.Errorf("PVC %s in %s namespace failed to expand in time: %v", pvcName, namespace, err)
	}
	return nil
}

func WaitForLoadBalancerIP(ctx context.Context, k8sclient client.WithWatch, namespace, svcName string, timeout time.Duration) error {
	svcList := &corev1.ServiceList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{"metadata.name": svcName},
	}
	err := WaitForCondition[*corev1.Service](
		ctx, timeout,
		func() (watch.Interface, error) {
			return k8sclient.Watch(ctx, svcList, listOptions...)
		},
		func(svc *corev1.Service) bool {
			return svc.Status.LoadBalancer.Ingress != nil
		},
	)
	if err != nil {
		return fmt.Errorf("could not allocate external IP for LB service %s in namespace %q within %v: %w", svcName, namespace, timeout, err)
	}
	return nil
}

func EnsurePod(ctx context.Context, watchClient client.WithWatch, podSpec *corev1.Pod, timeout time.Duration) (*corev1.Pod, error) {
	err := watchClient.Create(ctx, podSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to create pod %s in namespace %s: %v", podSpec.Name, podSpec.Namespace, err)
	}

	if err := WaitForPodReady(ctx, watchClient, podSpec.Namespace, podSpec.Name, timeout); err != nil {
		return nil, err
	}

	pod := &corev1.Pod{}
	err = watchClient.Get(ctx, client.ObjectKey{Name: podSpec.Name, Namespace: podSpec.Namespace}, pod)
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve Pod %s in namespace %s: %v", podSpec.Name, podSpec.Namespace, err)
	}

	return pod, nil
}

func ExecPod(ctx context.Context, client kubernetes.Interface, restCfg *rest.Config, command []string, namespace, podName string) error {
	req := client.CoreV1().RESTClient().
		Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: command,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		}, scheme.ParameterCodec)

	var stdout, stderr bytes.Buffer
	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %v", err)
	}
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("Command failed: %v, stderr: %s", err, stderr.String())
	}
	fmt.Printf("Command output :\n%s\n", stdout.String())
	return nil
}

// WaitForCondition watches a Kubernetes resource until the isReady predicate returns true
// or the timeout is reached.
func WaitForCondition[T runtime.Object](
	ctx context.Context,
	timeout time.Duration,
	startWatch func() (watch.Interface, error),
	isReady func(obj T) bool,
) error {
	// PollUntilContextTimeout retries establishing the watch on transient connection errors
	// and re-establishes the watch stream if it is closed or refreshed before timeout.
	return wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		watcher, err := startWatch()
		if err != nil {
			// Transient error starting the watch; return false, nil to retry after the poll interval.
			return false, nil
		}

		// Bound each watch session to 30s so idle TCP connections dropped silently by
		// outbound NAT gateways on external CI runners reconnect and emit fresh state.
		watchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// UntilWithoutRetry consumes events from watcher (and stops it on exit) until
		// the condition returns true, the channel closes, or watchCtx expires.
		_, err = clientwatch.UntilWithoutRetry(watchCtx, watcher, func(event watch.Event) (bool, error) {
			obj, ok := event.Object.(T)
			return ok && isReady(obj), nil
		})
		if err != nil {
			// If the overall timeout context is still active (ctx.Err() == nil),
			// return false, nil to reconnect on the next poll cycle.
			return false, ctx.Err()
		}
		return true, nil
	})
}
