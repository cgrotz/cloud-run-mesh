// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mesh

import (
	"context"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

type Cfg interface {
	GetSecret(ctx context.Context, ns string, name string) (map[string][]byte, error)
	GetCM(ctx context.Context, ns string, name string) (map[string]string, error)
}

type TokenProvider interface {
	GetToken(ctx context.Context, aud string) (string, error)
}

// KRun allows running an app in an Istio and K8S environment.
type KRun struct {
	// BaseDir is the root directory for all created files and all lookups.
	// If empty, will default to "/" when running as root, and "./" when running as regular user.
	// MESH_BASE_DIR will override it.
	BaseDir string

	// Config maps to 'mount'. Key is the config map name, value is a path.
	// Config mounts are optional (for now)
	CM2Dirs map[string]string

	// Audience to files. For each key, a k8s token with the given audience
	// will be created. Files should be under /var/run/secrets
	Aud2File map[string]string

	// ProxyConfig is a subset of istio ProxyConfig
	ProxyConfig *ProxyConfig

	// Address of the XDS server. If not specified, MCP is used.
	XDSAddr string

	// MeshTenant. Only set if using MCP or external Istiod.
	// Opaque, internal string that identifies the mesh to the XDS server.
	// Different from meshID - which is the user-visible form.
	MeshTenant string

	MeshConnectorAddr         string
	MeshConnectorInternalAddr string

	// Canonical name for the application.
	// Will be set as "app" and "service.istio.io/canonical-name" labels
	//
	// If not set "default" will be used.
	// TODO: use service name as default
	Name string

	// Revision
	Rev string

	// If not empty, will run Istio-agent as a gateway (router instead of sidecar)
	// with the "istio: $Gateway" label.
	Gateway string

	// Agent debug config (example dns:debug).
	// Based on ISTIO_DEBUG
	AgentDebug string

	// Namespace for the application. The user running the command must have
	// the appropriate Token, Secret, ConfigMap permissions in the namespace.
	//
	// If not set, "default" will be used.
	// TODO: use the GSA name as default namespace.
	Namespace string

	// KSA is the k8s service account for getting tokens.
	//
	// If not set, "default" will be used.
	// TODO: use service name as default
	KSA string

	// Primary client is the k8s client to use. If not set will be created based on
	// the config.
	Client *kubernetes.Clientset

	// ProjectId is the name of the project where config cluster is running
	// The workload may be in a different project.
	ProjectId string

	// ProjectNumber is used for GCP federated token exchange.
	// It is populated from the mesh-env PROJECT_NUMBER setting to construct the federated P4SA
	//    "service-" + s.kr.ProjectNumber + "@gcp-sa-meshdataplane.iam.gserviceaccount.com"
	// This is used for MeshCA and Stackdriver access.
	ProjectNumber string

	// Deprecated - ClusterAddress used instead.
	ClusterName string

	// TODO: replace with Workloadlocation. Config cluster location not used.
	ClusterLocation string

	agentCmd    *exec.Cmd
	appCmd      *exec.Cmd
	TrustDomain string

	StartTime  time.Time
	Labels     map[string]string
	VendorInit func(context.Context, *KRun) error

	// WhiteboxMode indicates no iptables capture
	WhiteboxMode bool
	InCluster    bool

	// PEM cert roots detected in the cluster - Citadel, custom CAs from mesh config.
	// Will be saved to a file.
	CARoots []string

	// Citadel root(s) - PEM format, may have multiple roots.
	CitadelRoot string

	// MeshAddr is the location of the mesh environment file.
	// This will be loaded at startup (TODO: and periodically or on demand for dynamic changes - XDS may also
	// push configs)
	//
	//
	//
	// Supported formats:
	// - https://.... - regular URL, using system certificates. Will return the mesh env directly.
	// - file://... - load from file
	// - gke://CONFIG_PROJECT_ID[/CLUSTER_LOCATION/CLUSTER_NAME/WORKLOAD_NAMESPACE] - GKE Container API.
	MeshAddr *url.URL

	// Config cluster address - https://container.googleapis.com/v1/projects/%s/locations/%s/clusters/%s
	// Used in the identitynamespace config for STS exchange.
	ClusterAddress string

	InstanceID string

	// Content of the 'mesh environment' - loaded from the config file.
	MeshEnv map[string]string

	CSRSigner CSRSigner

	// Interface to abstract k8s implementation
	TokenProvider    TokenProvider
	Cfg              Cfg
	TransportWrapper func(transport http.RoundTripper) http.RoundTripper

	// Function to call after config has been loaded, before init certs.
	PostConfigLoad   func(ctx context.Context, kr *KRun) error
}

var Debug = false

// New creates an uninitialized mesh launcher.
func New() *KRun {
	kr := &KRun{
		StartTime:   time.Now(),
		Aud2File:    map[string]string{},
		Labels:      map[string]string{},
		ProxyConfig: &ProxyConfig{},
		MeshEnv: map[string]string{},
	}
	kr.initFromEnv()
	return kr
}

// Extract Region from ClusterLocation
func (kr *KRun) Region() string {
	p := strings.Split(kr.ClusterLocation, "-")
	if len(p) < 3 {
		return kr.ClusterLocation
	}
	return strings.Join(p[0:2], "-")
}

// initFromEnv will use the env variables, metadata server and cluster configmaps
// to get the initial configuration for Istio and KRun.
//
func (kr *KRun) initFromEnv() {
	mesh := kr.Config("MESH", "")
	if mesh != "" {
		meshURL, err := url.Parse(mesh)
		if err != nil {
			log.Println("Ignoring invalid meshURL", mesh, err)
		}
		kr.MeshAddr = meshURL
	}

	// TODO: if meshURL is set and is file:// or gke:// - use it directly


	if kr.KSA == "" {
		// Same environment used for VMs
		kr.KSA = os.Getenv("WORKLOAD_SERVICE_ACCOUNT")
	}
	// TODO: on GKE detect KSA from the JWT or workload cert.
	// Same for trust domain if workload certs are enabled
	if kr.KSA == "" {
		kr.KSA = "default"
	}

	if kr.Namespace == "" {
		// Same environment used for VMs
		kr.Namespace = os.Getenv("WORKLOAD_NAMESPACE")
	}
	// TODO: detect the namespace from the JWT token if on GKE

	if kr.Name == "" {
		kr.Name = os.Getenv("WORKLOAD_NAME")
	}
	if kr.Gateway == "" {
		kr.Gateway = os.Getenv("GATEWAY_NAME")
	}
	if kr.MeshTenant == "" {
		kr.MeshTenant = os.Getenv("MESH_TENANT")
	}

	ks := os.Getenv("K_SERVICE")
	if kr.Name == "" {
		verNsName := strings.SplitN(ks, "--", 2)
		if len(verNsName) > 1 {
			ks = verNsName[1]
			kr.Labels["ver"] = verNsName[0]
		} else {
			kr.Name = ks
		}
	}

	if kr.Namespace == "" {
		kr.Namespace = "default"
	}
	if kr.Name == "" {
		kr.Name = kr.Namespace
	}

	kr.Aud2File = map[string]string{}
	prefix := "."
	if os.Getuid() == 0 {
		prefix = ""
	}
	if kr.BaseDir == "" {
		kr.BaseDir = os.Getenv("MESH_BASE_DIR")
	}
	if kr.BaseDir != "" {
		prefix = kr.BaseDir
	} else {
		kr.BaseDir = prefix
	}

	if kr.TrustDomain == "" {
		kr.TrustDomain = os.Getenv("TRUST_DOMAIN")
	}
	if kr.TrustDomain == "" && kr.ProjectId != "" {
		kr.TrustDomain = kr.ProjectId + ".svc.id.goog"
	}
	// This can be used to provide a k8s-like environment, for apps that need it.
	// It might be better to just generate a kubeconfig file and not pretend we are inside a cluster.
	//if !kr.InCluster {
	//	kr.Aud2File["api"] = prefix + "/var/run/secrets/kubernetes.io/serviceaccount/token"
	//}
	if kr.KSA == "" {
		kr.KSA = "default"
	}

	// TODO: stop using this, use ProxyConfig.DiscoveryAddress instead
	if kr.XDSAddr == "" {
		kr.XDSAddr = os.Getenv("XDS_ADDR")
	}

	pc := os.Getenv("PROXY_CONFIG")
	if pc != "" {
		err := yaml.Unmarshal([]byte(pc), &kr.ProxyConfig)
		if err != nil {
			log.Println("Invalid ProxyConfig, ignoring", err)
		}
		if kr.ProxyConfig.DiscoveryAddress != "" {
			kr.XDSAddr = kr.ProxyConfig.DiscoveryAddress
		}
	}

	// Advanced options

	// example dns:debug
	kr.AgentDebug = kr.Config("XDS_AGENT_DEBUG", "")
}


func (kr *KRun) LoadConfig(ctx context.Context) error {
	// It is possible to have only one of the 2 mesh connector services installed
	if kr.XDSAddr == "" || kr.ProjectNumber == "" ||
			(kr.MeshConnectorAddr == "" && kr.MeshConnectorInternalAddr == "") {

		err := kr.loadMeshEnv(ctx)
		if err != nil {
			return err
		}
		// Adjust 'derived' values if needed.
		if kr.TrustDomain == "" && kr.ProjectId != "" {
			kr.TrustDomain = kr.ProjectId + ".svc.id.goog"
		}
	}

	return nil
}

// RefreshAndSaveTokens is run periodically to create token, secrets, config map files.
// The primary use is istio token expected by pilot agent.
// This should not be called unless pilot-agent/envoy  or proxyless gRPC without library are used.
// pilot-agent is currently refreshing the certificates - WIP to move that here.
//
// Certs for 'direct' (library) use can be created without saving the tokens.
// 'library' means linking this or a similar package with the application.
func (kr *KRun) RefreshAndSaveTokens() {
	for aud, f := range kr.Aud2File {
		kr.saveTokenToFile(kr.Namespace, aud, f)
	}
	time.AfterFunc(30*time.Minute, kr.RefreshAndSaveTokens)
}

func (kr *KRun) saveTokenToFile(ns string, audience string, destFile string) error {
	t, err := kr.GetToken(context.TODO(), audience)
	if err != nil {
		log.Println("Error creating ", ns, kr.KSA, audience, err)
		return err
	}
	lastSlash := strings.LastIndex(destFile, "/")
	err = os.MkdirAll(destFile[:lastSlash], 0755)
	if err != nil {
		log.Println("Error creating dir", ns, kr.KSA, destFile[:lastSlash])
	}
	// Save the token, readable by app. Little value to have istio token as different user,
	// for this separate container/sandbox is needed.
	err = ioutil.WriteFile(destFile, []byte(t), 0644)
	if err != nil {
		log.Println("Error creating ", ns, kr.KSA, audience, destFile, err)
		return err
	}

	return nil
}

// FindXDSAddr will determine which discovery address to use.
//
// The logic is:
// - if "mesh tenant" is set - use MCP. This is the main case.
// - if "mesh tehant" is not set - use the mesh connector for ASM/OSS
// - if an XDS_ADDR is explicitly set, use it - unless it is invalid ( MCP without tenant ID)
func (kr *KRun) FindXDSAddr() string {
	if kr.XDSAddr != "" {
		if (kr.MeshTenant == "-" || kr.MeshTenant == "") &&
			strings.Contains(kr.XDSAddr, "googleapis.com") &&
			strings.Contains(kr.XDSAddr, "meshconfig") {
			log.Println("Ignoring meshconfig XDS address without tenant, using mesh connector")
		} else {
			return kr.XDSAddr
		}
	}
	addr := ""
	if kr.MeshTenant == "-" || kr.MeshTenant == "" {
		// Explicitly in-cluster
		addr = kr.MeshConnectorAddr + ":15012"
	} else {
		// we have a mesh tenant - use MCP
		// For staging: explicitly set XDS_ADDR in mesh-env
		// To force use of in-cluster: set tenant to "-" in mesh-env
		addr = "meshconfig.googleapis.com:443"
	}
	return addr
}

// loadMeshEnv will lookup the 'mesh-env', an opaque config for the mesh.
// Currently it is loaded from K8S
// TODO: URL, like 'konfig' ( including gcp pseudo-URL like gcp://cluster.location.project/.... )
//
func (kr *KRun) loadMeshEnv(ctx context.Context) error {
	d, err := kr.Cfg.GetCM(ctx, "istio-system", "mesh-env")
	if err != nil {
		return err
	}
	return kr.initFromMeshEnv(d)
}

// initFromMeshEnv updates settings in KR - but only if they were not explicitly set by env
// variables.
func (kr *KRun) initFromMeshEnv(d map[string]string) error {
	kr.MeshEnv = d
	// See connector for supported values
	updateFromMap(d, "PROJECT_NUMBER", &kr.ProjectNumber)
	updateFromMap(d, "MESH_TENANT", &kr.MeshTenant)
	updateFromMap(d, "XDS_ADDR", &kr.XDSAddr)
	updateFromMap(d, "CLUSTER_NAME", &kr.ClusterName)
	updateFromMap(d, "CLUSTER_LOCATION", &kr.ClusterLocation)
	updateFromMap(d, "PROJECT_ID", &kr.ProjectId)
	updateFromMap(d, "MCON_ADDR", &kr.MeshConnectorAddr)
	updateFromMap(d, "IMCON_ADDR", &kr.MeshConnectorInternalAddr)
	updateFromMap(d, "CAROOT_ISTIOD", &kr.CitadelRoot)

	if kr.CitadelRoot != "" {
		kr.CARoots = append(kr.CARoots, kr.CitadelRoot)
	}
	return nil
}

func updateFromMap(d map[string]string, key string, dest *string) {
	if d[key] != "" && *dest == "" {
		*dest = d[key]
	}
}

func (kr *KRun) Config(name, def string) string {
	v := os.Getenv(name)
	if name == "" {
		return def
	}
	return v
}

func Is404(err error) bool {
	if se, ok := err.(*errors.StatusError); ok {
		if se.ErrStatus.Code == 404 {
			return true
		}
	}
	return false
}

// Signals handles the special signals.
//
// SIGTERM - send by docker on 'docker stop'.
// See https://cloud.google.com/blog/products/containers-kubernetes/kubernetes-best-practices-terminating-with-grace
func (kr *KRun) Signals() {
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		s := <-sigs
		log.Println("Received SIGINT", "total_time", time.Since(kr.StartTime))
		if kr.agentCmd != nil {
			kr.agentCmd.Process.Signal(s)
		}
		if kr.appCmd != nil {
			kr.appCmd.Process.Signal(s)
		}
	}()
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGTERM)
		s := <-sigs
		log.Println("Received SIGTERM", "total_time", time.Since(kr.StartTime))
		// Will start draining envoy
		if kr.agentCmd != nil {
			kr.agentCmd.Process.Signal(s)
		}
		if kr.appCmd != nil {
			kr.appCmd.Process.Signal(s)
		}
	}()

}
