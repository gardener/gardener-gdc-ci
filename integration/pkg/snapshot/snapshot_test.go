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

package snapshot

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/gcs"
)

func TestCaptureSnapshot_Namespace(t *testing.T) {
	const testBucketName = "test-override-bucket"
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "default",
			},
		},
	}

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)

	crClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(pod).Build()

	config := ClusterSnapshotConfig{
		ClusterName: "shoot",
		Targets: []NamespaceTarget{
			{
				Namespace: "default",
				Resources: []ResourceTarget{
					{APIVersion: "v1", Kind: "Pod"},
				},
			},
		},
	}

	tempDir := t.TempDir()
	if err := CaptureSnapshotToDir(context.Background(), SnapshotClients{CRClient: crClient}, tempDir, CaptureOptions{Config: &config}); err != nil {
		t.Fatalf("CaptureSnapshotToDir failed: %v", err)
	}

	expectedPath := filepath.Join(tempDir, "default", "pods", "test-pod.yaml")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("Expected file %s not found: %v", expectedPath, err)
	}
}

func TestCaptureSnapshot_MatchLabels(t *testing.T) {
	const testBucketName = "test-override-bucket"
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": "test-ns",
				"labels": map[string]interface{}{
					"app": "test-app",
				},
			},
		},
	}

	pod := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]interface{}{
				"name":      "test-pod",
				"namespace": "test-ns",
			},
		},
	}

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)

	crClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(ns, pod).Build()

	config := ClusterSnapshotConfig{
		ClusterName: "shoot",
		Targets: []NamespaceTarget{
			{
				MatchLabels: map[string]string{"app": "test-app"},
				Resources: []ResourceTarget{
					{APIVersion: "v1", Kind: "Pod"},
				},
			},
		},
	}

	tempDir := t.TempDir()
	if err := CaptureSnapshotToDir(context.Background(), SnapshotClients{CRClient: crClient}, tempDir, CaptureOptions{Config: &config}); err != nil {
		t.Fatalf("CaptureSnapshotToDir failed: %v", err)
	}

	expectedPath := filepath.Join(tempDir, "test-ns", "pods", "test-pod.yaml")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("Expected file %s not found: %v", expectedPath, err)
	}
}

func TestGetNamespaces_MutualExclusive(t *testing.T) {
	target := NamespaceTarget{
		Namespace:   "default",
		MatchLabels: map[string]string{"app": "test-app"},
	}

	_, err := getNamespaces(context.Background(), nil, target)
	if err == nil {
		t.Error("Expected error when both namespace and labelSelector are specified, got nil")
	}
}

func TestLoadSnapshotResources(t *testing.T) {
	res, err := LoadSnapshotResources("testdata/snapshot_resources.yaml")
	if err != nil {
		t.Fatalf("LoadSnapshotResources failed: %v", err)
	}

	if len(res.Clusters) != 1 {
		t.Fatalf("Expected 1 cluster, got %d", len(res.Clusters))
	}
	if res.Clusters[0].ClusterName != "shoot" {
		t.Errorf("Expected cluster name 'shoot', got %s", res.Clusters[0].ClusterName)
	}
	if len(res.Clusters[0].Targets) != 2 {
		t.Fatalf("Expected 2 targets, got %d", len(res.Clusters[0].Targets))
	}
	if res.Clusters[0].Targets[0].Namespace != "default" {
		t.Errorf("Expected namespace 'default', got %s", res.Clusters[0].Targets[0].Namespace)
	}
	if res.Clusters[0].Targets[1].MatchLabels["app"] != "test-app" {
		t.Errorf("Expected matchLabels['app'] 'test-app', got %s", res.Clusters[0].Targets[1].MatchLabels["app"])
	}

	if len(res.Clusters[0].Targets[0].Resources) != 2 {
		t.Fatalf("Expected 2 resources, got %d", len(res.Clusters[0].Targets[0].Resources))
	}

	if res.Clusters[0].Targets[0].Resources[0].Kind != "Pod" || res.Clusters[0].Targets[0].Resources[0].APIVersion != "v1" {
		t.Errorf("Unexpected resource[0]: Kind=%s, APIVersion=%s", res.Clusters[0].Targets[0].Resources[0].Kind, res.Clusters[0].Targets[0].Resources[0].APIVersion)
	}
	if res.Clusters[0].Targets[0].Resources[1].Kind != "CSIDriver" || res.Clusters[0].Targets[0].Resources[1].APIVersion != "storage.k8s.io/v1" {
		t.Errorf("Unexpected resource[1]: Kind=%s, APIVersion=%s", res.Clusters[0].Targets[0].Resources[1].Kind, res.Clusters[0].Targets[0].Resources[1].APIVersion)
	}
}

