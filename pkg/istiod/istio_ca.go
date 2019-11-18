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

package istiod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	oidc "github.com/coreos/go-oidc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"istio.io/istio/pkg/spiffe"
	"istio.io/istio/security/pkg/cmd"
	"istio.io/istio/security/pkg/pki/ca"
	caserver "istio.io/istio/security/pkg/server/ca"
	"istio.io/istio/security/pkg/server/ca/authenticate"
	"istio.io/pkg/env"
	"istio.io/pkg/log"
)

// Based on istio_ca main - removing creation of Secrets with private keys in all namespaces and install complexity.
//
// For backward compat, will preserve support for the "cacerts" Secret used for self-signed certificates.
// It is mounted in the same location, and if found will be used - creating the secret is sufficient, no need for
// extra options.
//
// In old installer, the localCertDir is hardcoded to /etc/cacerts and mounted from "cacerts" secret.
//
// Support for signing other root CA has been removed - too dangerous, no clear use case.
//
// Default config, for backward compat with Citadel:
// - if "cacerts" secret exists in istio-system, will be mounted. It may contain an optional "root-cert.pem",
// with additional roots and optional {ca-key, ca-cert, cert-chain}.pem user-provided root CA.
// - if user-provided root CA is not found, the Secret "istio-ca-secret" is used, with ca-cert.pem and ca-key.pem files.
// - if neither is found, istio-ca-secret will be created.
//
// - a config map "istio-security" with a "caTLSRootCert" file will be used for root cert, and created if needed.
//   The config map was used by node agent - no longer possible to use in sds-agent, but we still save it for
//   backward compat. Will be removed with the node-agent. sds-agent is calling NewCitadelClient directly, using
//   K8S root.

var (
	// This replaces the "cert-chain", "signing-cert" and "signing-key" flags in citadel - Istio installer is
	// requires a secret named "cacerts" with specific files inside.
	localCertDir = env.RegisterStringVar("ROOT_CA_DIR", "./etc/cacerts",
		"Location of a local or mounted CA root")

	workloadCertTTL = env.RegisterDurationVar("MAX_WORKLOAD_CERT_TTL",
		cmd.DefaultWorkloadCertTTL,
		"The TTL of issued workload certificates.")

	maxWorkloadCertTTL = env.RegisterDurationVar("MAX_WORKLOAD_CERT_TTL",
		cmd.DefaultMaxWorkloadCertTTL,
		"The max TTL of issued workload certificates.")

	selfSignedCACertTTL = env.RegisterDurationVar("CITADEL_SELF_SIGNED_CA_CERT_TTL",
		cmd.DefaultSelfSignedCACertTTL,
		"The TTL of self-signed CA root certificate.")

	selfSignedRootCertCheckInterval = env.RegisterDurationVar("CITADEL_SELF_SIGNED_ROOT_CERT_CHECK_INTERVAL",
		cmd.DefaultSelfSignedRootCertCheckInterval,
		"The interval that self-signed CA checks its root certificate "+
			"expiration time and rotates root certificate. Setting this interval "+
			"to zero or a negative value disables automated root cert check and "+
			"rotation. This interval is suggested to be larger than 10 minutes.")

	selfSignedRootCertGracePeriodPercentile = env.RegisterIntVar("CITADEL_SELF_SIGNED_ROOT_CERT_GRACE_PERIOD_PERCENTILE",
		cmd.DefaultRootCertGracePeriodPercentile,
		"Grace period percentile for self-signed root cert.")

	enableJitterForRootCertRotator = env.RegisterBoolVar("CITADEL_ENABLE_JITTER_FOR_ROOT_CERT_ROTATOR",
		true,
		"If true, set up a jitter to start root cert rotator. "+
			"Jitter selects a backoff time in seconds to start root cert rotator, "+
			"and the back off time is below root cert check interval.")

	k8sInCluster = env.RegisterStringVar("KUBERNETES_SERVICE_HOST", "",
		"Kuberenetes service host, set automatically when running in-cluster")

	// JWTPath is the well-knwon location of the projected K8S JWT. This is mounted on all workloads, as well as istiod.
	// In a cluster that doesn't support projected JWTs we can't run the CA functionality of istiod - instead
	// old-style Citadel must be run, with Secret created for each workload.
	JWTPath = "./var/run/secrets/tokens/istio-token"

	// This value can also be extracted from the mounted token
	trustedIssuer = env.RegisterStringVar("TOKEN_ISSUER", "",
		"OIDC token issuer. If set, will be used to check the tokens.")

	audience = env.RegisterStringVar("AUDIENCE", "istio-ca",
		"Expected audience in the tokens. For backward compat, default is istio-ca.")
)

const (
	bearerTokenPrefix = "Bearer "
	httpAuthHeader    = "authorization"
	identityTemplate  = "spiffe://%s/ns/%s/sa/%s"
)

type CAOptions struct {
	// domain to use in SPIFFE identity URLs
	TrustDomain string
}

