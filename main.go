package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog"

	"github.com/maelvls/kubectl-incluster/logutil"
)

var (
	kubeconfig  = flag.String("kubeconfig", "", "")
	kubecontext = flag.String("context", "", "")
	root        = flag.String("root", os.Getenv("CONTAINER_ROOT"), "The container root. You can also set CONTAINER_ROOT instead.")
	embed       = flag.Bool("embed", false, "Embed the token and ca.crt data inside the kubeconfig instead of using file paths.")
)

func main() {
	flag.Parse()

	c, err := RestConfig(*kubeconfig, *kubecontext, "kubectl-incluster")
	if err != nil {
		logutil.Errorf("loading: %s", err)
		os.Exit(1)
	}

	kubeconfig, err := kubeconfigFromRestConfig(c, *embed)
	if err != nil {
		logutil.Errorf("building the kubeconfig: %s", err)
		os.Exit(1)
	}

	err = clientcmd.WriteToFile(*kubeconfig, "/dev/stdout")
	if err != nil {
		logutil.Errorf("writing: %s", err)
		os.Exit(1)
	}
}

// When embed is true, the ca certificate and token are embedded in the
// kube config as a base64 string. Otherwise, the paths to the token and to
// the ca file are used in the kube config.
func kubeconfigFromRestConfig(restconf *rest.Config, embed bool) (*clientcmdapi.Config, error) {
	apiconf := clientcmdapi.NewConfig()

	apiconf.Clusters["foo"] = &clientcmdapi.Cluster{
		Server: restconf.Host,
	}
	if embed {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca.crt: %w", err)
		}
		apiconf.Clusters["foo"].CertificateAuthorityData = bytes
	} else {
		apiconf.Clusters["foo"].CertificateAuthority = restconf.TLSClientConfig.CAFile
	}

	apiconf.AuthInfos["foo"] = &clientcmdapi.AuthInfo{}
	if embed {
		apiconf.AuthInfos["foo"].Token = restconf.BearerToken
	} else {
		apiconf.AuthInfos["foo"].TokenFile = restconf.BearerTokenFile
	}

	return apiconf, nil
}

// RestConfig creates a clientset by first trying to find the in-cluster
// config (i.e., in a Kubernetes pod). Otherwise, it loads the kube config
// from the given kubeconfig path. If the kubeconfig variable if left
// empty, the kube config will be loaded from $KUBECONFIG or by default
// ~/.kube/config.
//
// The context is useful for selecting which entry of the kube config you
// want to use. If context is left empty, the default context of the kube
// config is used.
//
// The userAgent can be for example "multi-tenancy-controllers/v0.1.4/0848c95".
//
// NOTE(Matt): this piece of code is left untested (bad, bad!) but I have
// two excuses: (1) these code paths are going to be run over and over by
// every single integration test (2) there is not much logic (what a poor
// excuse, right?)
func RestConfig(kubeconfig, kubecontext, userAgent string) (*rest.Config, error) {
	cfg, err := InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config was not found")
	}
	cfg.UserAgent = userAgent
	return cfg, nil
}

// InClusterConfig is the vendored version of rest.InClusterConfig:
// https://github.com/kubernetes/client-go/blob/fb61a7c/rest/config.go
func InClusterConfig() (*rest.Config, error) {
	var (
		tokenFile  = *root + "/var/run/secrets/kubernetes.io/serviceaccount/token"
		rootCAFile = *root + "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		return nil, rest.ErrNotInCluster
	}

	token, err := ioutil.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}

	tlsClientConfig := rest.TLSClientConfig{}

	if _, err := certutil.NewPool(rootCAFile); err != nil {
		klog.Errorf("Expected to load root CA config from %s, but got err: %v", rootCAFile, err)
	} else {
		tlsClientConfig.CAFile = rootCAFile
	}

	return &rest.Config{
		// TODO: switch to using cluster DNS.
		Host:            "https://" + net.JoinHostPort(host, port),
		TLSClientConfig: tlsClientConfig,
		BearerToken:     string(token),
		BearerTokenFile: tokenFile,
	}, nil
}