func TestCreateArchive(t *testing.T) {
	srcDir := t.TempDir()

	subDir := filepath.Join(srcDir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("Failed to create subdirectory: %v", err)
	}

	file1Path := filepath.Join(srcDir, "file1.txt")
	file2Path := filepath.Join(subDir, "file2.txt")

	if err := os.WriteFile(file1Path, []byte("content1"), 0600); err != nil {
		t.Fatalf("Failed to write file1: %v", err)
	}
	if err := os.WriteFile(file2Path, []byte("content2"), 0600); err != nil {
		t.Fatalf("Failed to write file2: %v", err)
	}

	destTar := filepath.Join(t.TempDir(), "archive.tar.gz")

	if err := createArchive(srcDir, destTar); err != nil {
		t.Fatalf("createArchive failed: %v", err)
	}

	if _, err := os.Stat(destTar); err != nil {
		t.Fatalf("Archive file not found: %v", err)
	}

	f, err := os.Open(destTar)
	if err != nil {
		t.Fatalf("Failed to open archive: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	filesFound := make(map[string]string)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Error reading tar header: %v", err)
		}

		if header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("Failed to read file content from tar: %v", err)
			}
			filesFound[header.Name] = string(data)
		}
	}

	if len(filesFound) != 2 {
		t.Errorf("Expected 2 files in archive, found %d: %v", len(filesFound), filesFound)
	}

	if filesFound["file1.txt"] != "content1" {
		t.Errorf("Expected file1.txt to have content 'content1', got %q", filesFound["file1.txt"])
	}

	if filesFound["sub/file2.txt"] != "content2" {
		t.Errorf("Expected sub/file2.txt to have content 'content2', got %q", filesFound["sub/file2.txt"])
	}
}

type errorClient struct {
	client.Client
	listErr error
}

func (c *errorClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.listErr != nil {
		return c.listErr
	}
	return c.Client.List(ctx, list, opts...)
}

func TestCaptureSnapshot_PartialSnapshotError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)

	crClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).Build()

	mockErr := fmt.Errorf("simulated list permission error")
	wrappedClient := &errorClient{
		Client:  crClient,
		listErr: mockErr,
	}

	config := ClusterSnapshotConfig{
		ClusterName: "shoot",
		Targets: []NamespaceTarget{
			{
				Namespace: "default",
				Resources: []ResourceTarget{
					{APIVersion: "v1", Kind: "Pod"},
				},
			},
		},
	}

	tempDir := t.TempDir()
	err := CaptureSnapshotToDir(context.Background(), SnapshotClients{CRClient: wrappedClient}, tempDir, CaptureOptions{Config: &config})
	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	var partialErr *PartialSnapshotError
	if !errors.As(err, &partialErr) {
		t.Fatalf("Expected PartialSnapshotError, got: %T (%v)", err, err)
	}

	if len(partialErr.Errors) != 1 {
		t.Errorf("Expected 1 accumulated error, got %d", len(partialErr.Errors))
	}

	expectedErrStr := "failed to list resource pods in default: simulated list permission error"
	if !strings.Contains(partialErr.Error(), expectedErrStr) {
		t.Errorf("Expected error message to contain %q, got %q", expectedErrStr, partialErr.Error())
	}
}