// RunCA will start the cert signing GRPC service on an existing server.
func RunCA(grpc *grpc.Server, cs kubernetes.Interface, opts *CAOptions) {
	ca := createCA(cs.CoreV1(), opts)

	iss := trustedIssuer.Get()
	aud := audience.Get()

	ch := make(chan struct{})
	if token, err := ioutil.ReadFile(JWTPath); err != nil {
		// for debug we may want to override this by setting trustedIssuer explicitly
		if iss == "" {
			log.Warna("istiod running without access to K8S tokens. Disable the CA functionality",
				JWTPath)
			return
		}
	} else {
		tok, err := detectAuthEnv(string(token))
		if err != nil {
			log.Warna("Starting with invalid K8S JWT token", err, string(token))
		} else {
			if iss == "" {
				iss = tok.Iss
			}
			if len(tok.Aud) > 0 {
				aud = tok.Aud[0]
			}
		}
	}

	// The CA API uses cert with the max workload cert TTL.
	// 'hostlist' must be non-empty - but is not used since a grpc server is passed.
	caServer, startErr := caserver.NewWithGRPC(grpc, ca, maxWorkloadCertTTL.Get(),
		false, []string{"istiod.istio-system"}, 0, spiffe.GetTrustDomain(),
		true)
	if startErr != nil {
		log.Fatalf("failed to create istio ca server: %v", startErr)
	}

	// TODO: if not set, parse Istiod's own token (if present) and get the issuer. The same issuer is used
	// for all tokens - no need to configure twice. The token may also include cluster info to auto-configure
	// networking properties.
	if iss != "" && // issuer set explicitly or extracted from our own JWT
		(k8sInCluster.Get() != "" || trustedIssuer.Get() != "") { // either set explicitly, or not running in cluster.
		// Add a custom authenticator using standard JWT validation, if not running in K8S
		// When running inside K8S - we can use the built-in validator, which also check pod removal (invalidation).
		oidcAuth, err := newJwtAuthenticator(iss, opts.TrustDomain, aud)
		if err == nil {
			caServer.Authenticators = append(caServer.Authenticators, oidcAuth)
			log.Infoa("Using out-of-cluster JWT authentication")
		} else {
			log.Infoa("K8S token doesn't support OIDC, using only in-cluster auth")
		}
	}

	if serverErr := caServer.Run(); serverErr != nil {
		// stop the registry-related controllers
		ch <- struct{}{}

		log.Warnf("Failed to start GRPC server with error: %v", serverErr)
	}
	log.Info("Istiod CA has started")
}

type jwtAuthenticator struct {
	provider    *oidc.Provider
	verifier    *oidc.IDTokenVerifier
	trustDomain string
}

// newJwtAuthenticator is used when running istiod outside of a cluster, to validate the tokens using OIDC
// K8S is created with --service-account-issuer, service-account-signing-key-file and service-account-api-audiences
// which enable OIDC.
func newJwtAuthenticator(iss string, trustDomain, audience string) (*jwtAuthenticator, error) {
	provider, err := oidc.NewProvider(context.Background(), iss)
	if err != nil {
		return nil, fmt.Errorf("running in cluster with K8S tokens, but failed to initialize %s %s", iss, err)
	}

	return &jwtAuthenticator{
		trustDomain: trustDomain,
		provider:    provider,
		verifier:    provider.Verifier(&oidc.Config{ClientID: audience}),
	}, nil
}

// Authenticate - based on the old OIDC authenticator for mesh expansion.
func (j *jwtAuthenticator) Authenticate(ctx context.Context) (*authenticate.Caller, error) {
	bearerToken, err := extractBearerToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("ID token extraction error: %v", err)
	}

	idToken, err := j.verifier.Verify(context.Background(), bearerToken)
	if err != nil {
		return nil, fmt.Errorf("failed to verify the ID token (error %v)", err)
	}

	// for GCP-issued JWT, the service account is in the "email" field
	sa := &jwtPayload{}

	if err := idToken.Claims(&sa); err != nil {
		return nil, fmt.Errorf("failed to extract email field from ID token: %v", err)
	}
	if !strings.HasPrefix(sa.Sub, "system:serviceaccount") {
		return nil, fmt.Errorf("invalid sub %v", sa.Sub)
	}
	parts := strings.Split(sa.Sub, ":")
	ns := parts[2]
	ksa := parts[3]

	return &authenticate.Caller{
		AuthSource: authenticate.AuthSourceIDToken,
		Identities: []string{fmt.Sprintf(identityTemplate, j.trustDomain, ns, ksa)},
	}, nil

}

func extractBearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("no metadata is attached")
	}

	authHeader, exists := md[httpAuthHeader]
	if !exists {
		return "", fmt.Errorf("no HTTP authorization header exists")
	}

	for _, value := range authHeader {
		if strings.HasPrefix(value, bearerTokenPrefix) {
			return strings.TrimPrefix(value, bearerTokenPrefix), nil
		}
	}

	return "", fmt.Errorf("no bearer token exists in HTTP authorization header")
}

