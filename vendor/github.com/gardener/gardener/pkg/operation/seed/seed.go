// Copyright (c) 2018 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package seed

import (
	"fmt"
	"path/filepath"

	"github.com/gardener/gardener/pkg/apis/garden"
	gardenv1beta1 "github.com/gardener/gardener/pkg/apis/garden/v1beta1"
	"github.com/gardener/gardener/pkg/apis/garden/v1beta1/helper"
	"github.com/gardener/gardener/pkg/chartrenderer"
	gardeninformers "github.com/gardener/gardener/pkg/client/garden/informers/externalversions/garden/v1beta1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/features"
	"github.com/gardener/gardener/pkg/operation/common"
	"github.com/gardener/gardener/pkg/utils"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/gardener/gardener/pkg/utils/secrets"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	caSeed = "ca-seed"
)

var wantedCertificateAuthorities = map[string]*secrets.CertificateSecretConfig{
	caSeed: &secrets.CertificateSecretConfig{
		Name:       caSeed,
		CommonName: "kubernetes",
		CertType:   secrets.CACert,
	},
}

// New takes a <k8sGardenClient>, the <k8sGardenInformers> and a <seed> manifest, and creates a new Seed representation.
// It will add the CloudProfile and identify the cloud provider.
func New(k8sGardenClient kubernetes.Client, k8sGardenInformers gardeninformers.Interface, seed *gardenv1beta1.Seed) (*Seed, error) {
	secret, err := k8sGardenClient.GetSecret(seed.Spec.SecretRef.Namespace, seed.Spec.SecretRef.Name)
	if err != nil {
		return nil, err
	}

	cloudProfile, err := k8sGardenInformers.CloudProfiles().Lister().Get(seed.Spec.Cloud.Profile)
	if err != nil {
		return nil, err
	}

	seedObj := &Seed{
		Info:         seed,
		Secret:       secret,
		CloudProfile: cloudProfile,
	}

	cloudProvider, err := helper.DetermineCloudProviderInProfile(cloudProfile.Spec)
	if err != nil {
		return nil, err
	}
	seedObj.CloudProvider = cloudProvider

	return seedObj, nil
}

// NewFromName creates a new Seed object based on the name of a Seed manifest.
func NewFromName(k8sGardenClient kubernetes.Client, k8sGardenInformers gardeninformers.Interface, seedName string) (*Seed, error) {
	seed, err := k8sGardenInformers.Seeds().Lister().Get(seedName)
	if err != nil {
		return nil, err
	}
	return New(k8sGardenClient, k8sGardenInformers, seed)
}

// List returns a list of Seed clusters (along with the referenced secrets).
func List(k8sGardenClient kubernetes.Client, k8sGardenInformers gardeninformers.Interface) ([]*Seed, error) {
	var seedList []*Seed

	list, err := k8sGardenInformers.Seeds().Lister().List(labels.Everything())
	if err != nil {
		return nil, err
	}

	for _, obj := range list {
		seed, err := New(k8sGardenClient, k8sGardenInformers, obj)
		if err != nil {
			return nil, err
		}
		seedList = append(seedList, seed)
	}

	return seedList, nil
}

// generateWantedSecrets returns a list of Secret configuration objects satisfying the secret config intface,
// each containing their specific configuration for the creation of certificates (server/client), RSA key pairs, basic
// authentication credentials, etc.
func generateWantedSecrets(seed *Seed, certificateAuthorities map[string]*secrets.Certificate) ([]secrets.ConfigInterface, error) {
	var (
		kibanaHost = seed.GetIngressFQDN("k", "", "garden")
	)

	if len(certificateAuthorities) != len(wantedCertificateAuthorities) {
		return nil, fmt.Errorf("missing certificate authorities")
	}

	secretList := []secrets.ConfigInterface{
		&secrets.CertificateSecretConfig{
			Name: "kibana-tls",

			CommonName:   "kibana",
			Organization: []string{fmt.Sprintf("%s:logging:ingress", garden.GroupName)},
			DNSNames:     []string{kibanaHost},
			IPAddresses:  nil,

			CertType:  secrets.ServerCert,
			SigningCA: certificateAuthorities[caSeed],
		},
		// Secret definition for monitoring
		&secrets.BasicAuthSecretConfig{
			Name:   "seed-logging-ingress-credentials",
			Format: secrets.BasicAuthFormatNormal,

			Username:       "admin",
			PasswordLength: 32,
		},
	}

	return secretList, nil
}

