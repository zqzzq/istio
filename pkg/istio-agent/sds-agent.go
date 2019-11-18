// Copyright 2019 Istio Authors
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

package istioagent

import (
	"context"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"istio.io/istio/pkg/kube"
	caClientInterface "istio.io/istio/security/pkg/nodeagent/caclient/interface"
	citadel "istio.io/istio/security/pkg/nodeagent/caclient/providers/citadel"
	gca "istio.io/istio/security/pkg/nodeagent/caclient/providers/google"
	"istio.io/istio/security/pkg/nodeagent/plugin/providers/google/stsclient"

	"istio.io/istio/security/pkg/nodeagent/cache"
	"istio.io/istio/security/pkg/nodeagent/sds"
	"istio.io/istio/security/pkg/nodeagent/secretfetcher"
	"istio.io/pkg/env"
	"istio.io/pkg/log"
)

// To debug:
// curl -X POST localhost:15000/logging?config=trace - to see SendingDiscoveryRequest

// Breakpoints in secretcache.go GenerateSecret..

// Note that istiod currently can't validate the JWT token unless it runs on k8s
// Main problem is the JWT validation check which hardcodes the k8s server address and token location.
//
// To test on a local machine, for debugging:
//
// kis exec $POD -- cat /run/secrets/istio-token/istio-token > var/run/secrets/tokens/istio-token
// kis port-forward $POD 15010:15010 &
//
// You can also copy the K8S CA and a token to be used to connect to k8s - but will need removing the hardcoded addr
// kis exec $POD -- cat /run/secrets/kubernetes.io/serviceaccount/{ca.crt,token} > var/run/secrets/kubernetes.io/serviceaccount/
//
// Or disable the jwt validation while debugging SDS problems.

