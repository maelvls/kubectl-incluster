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
	kubeconfig    = flag.String("kubeconfig", "", "")
	kubecontext   = flag.String("context", "", "")
	root          = flag.String("root", os.Getenv("CONTAINER_ROOT"), "The container root. You can also set CONTAINER_ROOT instead. If TELEPRESENCE_ROOT is set, it will default to that.")
	deprecated    = flag.Bool("embed", false, "Deprecated since this is now the default behavior. Embeds the token and ca.crt data inside the kubeconfig instead of using file paths.")
	replacecacert = flag.String("replace-cacert", "", "Instead of using the cacert provided in /var/run/secrets or in the kube config, use this one. Useful when using a proxy like mitmproxy.")
)

func main() {
	flag.Parse()

	if *deprecated {
		logutil.Infof("--embed is deprecated since it is now turned on by default")
	}

	// Defaults to TELEPRESENCE_ROOT only if --root is not passed.
	if os.Getenv("TELEPRESENCE_ROOT") != "" && *root == "" {
		*root = os.Getenv("TELEPRESENCE_ROOT")
	}

	c, err := RestConfig(*kubeconfig, *kubecontext, "kubectl-incluster")
	if err != nil {
		logutil.Errorf("loading: %s", err)
		os.Exit(1)
	}

	kubeconfig, err := kubeconfigFromRestConfig(c, *replacecacert)
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
// https://github.com/kubernetes/client-go/issues/711
func kubeconfigFromRestConfig(restconf *rest.Config, replaceCACertFile string) (*clientcmdapi.Config, error) {
	apiconf := clientcmdapi.NewConfig()

	apiconf.Clusters["kubectl-incluster"] = &clientcmdapi.Cluster{
		Server: restconf.Host,
	}

	apiconf.Clusters["kubectl-incluster"].CertificateAuthorityData = restconf.TLSClientConfig.CAData
	if replaceCACertFile != "" {
		restconf.TLSClientConfig.CAFile = replaceCACertFile
	}
	if restconf.TLSClientConfig.CAFile != "" {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		apiconf.Clusters["kubectl-incluster"].CertificateAuthorityData = bytes
	}

	apiconf.AuthInfos["kubectl-incluster"] = &clientcmdapi.AuthInfo{}

	apiconf.AuthInfos["kubectl-incluster"].ClientCertificateData = restconf.TLSClientConfig.CertData
	if restconf.TLSClientConfig.CertFile != "" {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.CertFile)
		if err != nil {
			return nil, fmt.Errorf("reading client certificate file: %w", err)
		}
		apiconf.AuthInfos["kubectl-incluster"].ClientCertificateData = bytes
	}

	apiconf.AuthInfos["kubectl-incluster"].ClientKeyData = restconf.TLSClientConfig.KeyData
	if restconf.TLSClientConfig.KeyFile != "" {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("reading client key file: %w", err)
		}
		apiconf.AuthInfos["kubectl-incluster"].ClientKeyData = bytes
	}

	apiconf.AuthInfos["kubectl-incluster"].Token = restconf.BearerToken
	if restconf.BearerTokenFile != "" {
		bytes, err := ioutil.ReadFile(restconf.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("reading token file: %w", err)
		}

		apiconf.AuthInfos["kubectl-incluster"].Token = string(bytes)
	}

	apiconf.CurrentContext = "kubectl-incluster"
	apiconf.Contexts["kubectl-incluster"] = clientcmdapi.NewContext()
	apiconf.Contexts["kubectl-incluster"].Cluster = "kubectl-incluster"
	apiconf.Contexts["kubectl-incluster"].AuthInfo = "kubectl-incluster"

	return apiconf, nil
}

// RestConfig creates a clientset by first trying to find the in-cluster config
// (i.e., in a Kubernetes pod). Otherwise, it loads the kube config from the
// given kubeconfig path. If the kubeconfig variable if left empty, the kube
// config will be loaded from $KUBECONFIG or by default ~/.kube/config.
//
// The context is useful for selecting which entry of the kube config you want
// to use. If context is left empty, the default context of the kube config is
// used.
//
// The userAgent can be for example "controller/v0.1.4/0848c95".
func RestConfig(kubeconfig, kubecontext, userAgent string) (*rest.Config, error) {
	var cfg *rest.Config
	var err error

	if kubeconfig != "" {
		logutil.Debugf("using you local kube config since --kubeconfig was passed")
		cfg, err = outClusterConfig(kubeconfig, kubecontext)
		if err != nil {
			return nil, fmt.Errorf("error loading kube config: %w", err)
		}
	}

	cfg, err = InClusterConfig()
	if err != nil {
		logutil.Debugf("in-cluster config was not found, now trying with your local kube config")
		cfg, err = outClusterConfig("", kubecontext)
		if err != nil {
			return nil, fmt.Errorf("error loading kube config: %w", err)
		}
	} else {
		logutil.Debugf("in-cluster config found")
	}

	cfg.UserAgent = userAgent

	return cfg, nil
}

func outClusterConfig(kubeconfig, kubecontext string) (*rest.Config, error) {
	loadRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadRules.ExplicitPath = kubeconfig

	apicfg, err := loadRules.Load()
	if err != nil {
		return nil, fmt.Errorf("error loading kubeconfig: %v", err)
	}

	return clientcmd.NewDefaultClientConfig(*apicfg, &clientcmd.ConfigOverrides{
		CurrentContext: kubecontext,
	}).ClientConfig()
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
