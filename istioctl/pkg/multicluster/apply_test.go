// Copyright 2019 Istio Authors.
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

package multicluster

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd/api"

	"istio.io/istio/pkg/kube/secretcontroller"
)

func cloneCluster(in *Cluster) *Cluster {
	return &Cluster{
		ClusterDesc: in.ClusterDesc,
		Context:     in.Context,
		uid:         in.uid,
		installed:   in.installed,
	}
}

var (
	// required to build remote secret
	pilotServiceAccount = &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultServiceAccountName,
			Namespace: defaultIstioNamespace,
		},
		Secrets: []v1.ObjectReference{{
			Name: "fake-service-account-secret-name",
		}},
	}

	kubeconfigTemplateData = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: {{ .CAData }}
    server: {{ .Server }}
  name: {{ .Context }}
contexts:
- context:
    cluster: {{ .Context }}
    user: {{ .Context }}
  name: {{ .Context }}
current-context: {{ .Context }}
kind: Config
preferences: {}
users:
- name: {{ .Context }}
  user:
    token: {{ .Token }}
`

	kubeconfigTemplate = template.Must(template.New("").Parse(kubeconfigTemplateData))
)

func makeUniqueKubeNamespace(c *Cluster) *v1.Namespace {
	return &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system",
			UID:  c.uid,
		},
	}
}

func makeCluster(id int) *Cluster {
	return &Cluster{
		ClusterDesc: ClusterDesc{
			Network:              fmt.Sprintf("net%v", id),
			Namespace:            defaultIstioNamespace,
			ServiceAccountReader: DefaultServiceAccountName,
			DisableRegistryJoin:  false,
		},
		Context:   fmt.Sprintf("context%v", id),
		uid:       types.UID(fmt.Sprintf("uid%v", id)),
		installed: true,
	}
}

func makeTokenSecret(token, caCertData []byte) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fake-service-account-secret-name",
			Namespace: defaultIstioNamespace,
		},
		Data: map[string][]byte{
			v1.ServiceAccountRootCAKey: caCertData,
			v1.ServiceAccountTokenKey:  token,
		},
	}
}

func makeServerName(c *Cluster) string {
	return fmt.Sprintf("server-%v", c.Context)
}

func makeKubeconfig(c *Cluster, token, caCert []byte) (string, []byte) {
	var out bytes.Buffer
	_ = kubeconfigTemplate.Execute(&out, map[string]string{
		"CAData":  base64.StdEncoding.EncodeToString(caCert),
		"Server":  makeServerName(c),
		"Context": c.Context,
		"Token":   string(token),
	})
	kubeconfig := out.Bytes()
	kubeconfigBase64 := base64.StdEncoding.EncodeToString(kubeconfig)
	return kubeconfigBase64, kubeconfig
}

func makeRemoteSecret(c *Cluster, kubeconfig []byte) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      remoteSecretNameFromUID(c.uid),
			Namespace: defaultIstioNamespace,
			Annotations: map[string]string{
				clusterContextAnnotationKey: c.Context,
			},
			Labels: map[string]string{
				secretcontroller.MultiClusterSecretLabel: "true",
			},
		},
		Data: map[string][]byte{
			string(c.uid): kubeconfig,
		},
	}
}

func makeCAData(c *Cluster) []byte {
	return []byte(fmt.Sprintf("%v-caCert", c.Context))
}
func makeToken(c *Cluster) []byte {
	return []byte(fmt.Sprintf("%v-token", c.Context))
}

const numFakeClusters = 3

// these variables must be initialized at runtime in init()
var (
	clusters             [numFakeClusters]*Cluster
	kubeSystemNamespaces [numFakeClusters]*v1.Namespace
	remoteSecretClusters [numFakeClusters]*v1.Secret
	base64Kubeconfigs    [numFakeClusters]string
	kubeconfigs          [numFakeClusters][]byte
	tokens               [numFakeClusters][]byte
	caDatas              [numFakeClusters][]byte
	pilotTokenSecrets    [numFakeClusters]*v1.Secret

	cluster0IstioNotInstalled   *Cluster
	cluster1DisableRegistryJoin *Cluster

	apiConfig *api.Config
)

func init() {
	apiConfig = &api.Config{
		Contexts: map[string]*api.Context{},
		Clusters: map[string]*api.Cluster{},
	}

	for i := 0; i < numFakeClusters; i++ {
		clusters[i] = makeCluster(i)
		kubeSystemNamespaces[i] = makeUniqueKubeNamespace(clusters[i])
		tokens[i] = makeToken(clusters[i])
		caDatas[i] = makeCAData(clusters[i])
		base64Kubeconfigs[i], kubeconfigs[i] = makeKubeconfig(clusters[i], tokens[i], caDatas[i])
		remoteSecretClusters[i] = makeRemoteSecret(clusters[i], kubeconfigs[i])
		pilotTokenSecrets[i] = makeTokenSecret(tokens[i], caDatas[i])

		apiConfig.Contexts[clusters[i].Context] = &api.Context{
			Cluster: clusters[i].Context,
		}
		apiConfig.Clusters[clusters[i].Context] = &api.Cluster{
			Server: makeServerName(clusters[i]),
		}
	}

	cluster0IstioNotInstalled = cloneCluster(clusters[0])
	cluster0IstioNotInstalled.installed = false

	cluster1DisableRegistryJoin = cloneCluster(clusters[1])
	cluster1DisableRegistryJoin.DisableRegistryJoin = true

	apiConfig.CurrentContext = clusters[0].Context
}

// cmp.Diff helper function for sorting slices of secrets.
func lessSecret(a, b *v1.Secret) bool {
	switch {
	case a.Namespace < b.Namespace:
		return true
	case a.Name < b.Name:
		return true
	default:
		return false
	}
}

// create a map key for a k8s action
func action(verb, resource string) string {
	return fmt.Sprintf("%v/%v", resource, verb)
}

// StringData is write-only. Simulate kube-apiserver logic and convert StringData to BinaryData
func simulateWriteOnlyKubeApiserverBehavior(secret *v1.Secret) *v1.Secret {
	fixedSecret := secret.DeepCopy()

	if fixedSecret.Data == nil {
		fixedSecret.Data = make(map[string][]byte, len(fixedSecret.StringData))
		for k, strV := range fixedSecret.StringData {
			fixedSecret.Data[k] = []byte(strV)
		}
		fixedSecret.StringData = nil // clear write-only field
	}
	return fixedSecret
}

type applyTestCase struct {
	clusters    []*Cluster
	config      *api.Config
	initObjs    map[types.UID][]runtime.Object
	wantSecrets map[types.UID][]*v1.Secret
	wantActions map[types.UID]map[string]int // verb+resource
	wantErr     bool
}

func runApplyTest(t *testing.T, testCase *applyTestCase) {
	t.Helper()

	g := NewWithT(t)

	env := newFakeEnvironmentOrDie(t, testCase.config)
	mesh := NewMesh(&MeshDesc{MeshID: "MyMeshID"}, testCase.clusters...)

	fakeClients := make(map[types.UID]*fake.Clientset, len(testCase.clusters))
	for _, cluster := range testCase.clusters {
		// create fake client with initial set of objections
		client := fake.NewSimpleClientset(testCase.initObjs[cluster.uid]...)
		fakeClients[cluster.uid] = client
		cluster.client = client

		mesh.addCluster(cluster)
	}

	err := apply(mesh, env)
	if testCase.wantErr {
		g.Expect(err).To(HaveOccurred())
	} else {
		g.Expect(err).NotTo(HaveOccurred())
	}

	// verify test results
	for _, cluster := range testCase.clusters {
		t.Run(fmt.Sprintf("cluster %v", cluster.Context), func(tt *testing.T) {
			tt.Helper()

			uid := cluster.uid
			fakeClient := fakeClients[uid]

			secretList, err := fakeClient.CoreV1().Secrets(cluster.Namespace).List(metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())

			gotSecrets := make([]*v1.Secret, 0, len(secretList.Items))
			for _, secret := range secretList.Items {
				gotSecrets = append(gotSecrets, simulateWriteOnlyKubeApiserverBehavior(&secret))
			}

			if diff := cmp.Diff(testCase.wantSecrets[uid], gotSecrets, cmpopts.SortSlices(lessSecret)); diff != "" {
				tt.Errorf("\n got %v\nwant %v\ndiff %v",
					gotSecrets, testCase.wantSecrets[uid], diff)
			}

			wantActions := testCase.wantActions[uid]
			gotActions := make(map[string]int)
			for _, a := range fakeClient.Actions() {
				gotActions[action(a.GetVerb(), a.GetResource().Resource)]++
			}

			if diff := cmp.Diff(wantActions, gotActions); diff != "" {
				tt.Errorf("wrong set of actions:\n got %v want %v diff %v",
					gotActions, wantActions, diff)
			}
		})
	}
}

func TestApply_InitialSuccess(t *testing.T) {
	testCase := &applyTestCase{
		clusters: clusters[:],
		config:   apiConfig,
		initObjs: map[types.UID][]runtime.Object{
			clusters[0].uid: {pilotServiceAccount, pilotTokenSecrets[0], kubeSystemNamespaces[0]},
			clusters[1].uid: {pilotServiceAccount, pilotTokenSecrets[1], kubeSystemNamespaces[1]},
			clusters[2].uid: {pilotServiceAccount, pilotTokenSecrets[2], kubeSystemNamespaces[2]},
		},
		wantSecrets: map[types.UID][]*v1.Secret{
			clusters[0].uid: {remoteSecretClusters[1], remoteSecretClusters[2], pilotTokenSecrets[0]},
			clusters[1].uid: {remoteSecretClusters[0], remoteSecretClusters[2], pilotTokenSecrets[1]},
			clusters[2].uid: {remoteSecretClusters[0], remoteSecretClusters[1], pilotTokenSecrets[2]},
		},
		wantActions: map[types.UID]map[string]int{
			clusters[0].uid: {
				action("get", "secrets"):         3,
				action("list", "secrets"):        2,
				action("create", "secrets"):      2,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
			clusters[1].uid: {
				action("get", "secrets"):         3,
				action("list", "secrets"):        2,
				action("create", "secrets"):      2,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
			clusters[2].uid: {
				action("get", "secrets"):         3,
				action("list", "secrets"):        2,
				action("create", "secrets"):      2,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
		},
	}

	runApplyTest(t, testCase)
}

func TestApply_SingleClusterMesh(t *testing.T) {
	testCase := &applyTestCase{
		clusters: clusters[0:1],
		config:   apiConfig,
		initObjs: map[types.UID][]runtime.Object{
			clusters[0].uid: {pilotServiceAccount, pilotTokenSecrets[0], kubeSystemNamespaces[0]},
		},
		wantSecrets: map[types.UID][]*v1.Secret{
			clusters[0].uid: {pilotTokenSecrets[0]},
		},
		wantActions: map[types.UID]map[string]int{
			clusters[0].uid: {
				action("get", "secrets"):         1,
				action("list", "secrets"):        2,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
		},
	}

	runApplyTest(t, testCase)
}

func TestApply_IstioNotInstalledInOneCluster(t *testing.T) {
	testCase := &applyTestCase{
		clusters: []*Cluster{cluster0IstioNotInstalled, clusters[1], clusters[2]},
		config:   apiConfig,
		initObjs: map[types.UID][]runtime.Object{
			cluster0IstioNotInstalled.uid: {kubeSystemNamespaces[0]},
			clusters[1].uid:               {pilotServiceAccount, pilotTokenSecrets[1], kubeSystemNamespaces[1]},
			clusters[2].uid:               {pilotServiceAccount, pilotTokenSecrets[2], kubeSystemNamespaces[2]},
		},
		wantSecrets: map[types.UID][]*v1.Secret{
			cluster0IstioNotInstalled.uid: {},
			clusters[1].uid:               {remoteSecretClusters[2], pilotTokenSecrets[1]},
			clusters[2].uid:               {remoteSecretClusters[1], pilotTokenSecrets[2]},
		},
		wantActions: map[types.UID]map[string]int{
			cluster0IstioNotInstalled.uid: {
				action("list", "secrets"): 1,
			},
			clusters[1].uid: {
				action("get", "secrets"):         2,
				action("list", "secrets"):        2,
				action("create", "secrets"):      1,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
			clusters[2].uid: {
				action("get", "secrets"):         2,
				action("list", "secrets"):        2,
				action("create", "secrets"):      1,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
		},
	}

	runApplyTest(t, testCase)
}

func TestApply_DisableRegistryInOneCluster(t *testing.T) {
	testCase := &applyTestCase{
		clusters: []*Cluster{clusters[0], cluster1DisableRegistryJoin, clusters[2]},
		config:   apiConfig,
		initObjs: map[types.UID][]runtime.Object{
			clusters[0].uid:                 {pilotServiceAccount, pilotTokenSecrets[0], kubeSystemNamespaces[0]},
			cluster1DisableRegistryJoin.uid: {pilotServiceAccount, pilotTokenSecrets[1], kubeSystemNamespaces[1]},
			clusters[2].uid:                 {pilotServiceAccount, pilotTokenSecrets[2], kubeSystemNamespaces[2]},
		},
		wantSecrets: map[types.UID][]*v1.Secret{
			clusters[0].uid:                 {remoteSecretClusters[2], pilotTokenSecrets[0]},
			cluster1DisableRegistryJoin.uid: {pilotTokenSecrets[1]},
			clusters[2].uid:                 {remoteSecretClusters[0], pilotTokenSecrets[2]},
		},
		wantActions: map[types.UID]map[string]int{
			clusters[0].uid: {
				action("get", "secrets"):         2,
				action("list", "secrets"):        2,
				action("create", "secrets"):      1,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
			clusters[1].uid: {
				action("list", "secrets"):        2,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
				action("get", "secrets"):         1,
			},
			clusters[2].uid: {
				action("get", "secrets"):         2,
				action("list", "secrets"):        2,
				action("create", "secrets"):      1,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
		},
	}

	runApplyTest(t, testCase)
}

func TestApply_JoinClusterToExistingMesh(t *testing.T) {
	// Cluster 0 and 1 are in mesh. Adding cluster 2.
	testCase := &applyTestCase{
		clusters: clusters[:],
		config:   apiConfig,
		initObjs: map[types.UID][]runtime.Object{
			clusters[0].uid: {pilotServiceAccount, pilotTokenSecrets[0], kubeSystemNamespaces[0], remoteSecretClusters[1]},
			clusters[1].uid: {pilotServiceAccount, pilotTokenSecrets[1], kubeSystemNamespaces[1], remoteSecretClusters[0]},
			clusters[2].uid: {pilotServiceAccount, pilotTokenSecrets[2], kubeSystemNamespaces[2]},
		},
		wantSecrets: map[types.UID][]*v1.Secret{
			clusters[0].uid: {remoteSecretClusters[1], remoteSecretClusters[2], pilotTokenSecrets[0]},
			clusters[1].uid: {remoteSecretClusters[0], remoteSecretClusters[2], pilotTokenSecrets[1]},
			clusters[2].uid: {remoteSecretClusters[0], remoteSecretClusters[1], pilotTokenSecrets[2]},
		},
		wantActions: map[types.UID]map[string]int{
			clusters[0].uid: {
				action("get", "secrets"):         3,
				action("list", "secrets"):        2,
				action("create", "secrets"):      1,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
			clusters[1].uid: {
				action("get", "secrets"):         3,
				action("list", "secrets"):        2,
				action("create", "secrets"):      1,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
			clusters[2].uid: {
				action("get", "secrets"):         3,
				action("list", "secrets"):        2,
				action("create", "secrets"):      2,
				action("get", "namespaces"):      1,
				action("get", "serviceaccounts"): 1,
			},
		},
	}

	runApplyTest(t, testCase)
}