func TestCaptureSnapshot_NeverCapturesSecrets(t *testing.T) {
	// Readable placeholder stored in the Secret; it must never show up in a snapshot file.
	const unwantedContent = "do-not-capture"

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	namespace := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": "garden"},
		},
	}
	secret := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]interface{}{"name": "virtual-garden-kubeconfig", "namespace": "garden"},
			"stringData": map[string]interface{}{"kubeconfig": unwantedContent},
		},
	}
	configMap := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": "settings", "namespace": "garden"},
		},
	}

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)

	tests := []struct {
		name   string
		config *ClusterSnapshotConfig
	}{
		{
			name:   "auto-discovery",
			config: nil,
		},
		{
			name: "explicit Secret target",
			config: &ClusterSnapshotConfig{
				ClusterName: "runtime-cluster",
				Targets: []NamespaceTarget{
					{
						Namespace: "garden",
						Resources: []ResourceTarget{
							{APIVersion: "v1", Kind: "Secret"},
							{APIVersion: "v1", Kind: "ConfigMap"},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			crClient := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
				WithObjects(namespace.DeepCopy(), secret.DeepCopy(), configMap.DeepCopy()).Build()

			dir := filepath.Join(t.TempDir(), "runtime-cluster")
			if err := CaptureSnapshotToDir(context.Background(), SnapshotClients{CRClient: crClient}, dir, CaptureOptions{Config: tc.config}); err != nil {
				t.Fatalf("CaptureSnapshotToDir failed: %v", err)
			}

			if _, err := os.Stat(filepath.Join(dir, "garden", "configmaps", "settings.yaml")); err != nil {
				t.Errorf("Expected the ConfigMap to be captured: %v", err)
			}

			err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					if entry.Name() == "secrets" {
						t.Errorf("Snapshot must not contain a secrets directory, found %s", path)
					}
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if strings.Contains(string(data), unwantedContent) {
					t.Errorf("Snapshot file %s contains Secret data", path)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("Failed to inspect snapshot directory: %v", err)
			}
		})
	}
}

// newSnapshotSourceDir returns a directory with a single captured file to archive.
func newSnapshotSourceDir(t *testing.T) string {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "pod.yaml"), []byte("kind: Pod"), 0600); err != nil {
		t.Fatalf("Failed to write snapshot source file: %v", err)
	}
	return srcDir
}

// countArchiveCreations wraps createArchive and reports how often an archive is built during the test.
func countArchiveCreations(t *testing.T) *int {
	t.Helper()
	orig := createArchive
	calls := 0
	createArchive = func(srcDir, archivePath string) error {
		calls++
		return orig(srcDir, archivePath)
	}
	t.Cleanup(func() { createArchive = orig })
	return &calls
}

// stubUpload replaces the GCS upload with fn for the duration of the test.
func stubUpload(t *testing.T, fn func(ctx context.Context, options gcs.UploadOptions) error) {
	t.Helper()
	orig := uploadFile
	uploadFile = fn
	t.Cleanup(func() { uploadFile = orig })
}

// listArchives returns the snapshot archives found in dir.
func listArchives(t *testing.T, dir string) []string {
	t.Helper()
	archives, err := filepath.Glob(filepath.Join(dir, "snapshots-all-clusters-*.tar.gz"))
	if err != nil {
		t.Fatalf("Failed to list archives in %s: %v", dir, err)
	}
	return archives
}

func TestPersistArchive_HandsSameArchiveToAllDestinations(t *testing.T) {
	srcDir := newSnapshotSourceDir(t)
	localDir := filepath.Join(t.TempDir(), "run-1", "3-deploy-gardener")
	archiveCreations := countArchiveCreations(t)

	var uploaded []gcs.UploadOptions
	stubUpload(t, func(_ context.Context, options gcs.UploadOptions) error {
		uploaded = append(uploaded, options)
		return nil
	})

	err := PersistArchive(context.Background(), srcDir, "all-clusters", ArchiveDestinations{
		LocalDir: localDir,
		Upload:   &DestinationConfig{Bucket: "private-bucket", DestDir: "release-pipeline-snapshots/run-1"},
	})
	if err != nil {
		t.Fatalf("PersistArchive failed: %v", err)
	}

	if *archiveCreations != 1 {
		t.Errorf("Expected the archive to be built once, got %d builds", *archiveCreations)
	}
	archives := listArchives(t, localDir)
	if len(archives) != 1 {
		t.Fatalf("Expected exactly one local archive, got %v", archives)
	}
	if len(uploaded) != 1 {
		t.Fatalf("Expected exactly one upload, got %d", len(uploaded))
	}
	if uploaded[0].LocalFilePath != archives[0] {
		t.Errorf("Expected the local archive %s to be uploaded, got %s", archives[0], uploaded[0].LocalFilePath)
	}
	if uploaded[0].BucketName != "private-bucket" {
		t.Errorf("Unexpected bucket %q", uploaded[0].BucketName)
	}
	wantObject := "release-pipeline-snapshots/run-1/" + filepath.Base(archives[0])
	if uploaded[0].ObjectName != wantObject {
		t.Errorf("Expected object name %q, got %q", wantObject, uploaded[0].ObjectName)
	}
}

