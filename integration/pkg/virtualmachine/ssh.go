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

// This file contains SSH-related methods for the VMManager struct.
// The VMManager struct definition and its core lifecycle methods are located in virtualmachine.go.
package virtualmachine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
)

const (
	defaultUser                 = "useraccess"
	defaultPort                 = 22
	defaultSSHConnectionTimeout = 5 * time.Second
	defaultSSHPollInterval      = 5 * time.Second
)

// SSHCommandOptions contains options for running an SSH command.
type SSHCommandOptions struct {
	Command      string
	Timeout      time.Duration
	PollInterval time.Duration
}

// RunSSHCommand runs a command on the VM instance via SSH, polling until success or timeout.
// It returns the output and error from the command execution.
func (m *VMManager) RunSSHCommand(ctx context.Context, opts SSHCommandOptions) (string, error) {
	config, err := getSSHConfig(m.sshKeyPath)
	if err != nil {
		return "", fmt.Errorf("failed to get SSH config: %w", err)
	}

	if opts.Timeout == 0 {
		return "", fmt.Errorf("timeout must be set in SSHCommandOptions")
	}

	if opts.PollInterval == 0 {
		opts.PollInterval = defaultSSHPollInterval
	}

	var lastOutput string
	var lastErr error

	pollErr := wait.PollUntilContextTimeout(ctx, opts.PollInterval, opts.Timeout, true, func(ctx context.Context) (bool, error) {
		lastOutput, lastErr = m.connectAndExec(ctx, config, opts.Command)
		if lastErr != nil {
			return false, nil // Keep retrying
		}
		return true, nil // Success
	})

	if pollErr != nil {
		return lastOutput, fmt.Errorf("SSH command failed or timed out: %w, last error: %v", pollErr, lastErr)
	}

	return lastOutput, nil
}

// connectAndExec runs a command on the VM instance.
func (m *VMManager) connectAndExec(ctx context.Context, config *ssh.ClientConfig, command string) (string, error) {
	client, err := dial(m.ingressIP, config)
	if err != nil {
		return "", err
	}
	defer client.Close()
	return execOnSSHClient(client, command)
}

// waitForSSH waits for SSH to be available on the given VM IP.
func waitForSSH(ctx context.Context, vmIP, sshKeyPath string, timeout time.Duration) error {
	config, err := getSSHConfig(sshKeyPath)
	if err != nil {
		return err
	}

	// Track the last connection or execution error during the polling lifecycle so we can
	// surface it upon timeout (since Kubernetes' PollUntilContextTimeout only returns a generic
	// deadline exceeded error which obscures the root cause of SSH connection failures).
	var lastErr error
	pollErr := wait.PollUntilContextTimeout(ctx, defaultSSHPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		client, dialErr := dial(vmIP, config)
		if dialErr != nil {
			lastErr = dialErr
			return false, nil // retry
		}
		defer client.Close()

		// Try to run a simple command to ensure it's fully ready
		_, execErr := execOnSSHClient(client, "echo hello")
		if execErr != nil {
			lastErr = execErr
			return false, nil // retry
		}
		return true, nil
	})
	if pollErr != nil {
		return fmt.Errorf("failed to wait for SSH on %s (timeout): %w, last connection error: %v", vmIP, pollErr, lastErr)
	}
	return nil
}

// getSSHConfig creates an ssh.ClientConfig using the private key at sshKeyPath and defaultUser.
func getSSHConfig(sshKeyPath string) (*ssh.ClientConfig, error) {
	key, err := os.ReadFile(sshKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	return &ssh.ClientConfig{
		User: defaultUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         defaultSSHConnectionTimeout,
	}, nil
}

// dial connects to the given VM IP using the provided config.
func dial(vmIP string, config *ssh.ClientConfig) (*ssh.Client, error) {
	return ssh.Dial("tcp", fmt.Sprintf("%s:%d", vmIP, defaultPort), config)
}

// execOnSSHClient runs a command on the given SSH client and returns the output.
func execOnSSHClient(client *ssh.Client, command string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	var b bytes.Buffer
	session.Stdout = &b
	session.Stderr = &b

	err = session.Run(command)
	return b.String(), err
}

// generateSSHKey generates an SSH key pair for testing.
func generateSSHKey(name string) (string, error) {
	tmpDir, err := os.MkdirTemp("", fmt.Sprintf("vm_test_%s_*", name))
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for SSH key: %w", err)
	}
	keyPath := filepath.Join(tmpDir, "id_rsa")

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to generate RSA key: %w", err)
	}

	privateKeyPEM := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	privateKeyBytes := pem.EncodeToMemory(privateKeyPEM)

	if err := os.WriteFile(keyPath, privateKeyBytes, 0600); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to write private key: %w", err)
	}

	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to generate public key: %w", err)
	}
	publicKeyBytes := ssh.MarshalAuthorizedKey(publicKey)

	if err := os.WriteFile(keyPath+".pub", publicKeyBytes, 0644); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to write public key: %w", err)
	}

	return keyPath, nil
}

// readPublicKey reads the public key from the given path.
func readPublicKey(keyPath string) (string, error) {
	pubKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", fmt.Errorf("failed to read public key: %w", err)
	}
	return string(pubKeyBytes), nil
}

// createVirtualMachineAccessRequest creates a VirtualMachineAccessRequest.
func (m *VMManager) createVirtualMachineAccessRequest(ctx context.Context, vmName, user, pubKey, ttl string) (*vmv1.VirtualMachineAccessRequest, error) {
	duration, err := time.ParseDuration(ttl)
	if err != nil {
		return nil, fmt.Errorf("failed to parse TTL: %w", err)
	}

	vmar := &vmv1.VirtualMachineAccessRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: m.name + "-",
			Namespace:    m.namespace,
		},
		Spec: vmv1.VirtualMachineAccessRequestSpec{
			VM:   vmName,
			User: user,
			SSH: vmv1.SSHSpec{
				Key: pubKey,
				TTL: metav1.Duration{Duration: duration},
			},
		},
	}
	if err := m.client.Create(ctx, vmar); err != nil {
		return nil, fmt.Errorf("failed to create VirtualMachineAccessRequest: %w", err)
	}
	return vmar, nil
}