var (
	caProviderEnv = env.RegisterStringVar(caProvider, "Citadel", "").Get()
	// TODO: default to same as discovery address
	caEndpointEnv = env.RegisterStringVar(caEndpoint, "", "").Get()

	pluginNamesEnv             = env.RegisterStringVar(pluginNames, "", "").Get()
	enableIngressGatewaySDSEnv = env.RegisterBoolVar(enableIngressGatewaySDS, false, "").Get()

	trustDomainEnv                     = env.RegisterStringVar(trustDomain, "", "").Get()
	secretTTLEnv                       = env.RegisterDurationVar(secretTTL, 24*time.Hour, "").Get()
	secretRefreshGraceDurationEnv      = env.RegisterDurationVar(SecretRefreshGraceDuration, 1*time.Hour, "").Get()
	secretRotationIntervalEnv          = env.RegisterDurationVar(SecretRotationInterval, 10*time.Minute, "").Get()
	staledConnectionRecycleIntervalEnv = env.RegisterDurationVar(staledConnectionRecycleInterval, 5*time.Minute, "").Get()
	initialBackoffEnv                  = env.RegisterIntVar(InitialBackoff, 10, "").Get()

	// Location of a custom-mounted root (for example using Secret)
	mountedRoot = "/etc/certs/root-cert.pem"

	// Location of K8S CA root.
	k8sCAPath = "./var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

const (
	// name of authentication provider.
	caProvider = "CA_PROVIDER"

	// CA endpoint.
	caEndpoint = "CA_ADDR"

	// names of authentication provider's plugins.
	pluginNames = "PLUGINS"

	// The trust domain corresponds to the trust root of a system.
	// Refer to https://github.com/spiffe/spiffe/blob/master/standards/SPIFFE-ID.md#21-trust-domain
	trustDomain = "TRUST_DOMAIN"

	// The ingress gateway SDS mode allows node agent to provision credentials to ingress gateway
	// proxy by watching kubernetes secrets.
	enableIngressGatewaySDS = "ENABLE_INGRESS_GATEWAY_SDS"

	// The environmental variable name for secret TTL, node agent decides whether a secret
	// is expired if time.now - secret.createtime >= secretTTL.
	// example value format like "90m"
	secretTTL = "SECRET_TTL"

	// The environmental variable name for grace duration that secret is re-generated
	// before it's expired time.
	// example value format like "10m"
	SecretRefreshGraceDuration = "SECRET_GRACE_DURATION"

	// The environmental variable name for key rotation job running interval.
	// example value format like "20m"
	SecretRotationInterval = "SECRET_JOB_RUN_INTERVAL"

	// The environmental variable name for staled connection recycle job running interval.
	// example value format like "5m"
	staledConnectionRecycleInterval = "STALED_CONNECTION_RECYCLE_RUN_INTERVAL"

	// The environmental variable name for the initial backoff in milliseconds.
	// example value format like "10"
	InitialBackoff = "INITIAL_BACKOFF_MSEC"
)

var (
	// JWTPath is the default location of a JWT token to be used to authenticate with XDS and CA servers.
	// If the file is missing, the agent will fallback to using mounted certificates if XDS address is secure.
	JWTPath = "./var/run/secrets/tokens/istio-token"

	// LocalSDS is the location of the in-process SDS server - must be in a writeable dir.
	LocalSDS = "/etc/istio/proxy/SDS"

	workloadSdsCacheOptions cache.Options
	gatewaySdsCacheOptions  cache.Options
	serverOptions           sds.Options
	gatewaySecretChan       chan struct{}
)

// SDSAgent contains the configuration of the agent, based on the injected
// environment:
// - SDS hostPath if node-agent was used
// - /etc/certs/key if Citadel or other mounted Secrets are used
// - root cert to use for connecting to XDS server
// - CA address, with proper defaults and detection
type SDSAgent struct {
	// Location of JWTPath to connect to CA. If empty, SDS is not possible.
	// If set SDS will be used - either local or via hostPath.
	JWTPath string

	// SDSAddress is the address of the SDS server. Starts with unix: for hostpath mount or built-in
	// May also be a https address.
	SDSAddress string

	// CertPath is set with the location of the certs, or empty if mounted certs are not present.
	CertsPath string

	// RequireCerts is set if the agent requires certificates:
	// - if controlPlaneAuthEnabled is set
	// - port of discovery server is not 15010 (the plain text default).
	RequireCerts bool

	// Expected SAN
	SAN string
}

// NewSDSAgent wraps the logic for a local SDS. It will check if the JWT token required for local SDS is
// present, and set additional config options for the in-process SDS agent.
//
// The JWT token is currently using a pre-defined audience (istio-ca) or it must match the trust domain (WIP).
// If the JWT token is not present - the local SDS agent can't authenticate.
//
// If node agent and JWT are mounted: it indicates user injected a config using hostPath, and will be used.
//
func NewSDSAgent(discAddr string, tlsRequired bool) *SDSAgent {
	ac := &SDSAgent{}

	discHost, discPort, err := net.SplitHostPort(discAddr)
	if err != nil {
		log.Fatala("Invalid discovery address", discAddr, err)
	}

	if _, err := os.Stat(JWTPath); err == nil {
		ac.JWTPath = JWTPath
	} else {
		// Can't use in-process SDS.
		log.Warna("Missing JWT token, can't use in process SDS ", JWTPath, err)

		if discPort == "15012" {
			log.Fatala("Missing JWT, can't authenticate with control plane. Try using plain text (15010)")
		}
		return ac
	}

	ac.SDSAddress = "unix:" + LocalSDS

	if _, err := os.Stat("/etc/certs/key.pem"); err == nil {
		ac.CertsPath = "/etc/certs"
	}
	if tlsRequired {
		ac.RequireCerts = true
	}

	// Istiod uses a fixed, defined port for K8S-signed certificates.
	if discPort == "15012" {
		ac.RequireCerts = true
		// For local debugging - the discoveryAddress is set to localhost, but the cert issued for normal SA.
		if discHost == "localhost" {
			discHost = "istiod.istio-system"
		}
		ac.SAN = discHost
	}

	return ac
}

// Simplified SDS setup. This is called if and only if user has explicitly mounted a K8S JWT token, and is not
// using a hostPath mounted or external SDS server.
//
// 1. External CA: requires authenticating the trusted JWT AND validating the SAN against the JWT.
//    For example Google CA
//
// 2. Indirect, using istiod: using K8S cert.
//
// 3. Monitor mode - watching secret in same namespace ( Ingress)
//
// 4. TODO: File watching, for backward compat/migration from mounted secrets.
func (conf *SDSAgent) Start(isSidecar bool, podNamespace string) (*sds.Server, error) {
	applyEnvVars()

	gatewaySdsCacheOptions = workloadSdsCacheOptions

	// Next to the envoy config, writeable dir (mounted as mem)
	serverOptions.WorkloadUDSPath = LocalSDS
	serverOptions.UseLocalJWT = true

	// TODO: remove the caching, workload has a single cert
	workloadSecretCache, _ := newSecretCache(serverOptions)

	var gatewaySecretCache *cache.SecretCache
	if !isSidecar {
		serverOptions.EnableIngressGatewaySDS = true
		// TODO: what is the setting for ingress ?
		serverOptions.IngressGatewayUDSPath = serverOptions.WorkloadUDSPath + "_ROUTER"
		gatewaySecretCache = newIngressSecretCache(podNamespace)
	}

	// For sidecar and ingress we need to first get the certificates for the workload.
	// We'll also save them in files, for backward compat with servers generating files
	// TODO: use caClient.CSRSign() directly

	// fail hard if we need certs ( control plane security enabled ) and we don't have mounted certs and
	// we fail to load SDS
	fail := conf.RequireCerts && conf.CertsPath == ""

	tok, err := ioutil.ReadFile(conf.JWTPath)
	if err != nil && fail {
		log.Fatala("Failed to read token", err)
	} else {
		si, err := workloadSecretCache.GenerateSecret(context.Background(), "bootstrap", "default",
			string(tok))
		if err != nil {
			if fail {
				log.Fatala("Failed to get certificates", err)
			} else {
				log.Warna("Failed to get certificate from CA", err)
			}
		}
		if si != nil {
			// For debugging and backward compat - we may not need it long term
			// The files can be used if an Pilot configured with SDS disabled is used, will generate
			// file based XDS config instead of SDS.
			err = ioutil.WriteFile("/etc/istio/proxy/key.pem", si.PrivateKey, 0700)
			if err != nil {
				log.Fatalf("Failed to write certs: %v", err)
			}
			err = ioutil.WriteFile("/etc/istio/proxy/cert-chain.pem", si.CertificateChain, 0700)
			if err != nil {
				log.Fatalf("Failed to write certs: %v", err)
			}
		}
		sir, err := workloadSecretCache.GenerateSecret(context.Background(), "bootstrap", "ROOTCA",
			string(tok))
		if err != nil {
			if fail {
				log.Fatala("Failed to get certificates", err)
			} else {
				log.Warna("Failed to get certificate from CA", err)
			}
		}
		if sir != nil {
			// For debugging and backward compat - we may not need it long term
			// TODO: we should concatenate this file with the existing root-cert and possibly pilot-generated roots, for
			// smooth transition across CAs.
			err = ioutil.WriteFile("/etc/istio/proxy/root-cert.pem", sir.RootCert, 0700)
			if err != nil {
				log.Fatalf("Failed to write certs: %v", err)
			}
		}
	}

	server, err := sds.NewServer(serverOptions, workloadSecretCache, gatewaySecretCache)
	if err != nil {
		return nil, err
	}

	return server, nil
}

// newSecretCache creates the cache for workload secrets and/or gateway secrets.
func newSecretCache(serverOptions sds.Options) (workloadSecretCache *cache.SecretCache, caClient caClientInterface.Client) {
	ret := &secretfetcher.SecretFetcher{}

	// TODO: get the MC public keys from pilot.
	// TODO: root cert for Istiod from the K8S file or local override
	// In node agent, a controller is used getting 'istio-security.istio-system' config map
	// Single caTLSRootCert inside.

	var err error

	// TODO: this should all be packaged in a plugin, possibly with optional compilation.

	if (serverOptions.CAProviderName == "GoogleCA" || strings.Contains(serverOptions.CAEndpoint, "googleapis.com")) &&
		stsclient.GKEClusterURL != "" {
		// Use a plugin to an external CA - this has direct support for the K8S JWT token
		// This is only used if the proper env variables are injected - otherwise the existing Citadel or Istiod will be
		// used.
		caClient, err = gca.NewGoogleCAClient(serverOptions.CAEndpoint, true)
		serverOptions.PluginNames = []string{"GoogleTokenExchange"}
	} else {
		// Determine the default CA.
		// If /etc/certs exists - it means Citadel is used (possibly in a mode to only provision the root-cert, not keys)
		// Otherwise: default to istiod
		//
		// If an explicit CA is configured, assume it is mounting /etc/certs
		var rootCert []byte

		// explicitSecret is true if a /etc/certs/root-cert file has been mounted. Will be used
		// to authenticate the certificate of the SDS server (istiod or custom).
		explicitSecret := false

		if _, err := os.Stat(mountedRoot); err == nil {
			rootCert, err = ioutil.ReadFile(mountedRoot)
			if err != nil {
				log.Warna("Failed to load existing citadel root", err)
			} else {
				explicitSecret = true
			}
		}

		tls := true

		if serverOptions.CAEndpoint == "" {
			// Determine the default address, based on the presence of Citadel secrets
			if explicitSecret {
				log.Info("Using citadel CA for SDS")
				serverOptions.CAEndpoint = "istio-citadel.istio-system:8060"
			} else {
				rootCert, err = ioutil.ReadFile(k8sCAPath)
				if err != nil {
					log.Warna("Failed to load K8S cert, assume IP secure network ", err)
					serverOptions.CAEndpoint = "istiod.istio-system:15010"
				} else {
					log.Info("Using default istiod CA, with K8S certificates for SDS")
					serverOptions.CAEndpoint = "istiod.istio-system:15012"
				}
			}
		} else {
			// Explicitly configured CA
			log.Infoa("Using user-configured CA", serverOptions.CAEndpoint)
			if strings.HasSuffix(serverOptions.CAEndpoint, ":15010") {
				log.Warna("Debug mode or IP-secure network")
				tls = false
			}
			if strings.HasSuffix(serverOptions.CAEndpoint, ":15012") {
				rootCert, err = ioutil.ReadFile(k8sCAPath)
				if err != nil {
					log.Fatala("Invalid config - port 15012 expects a K8S-signed certificate but certs missing", err)
				}
			}
		}

		// Will use TLS unless the reserved 15010 port is used ( istiod on an ipsec/secure VPC)
		// rootCert may be nil - in which case the system roots are used, and the CA is expected to have public key
		// Otherwise assume the injection has mounted /etc/certs/root-cert.pem
		caClient, err = citadel.NewCitadelClient(serverOptions.CAEndpoint, tls, rootCert)
	}

	if err != nil {
		log.Errorf("failed to create secretFetcher for workload proxy: %v", err)
		os.Exit(1)
	}
	ret.UseCaClient = true
	ret.CaClient = caClient

	workloadSdsCacheOptions.TrustDomain = serverOptions.TrustDomain
	workloadSdsCacheOptions.Plugins = sds.NewPlugins(serverOptions.PluginNames)
	workloadSecretCache = cache.NewSecretCache(ret, sds.NotifyProxy, workloadSdsCacheOptions)
	return
}

// TODO: use existing 'sidecar/router' config to enable loading Secrets
func newIngressSecretCache(namespace string) (gatewaySecretCache *cache.SecretCache) {
	gSecretFetcher := &secretfetcher.SecretFetcher{
		UseCaClient: false,
	}

	cs, err := kube.CreateClientset("", "")

	if err != nil {
		log.Errorf("failed to create secretFetcher for gateway proxy: %v", err)
		os.Exit(1)
	}
	gSecretFetcher.FallbackSecretName = "gateway-fallback"

	gSecretFetcher.InitWithKubeClientAndNs(cs.CoreV1(), namespace)

	gatewaySecretChan = make(chan struct{})
	gSecretFetcher.Run(gatewaySecretChan)
	gatewaySecretCache = cache.NewSecretCache(gSecretFetcher, sds.NotifyProxy, gatewaySdsCacheOptions)
	return gatewaySecretCache
}

func applyEnvVars() {
	serverOptions.PluginNames = strings.Split(pluginNamesEnv, ",")

	serverOptions.EnableWorkloadSDS = true

	serverOptions.EnableIngressGatewaySDS = enableIngressGatewaySDSEnv
	serverOptions.CAProviderName = caProviderEnv
	serverOptions.CAEndpoint = caEndpointEnv
	serverOptions.TrustDomain = trustDomainEnv
	workloadSdsCacheOptions.SecretTTL = secretTTLEnv
	workloadSdsCacheOptions.SecretRefreshGraceDuration = secretRefreshGraceDurationEnv
	workloadSdsCacheOptions.RotationInterval = secretRotationIntervalEnv

	serverOptions.RecycleInterval = staledConnectionRecycleIntervalEnv

	workloadSdsCacheOptions.InitialBackoff = int64(initialBackoffEnv)
}