func TestPersistArchive_LocalOnly(t *testing.T) {
	srcDir := newSnapshotSourceDir(t)
	localDir := t.TempDir()
	stubUpload(t, func(context.Context, gcs.UploadOptions) error {
		t.Error("Upload must not be attempted without a bucket")
		return nil
	})

	for _, upload := range []*DestinationConfig{nil, {DestDir: "ignored-without-bucket"}} {
		if err := PersistArchive(context.Background(), srcDir, "all-clusters", ArchiveDestinations{LocalDir: localDir, Upload: upload}); err != nil {
			t.Fatalf("PersistArchive failed: %v", err)
		}
	}
	if archives := listArchives(t, localDir); len(archives) == 0 {
		t.Errorf("Expected a local archive in %s", localDir)
	}
}

func TestPersistArchive_UploadOnlyStagesArchive(t *testing.T) {
	srcDir := newSnapshotSourceDir(t)
	archiveCreations := countArchiveCreations(t)

	var stagedPath string
	stubUpload(t, func(_ context.Context, options gcs.UploadOptions) error {
		stagedPath = options.LocalFilePath
		if _, err := os.Stat(stagedPath); err != nil {
			t.Errorf("Expected the staged archive to exist during the upload: %v", err)
		}
		return nil
	})

	err := PersistArchive(context.Background(), srcDir, "all-clusters", ArchiveDestinations{
		Upload: &DestinationConfig{Bucket: "private-bucket", DestDir: "dest"},
	})
	if err != nil {
		t.Fatalf("PersistArchive failed: %v", err)
	}
	if *archiveCreations != 1 {
		t.Errorf("Expected the archive to be built once, got %d builds", *archiveCreations)
	}
	if stagedPath == "" {
		t.Fatal("Expected the archive to be uploaded")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Errorf("Expected the staged archive %s to be removed after the upload, got err=%v", stagedPath, err)
	}
}

func TestPersistArchive_KeepsLocalArchiveWhenUploadFails(t *testing.T) {
	srcDir := newSnapshotSourceDir(t)
	localDir := t.TempDir()
	stubUpload(t, func(context.Context, gcs.UploadOptions) error {
		return fmt.Errorf("simulated upload failure")
	})

	err := PersistArchive(context.Background(), srcDir, "all-clusters", ArchiveDestinations{
		LocalDir: localDir,
		Upload:   &DestinationConfig{Bucket: "private-bucket", DestDir: "dest"},
	})
	if err != nil {
		t.Fatalf("Expected the local archive to be sufficient, got error: %v", err)
	}
	if archives := listArchives(t, localDir); len(archives) != 1 {
		t.Errorf("Expected exactly one local archive, got %v", archives)
	}
}

func TestPersistArchive_FailsWhenNothingPersisted(t *testing.T) {
	srcDir := newSnapshotSourceDir(t)

	// A regular file where the local directory should be makes local persistence fail.
	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, nil, 0600); err != nil {
		t.Fatalf("Failed to create blocking file: %v", err)
	}
	unusableLocalDir := filepath.Join(blockingFile, "snapshots")

	stubUpload(t, func(context.Context, gcs.UploadOptions) error {
		return fmt.Errorf("simulated upload failure")
	})

	tests := []struct {
		name    string
		dest    ArchiveDestinations
		wantErr string
	}{
		{
			name:    "no destination configured",
			dest:    ArchiveDestinations{},
			wantErr: "no snapshot destination configured",
		},
		{
			name:    "bucket missing and no local directory",
			dest:    ArchiveDestinations{Upload: &DestinationConfig{DestDir: "dest"}},
			wantErr: "no snapshot destination configured",
		},
		{
			name:    "local directory unusable",
			dest:    ArchiveDestinations{LocalDir: unusableLocalDir},
			wantErr: "not persisted to any destination",
		},
		{
			name: "local directory unusable and upload fails",
			dest: ArchiveDestinations{
				LocalDir: unusableLocalDir,
				Upload:   &DestinationConfig{Bucket: "private-bucket", DestDir: "dest"},
			},
			wantErr: "simulated upload failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := PersistArchive(context.Background(), srcDir, "all-clusters", tc.dest)
			if err == nil {
				t.Fatal("Expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Expected error to contain %q, got %q", tc.wantErr, err)
			}
		})
	}
}