// deployCertificates deploys CA and TLS certificates inside the garden namespace
// It takes a map[string]*corev1.Secret object which contains secrets that have already been deployed inside that namespace to avoid duplication errors.
func deployCertificates(seed *Seed, k8sSeedClient kubernetes.Client, existingSecretsMap map[string]*corev1.Secret) (map[string]*corev1.Secret, error) {

	_, certificateAuthorities, err := secrets.GenerateCertificateAuthorities(k8sSeedClient, existingSecretsMap, wantedCertificateAuthorities, common.GardenNamespace)
	if err != nil {
		return nil, err
	}

	wantedSecretsList, err := generateWantedSecrets(seed, certificateAuthorities)
	if err != nil {
		return nil, err
	}

	return secrets.GenerateClusterSecrets(k8sSeedClient, existingSecretsMap, wantedSecretsList, common.GardenNamespace)
}

// BootstrapCluster bootstraps a Seed cluster and deploys various required manifests.
func BootstrapCluster(seed *Seed, k8sGardenClient kubernetes.Client, secrets map[string]*corev1.Secret, imageVector imagevector.ImageVector, numberOfAssociatedShoots int) error {
	const chartName = "seed-bootstrap"
	var existingSecretsMap = map[string]*corev1.Secret{}

	k8sSeedClient, err := kubernetes.NewClientFromSecretObject(seed.Secret)
	if err != nil {
		return err
	}

	body := fmt.Sprintf(`[{"op": "add", "path": "/metadata/labels", "value": %s}]`, "{\"role\":\"kube-system\"}")
	if _, err := k8sSeedClient.PatchNamespace("kube-system", []byte(body)); err != nil {
		return err
	}

	gardenNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: common.GardenNamespace,
		},
	}
	if _, err := k8sSeedClient.CreateNamespace(gardenNamespace, false); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}

	imageNames := []string{
		common.PrometheusImageName,
		common.ConfigMapReloaderImageName,
		common.PauseContainerImageName,
		common.GardenerExternalAdmissionControllerImageName,
	}
	images := make(map[string]string, len(imageNames))
	for _, imageName := range imageNames {
		image, err := imageVector.FindImage(imageName, k8sSeedClient.Version(), k8sSeedClient.Version())
		if err != nil {
			return err
		}
		images[imageName] = image.String()
	}

	elasticsearchVersion, err := imageVector.FindImage(common.ElasticsearchImageName, k8sSeedClient.Version(), k8sSeedClient.Version())
	if err != nil {
		return err
	}
	fluentdEsVersion, err := imageVector.FindImage(common.FluentdEsImageName, k8sSeedClient.Version(), k8sSeedClient.Version())
	if err != nil {
		return err
	}
	fluentBitVersion, err := imageVector.FindImage(common.FluentBitImageName, k8sSeedClient.Version(), k8sSeedClient.Version())
	if err != nil {
		return err
	}
	curatorEsVersion, err := imageVector.FindImage(common.CuratorImageName, k8sSeedClient.Version(), k8sSeedClient.Version())
	if err != nil {
		return err
	}
	kibanaVersion, err := imageVector.FindImage(common.KibanaImageName, k8sSeedClient.Version(), k8sSeedClient.Version())
	if err != nil {
		return err
	}
	alpineVersion, err := imageVector.FindImage(common.AlpineImageName, k8sSeedClient.Version(), k8sSeedClient.Version())
	if err != nil {
		return err
	}

	var (
		basicAuth  string
		kibanaHost string
		replicas   int
	)

	loggingEnabled := features.ControllerFeatureGate.Enabled(features.Logging)

	if loggingEnabled {
		existingSecrets, err := k8sSeedClient.ListSecrets(common.GardenNamespace, metav1.ListOptions{})
		if err != nil {
			return err
		}

		for _, secret := range existingSecrets.Items {
			secretObj := secret
			existingSecretsMap[secret.ObjectMeta.Name] = &secretObj
		}

		// currently the generated certificates are only used by the kibana so they are all disabled/enabled when the logging feature is disabled/enabled
		deployedSecretsMap, err := deployCertificates(seed, k8sSeedClient, existingSecretsMap)
		if err != nil {
			return err
		}

		credentials := deployedSecretsMap["seed-logging-ingress-credentials"]
		basicAuth = utils.CreateSHA1Secret(credentials.Data["username"], credentials.Data["password"])

		kibanaHost = seed.GetIngressFQDN("k", "", "garden")
		replicas = 1
	}

	nodes, err := k8sSeedClient.ListNodes(metav1.ListOptions{})
	if err != nil {
		return err
	}

	nodeCount := len(nodes.Items)

	chartRenderer, err := chartrenderer.New(k8sSeedClient)
	if err != nil {
		return err
	}

	return common.ApplyChart(k8sSeedClient, chartRenderer, filepath.Join("charts", chartName), chartName, common.GardenNamespace, nil, map[string]interface{}{
		"cloudProvider":         seed.CloudProvider,
		"images":                images,
		"reserveExcessCapacity": seed.reserveExcessCapacity,
		"replicas": map[string]interface{}{
			"reserve-excess-capacity": DesiredExcessCapacity(numberOfAssociatedShoots),
		},
		"prometheus": map[string]interface{}{
			"objectCount": nodeCount,
		},
		"elastic-kibana-curator": map[string]interface{}{
			"enabled": loggingEnabled,
			"ingress": map[string]interface{}{
				"basicAuthSecret": basicAuth,
				"host":            kibanaHost,
			},
			"kibanaReplicas":        replicas,
			"elasticsearchReplicas": replicas,
			"images": map[string]interface{}{
				"elasticsearch-oss": elasticsearchVersion.String(),
				"curator-es":        curatorEsVersion.String(),
				"kibana-oss":        kibanaVersion.String(),
				"alpine":            alpineVersion.String(),
			},
		},
		"fluentd-es": map[string]interface{}{
			"enabled": loggingEnabled,
			"images": map[string]interface{}{
				"fluentd-es": fluentdEsVersion.String(),
				"fluent-bit": fluentBitVersion.String(),
			},
		},
	})
}