type jwtPayload struct {
	// Aud is the expected audience, defaults to istio-ca - but is based on istiod.yaml configuration.
	// If set to a different value - use the value defined by istiod.yaml. Env variable can
	// still override
	Aud []string `json:"aud"`

	// Exp is not currently used - we don't use the token for authn, just to determine k8s settings
	Exp int `json:"exp"`

	// Issuer - configured by K8S admin for projected tokens. Will be used to verify all tokens.
	Iss string `json:"iss"`

	Sub string `json:"sub"`
}

// detectAuthEnv will use the JWT token that is mounted in istiod to set the default audience
// and trust domain for Istiod, if not explicitly defined.
// K8S will use the same kind of tokens for the pods, and the value in istiod's own token is
// simplest and safest way to have things match.
//
// Note that K8S is not required to use JWT tokens - we will fallback to the defaults
// or require explicit user option for K8S clusters using opaque tokens.
func detectAuthEnv(jwt string) (*jwtPayload, error) {
	jwtSplit := strings.Split(jwt, ".")
	if len(jwtSplit) != 3 {
		return nil, fmt.Errorf("invalid JWT parts: %s", jwt)
	}
	payload := jwtSplit[1]

	payloadBytes, err := base64.RawStdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to decode jwt: %v", err.Error())
	}

	structuredPayload := &jwtPayload{}
	err = json.Unmarshal(payloadBytes, &structuredPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal jwt: %v", err.Error())
	}

	return structuredPayload, nil
}

func (j jwtAuthenticator) AuthenticatorType() string {
	return authenticate.IDTokenAuthenticatorType
}

func createCA(client corev1.CoreV1Interface, opts *CAOptions) *ca.IstioCA {
	var caOpts *ca.IstioCAOptions
	var err error

	signingKeyFile := path.Join(localCertDir.Get(), "ca-key.pem")

	// If not found, will default to ca-cert.pem. May contain multiple roots.
	rootCertFile := path.Join(localCertDir.Get(), "root-cert.pem")
	if _, err := os.Stat(rootCertFile); err != nil {
		// In Citadel, normal self-signed doesn't use a root-cert.pem file for additional roots.
		// In Istiod, it is possible to provide one via "cacerts" secret in both cases, for consistency.
		rootCertFile = ""
	}

	if _, err := os.Stat(signingKeyFile); err != nil {
		// The user-provided certs are missing - create a self-signed cert.

		log.Info("Use self-signed certificate as the CA certificate")
		spiffe.SetTrustDomain(opts.TrustDomain)
		// Abort after 20 minutes.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute*20)
		defer cancel()
		// rootCertFile will be added to "ca-cert.pem".

		// readSigningCertOnly set to false - it doesn't seem to be used in Citadel, nor do we have a way
		// to set it only for one job.
		caOpts, err = ca.NewSelfSignedIstioCAOptions(ctx,
			selfSignedRootCertGracePeriodPercentile.Get(), selfSignedCACertTTL.Get(),
			selfSignedRootCertCheckInterval.Get(), workloadCertTTL.Get(),
			maxWorkloadCertTTL.Get(), opts.TrustDomain, true,
			IstiodNamespace.Get(), -1, client, rootCertFile,
			enableJitterForRootCertRotator.Get())
		if err != nil {
			log.Fatalf("Failed to create a self-signed Citadel (error: %v)", err)
		}
	} else {
		log.Info("Use local CA certificate")

		// The cert corresponding to the key, self-signed or chain.
		// rootCertFile will be added at the end, if present, to form 'rootCerts'.
		signingCertFile := path.Join(localCertDir.Get(), "ca-cert.pem")
		//
		certChainFile := path.Join(localCertDir.Get(), "cert-chain.pem")

		caOpts, err = ca.NewPluggedCertIstioCAOptions(certChainFile, signingCertFile, signingKeyFile,
			rootCertFile, workloadCertTTL.Get(), maxWorkloadCertTTL.Get(), IstiodNamespace.Get(), client)
		if err != nil {
			log.Fatalf("Failed to create an Citadel (error: %v)", err)
		}
	}

	istioCA, err := ca.NewIstioCA(caOpts)
	if err != nil {
		log.Errorf("Failed to create an Citadel (error: %v)", err)
	}

	// TODO: provide an endpoint returning all the roots. SDS can only pull a single root in current impl.
	// ca.go saves or uses the secret, but also writes to the configmap "istio-security", under caTLSRootCert

	// rootCertRotatorChan channel accepts signals to stop root cert rotator for
	// self-signed CA.
	rootCertRotatorChan := make(chan struct{})
	// Start root cert rotator in a separate goroutine.
	istioCA.Run(rootCertRotatorChan)

	return istioCA
}
