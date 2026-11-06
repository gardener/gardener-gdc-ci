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

package gardener

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type gardenletImageVectorCABundle struct {
	Inline string `yaml:"inline"`
}

type gardenletImageVectorOverwrite struct {
	Images   []struct{}                   `yaml:"images"`
	CABundle gardenletImageVectorCABundle `yaml:"caBundle"`
}

// BuildGardenletImageVectorOverwrite constructs a Gardener gardenlet imageVectorOverwrite YAML
// string containing caBundle.inline populated with the valid, non-expired PEM-encoded CA
// certificates extracted from GDC.CAData (base64-encoded PEM or raw PEM).
//
// Per Gardener's image vector documentation (gardener/gardener#15150), setting caBundle.inline
// in gardenlet's imageVectorOverwrite instructs gardenlet to distribute the CA bundle to the OS
// trust store of every Shoot worker node managed by the Seed. Non-CA leaf certificates (such as
// the Console server certificate returned first by /.well-known/certificate-authority) are
// filtered out so caBundle.inline contains only valid CA certificates.
//
// If gdcCAData is empty (after trimming whitespace), an empty string is returned with nil error.
func BuildGardenletImageVectorOverwrite(gdcCAData string) (string, error) {
	return buildGardenletImageVectorOverwriteAt(gdcCAData, time.Now())
}

func buildGardenletImageVectorOverwriteAt(gdcCAData string, now time.Time) (string, error) {
	trimmed := strings.TrimSpace(gdcCAData)
	if trimmed == "" {
		return "", nil
	}

	var pemBytes []byte
	if strings.HasPrefix(trimmed, "-----BEGIN ") {
		pemBytes = []byte(trimmed)
	} else {
		decoded, err := base64.StdEncoding.DecodeString(trimmed)
		if err != nil {
			return "", fmt.Errorf("failed to base64-decode GDC CA data: %w", err)
		}
		pemBytes = decoded
	}

	var caPEMBuf bytes.Buffer
	rest := pemBytes
	caCount := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("failed to parse X.509 certificate in GDC CA data: %w", err)
		}
		if !cert.IsCA || !now.Before(cert.NotAfter) {
			continue
		}
		if err := pem.Encode(&caPEMBuf, &pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}); err != nil {
			return "", fmt.Errorf("failed to encode CA certificate to PEM: %w", err)
		}
		caCount++
	}

	if caCount == 0 {
		return "", fmt.Errorf("no valid non-expired CA certificates found in GDC CA data")
	}

	overwrite := gardenletImageVectorOverwrite{
		Images: []struct{}{},
		CABundle: gardenletImageVectorCABundle{
			Inline: caPEMBuf.String(),
		},
	}
	yamlBytes, err := yaml.Marshal(overwrite)
	if err != nil {
		return "", fmt.Errorf("failed to marshal gardenlet imageVectorOverwrite: %w", err)
	}
	return string(yamlBytes), nil
}