// DesiredExcessCapacity computes the required resources (CPU and memory) required to deploy new shoot control planes
// (on the seed) in terms of reserve-excess-capacity deployment replicas. Each deployment replica currently
// corresponds to resources of (request/limits) 500m of CPU and 1200Mi of Memory.
// ReplicasRequiredToSupportSingleShoot is 4 which is 2000m of CPU and 4800Mi of RAM.
// The logic for computation of desired excess capacity corresponds to either deploying 3 new shoot control planes
// or 5% of existing shoot control planes of current number of shoots deployed in seed (5 if current shoots are 100),
// whichever of the two is larger
func DesiredExcessCapacity(numberOfAssociatedShoots int) int {
	var (
		replicasToSupportSingleShoot          = 4
		effectiveExcessCapacity               = 3
		excessCapacityBasedOnAssociatedShoots = int(float64(numberOfAssociatedShoots) * 0.05)
	)

	if excessCapacityBasedOnAssociatedShoots > effectiveExcessCapacity {
		effectiveExcessCapacity = excessCapacityBasedOnAssociatedShoots
	}

	return effectiveExcessCapacity * replicasToSupportSingleShoot
}

// GetIngressFQDN returns the fully qualified domain name of ingress sub-resource for the Seed cluster. The
// end result is '<subDomain>.<shootName>.<projectName>.<seed-ingress-domain>'.
func (s *Seed) GetIngressFQDN(subDomain, shootName, projectName string) string {
	if shootName == "" {
		return fmt.Sprintf("%s.%s.%s", subDomain, projectName, s.Info.Spec.IngressDomain)
	}
	return fmt.Sprintf("%s.%s.%s.%s", subDomain, shootName, projectName, s.Info.Spec.IngressDomain)
}

// CheckMinimumK8SVersion checks whether the Kubernetes version of the Seed cluster fulfills the minimal requirements.
func (s *Seed) CheckMinimumK8SVersion() error {
	var minSeedVersion string
	switch s.CloudProvider {
	case gardenv1beta1.CloudProviderAzure:
		minSeedVersion = "1.8.6" // https://github.com/kubernetes/kubernetes/issues/56898
	default:
		minSeedVersion = "1.8" // CRD garbage collection
	}

	k8sSeedClient, err := kubernetes.NewClientFromSecretObject(s.Secret)
	if err != nil {
		return err
	}

	seedVersionOK, err := utils.CompareVersions(k8sSeedClient.Version(), ">=", minSeedVersion)
	if err != nil {
		return err
	}
	if !seedVersionOK {
		return fmt.Errorf("the Kubernetes version of the Seed cluster must be at least %s", minSeedVersion)
	}
	return nil
}

// MustReserveExcessCapacity configures whether we have to reserve excess capacity in the Seed cluster.
func (s *Seed) MustReserveExcessCapacity(must bool) {
	s.reserveExcessCapacity = must
}
