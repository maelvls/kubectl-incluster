package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog"

	"github.com/maelvls/kubectl-incluster/logutil"
)

var (
	kubeconfig      = flag.String("kubeconfig", "", "")
	kubecontext     = flag.String("context", "", "")
	root            = flag.String("root", os.Getenv("CONTAINER_ROOT"), "The container root. You can also set CONTAINER_ROOT instead. If TELEPRESENCE_ROOT is set, it will default to that.")
	deprecated      = flag.Bool("embed", false, "Deprecated since this is now the default behavior. Embeds the token and ca.crt data inside the kubeconfig instead of using file paths.")
	replacecacert   = flag.String("replace-ca-cert", "", "Instead of using the cacert provided in /var/run/secrets or in the kube config, use this one. Useful when using a proxy like mitmproxy.")
	replacecacertD  = flag.String("replace-cacert", "", "Deprecated, please use --replace-ca-cert instead.")
	printClientCert = flag.Bool("print-client-cert", false, "Instead of printing the kube config, print the content of the kube config's client-certificate-data followed by the client-key-data.")
	printCACert     = flag.Bool("print-ca-cert", false, "Instead of printing a kubeconfig, print the content of the kube config's certificate-authority-data.")

	serviceaccount = flag.String("serviceaccount", "", strings.ReplaceAll(
		`Instead of using the current pod's /var/run/secrets (when in cluster)
		or the local kubeconfig (when out-of-cluster), you can use this flag to
		use the token and ca.crt from a given service account, for example
		'namespace-1/serviceaccount-1'. Useful when you want to force using a
		token (only available using service accounts) over client certificates
		provided in the kubeconfig, which is useful whenusing mitmproxy since
		the token is passed as a header (HTTP) instead of a client certificate
		(TLS).`, "\t", ""))
)

func main() {
	flag.Parse()

	if *deprecated {
		logutil.Infof("--embed is deprecated since it is now turned on by default")
	}

	if *replacecacertD != "" {
		logutil.Infof("--replace-cacert is deprecated, please use --replace-ca-cert instead")
		*replacecacert = *replacecacertD
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

	if *serviceaccount != "" {
		cacrt, token, err := getServiceAccount(c)
		if err != nil {
			logutil.Errorf("while processing flag --serviceaccount: %s", err)
			os.Exit(1)
		}

		c.TLSClientConfig.CAData = []byte(cacrt)
		c.BearerToken = token
		c.KeyData = nil
		c.KeyFile = ""
		c.CertData = nil
		c.CertFile = ""
	}

	switch {
	case *printClientCert:
		pem, err := clientCertPEMFromRestConfig(c)
		if err != nil {
			logutil.Errorf("building the PEM bundle with the client-certificate-data and client-key-data: %s", err)
			os.Exit(1)
		}
		fmt.Printf("%s", pem)
	case *printCACert:
		pem, err := caCertPEMFromRestConfig(c)
		if err != nil {
			logutil.Errorf("building the PEM bundle with the ca-certificate-data: %s", err)
			os.Exit(1)
		}
		fmt.Printf("%s", pem)
	default:
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
}

func getServiceAccount(c *rest.Config) (cacrt []byte, token string, _ error) {
	splits := strings.Split(*serviceaccount, "/")
	if len(splits) != 2 {
		return nil, "", fmt.Errorf("--serviceaccount: expected value of the form 'namespace/serviceaccount', got: %s", *serviceaccount)
	}

	namespace := splits[0]
	name := splits[1]

	cl, err := kubernetes.NewForConfig(c)
	if err != nil {
		return nil, "", fmt.Errorf("while processing flag --serviceaccount: creating Kubernetes client: %s", err)
	}

	serviceaccount, err := cl.CoreV1().ServiceAccounts(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("getting serviceaccount %s in namespace %s: %v", name, namespace, err)
	}

	if len(serviceaccount.Secrets) < 1 {
		return nil, "", fmt.Errorf("serviceaccount %s has no secrets", serviceaccount.GetName())
	}

	var secret *v1.Secret
	for _, secretRef := range serviceaccount.Secrets {
		secret, err = cl.CoreV1().Secrets(namespace).Get(context.TODO(), secretRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, "", fmt.Errorf("failed to get the secret %s in namespace %s: %v", secretRef.Name, namespace, err)
		}

		if secret.Type == v1.SecretTypeServiceAccountToken {
			break
		}
	}

	if secret == nil {
		return nil, "", fmt.Errorf("serviceaccount %s has no secret type %s", name, v1.SecretTypeServiceAccountToken)
	}

	var ok bool
	cacrt, ok = secret.Data["ca.crt"]
	if !ok {
		return nil, "", fmt.Errorf("key 'ca.crt' not found in %s", secret.GetName())
	}

	tokenBytes, ok := secret.Data["token"]
	if !ok {
		return nil, "", fmt.Errorf("key 'token' not found in %s", secret.GetName())
	}

	return cacrt, string(tokenBytes), nil
}

// The PEM-encoded private key is displayed first.
func clientCertPEMFromRestConfig(restconf *rest.Config) ([]byte, error) {
	var clientPEM []byte

	if restconf.TLSClientConfig.KeyFile != "" {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("reading client key file: %w", err)
		}

		clientPEM = append(clientPEM, bytes...)
	} else if len(restconf.TLSClientConfig.KeyData) > 0 {
		clientPEM = append(clientPEM, restconf.TLSClientConfig.KeyData...)
	} else if restconf.BearerTokenFile != "" {
		return nil, fmt.Errorf("cannot produce a PEM client certificate bundle when the kube config uses a token")
	}

	if len(restconf.TLSClientConfig.CertData) > 0 {
		clientPEM = append(clientPEM, restconf.TLSClientConfig.CertData...)
	} else if restconf.TLSClientConfig.CertFile != "" {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.CertFile)
		if err != nil {
			return nil, fmt.Errorf("reading client certificate file: %w", err)
		}

		clientPEM = append(clientPEM, bytes...)
	}

	return clientPEM, nil
}

func caCertPEMFromRestConfig(restconf *rest.Config) ([]byte, error) {
	if len(restconf.TLSClientConfig.CAData) > 0 {
		return restconf.TLSClientConfig.CAData, nil
	} else if restconf.TLSClientConfig.CAFile != "" {
		bytes, err := ioutil.ReadFile(restconf.TLSClientConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading client certificate file: %w", err)
		}

		return bytes, nil
	}

	return nil, fmt.Errorf("no ca-certificate-data nor ca-certificate-file")
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
