package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jaytaylor/go-hostsfile"
	authenticationv1 "k8s.io/api/authentication/v1"
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
	kubeconfig      = flag.String("kubeconfig", "", "Path to the kubeconfig file to use.")
	kubecontext     = flag.String("context", "", "The name of the kubeconfig context to use.")
	root            = flag.String("root", os.Getenv("CONTAINER_ROOT"), "The container root. You can also set CONTAINER_ROOT instead. If TELEPRESENCE_ROOT is set, it will default to that.")
	deprecated      = flag.Bool("embed", false, "Deprecated since this is now the default behavior. Embeds the token and ca.crt data inside the kubeconfig instead of using file paths.")
	replacecacert   = flag.String("replace-ca-cert", "", "Instead of using the cacert provided in /var/run/secrets or in the kube config, use this one. Useful when using a proxy like mitmproxy.")
	replacecacertD  = flag.String("replace-cacert", "", "Deprecated, please use --replace-ca-cert instead.")
	printClientCert = flag.Bool("print-client-cert", false, "Instead of printing the kube config, print the content of the kube config's client-certificate-data followed by the client-key-data.")
	printCACert     = flag.Bool("print-ca-cert", false, "Instead of printing a kubeconfig, print the content of the kube config's certificate-authority-data.")
	debug           = flag.Bool("d", false, "Print debug logs.")

	serviceaccount = flag.String("serviceaccount", "", strings.ReplaceAll(
		`Instead of using the current pod's /var/run/secrets (when in cluster)
		or the local kubeconfig (when out-of-cluster), you can use this flag to
		use the token and ca.crt from a given service account, for example
		'namespace-1/serviceaccount-1'. Useful when you want to force using a
		token (only available using service accounts) over client certificates
		provided in the kubeconfig, which is useful whenusing mitmproxy since
		the token is passed as a header (HTTP) instead of a client certificate
		(TLS).`, "\t", ""))
	sa = flag.String("sa", "", "Shorthand for --serviceaccount.")
)

func main() {
	flag.Parse()

	if *debug {
		logutil.EnableDebug = true
	}

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

	proxy := os.Getenv("HTTPS_PROXY")

	var proxyCACert string
	var err error
	if proxy != "" {
		proxyCACert, err = fetchCACertFromMitmproxy(proxy)
		if err != nil {
			logutil.Debugf("fetching the CA certificate from mitmproxy: %s", err)
		}
	}

	c, err := RestConfig(*kubeconfig, *kubecontext, "kubectl-incluster")
	if err != nil {
		logutil.Errorf("loading: %s", err)
		os.Exit(1)
	}

	// The flag --serviceaccount takes precedence over the --sa flag.
	if *sa != "" && *serviceaccount == "" {
		*serviceaccount = *sa
	}

	if *serviceaccount != "" {
		// We don't use the above 'c' because 'c' is meant to be customized (the
		// CA cert is changed, etc.). Here, we want the "unmodified" config so
		// that we can connect to the Kubernetes API.
		untouched, err := RestConfig(*kubeconfig, *kubecontext, "kubectl-incluster")
		if err != nil {
			logutil.Errorf("loading: %s", err)
			os.Exit(1)
		}

		// Chicken and egg: the whole purpose of kubectl incluster is to create
		// a kubeconfig that will work when used for MITM proxying over the HTTP
		// proxy protocol, i.e., when using HTTPS_PROXY and HTTP_PROXY. For
		// that, kubectl incluster needs to talk to the Kubernetes API, which is
		// impossible since HTTPS_PROXY is enabled but without the correct
		// adjustments to the kubeconfig. So we disable HTTPS_PROXY here.
		//
		// We can't just 'os.Unsetenv("HTTPS_PROXY")' because the default
		// http.Transport loads HTTPS_PROXY before this code runs.
		untouched.Proxy = func(r *http.Request) (*url.URL, error) {
			return nil, nil
		}

		token, err := getServiceAccount(untouched)
		if err != nil {
			logutil.Errorf("while processing flag --serviceaccount: %s", err)
			os.Exit(1)
		}

		c.BearerToken = token
		c.KeyData = nil
		c.KeyFile = ""
		c.CertData = nil
		c.CertFile = ""
	}

	if proxy != "" {
		// Now, let's check whether the proxy supports streaming. This check is
		// performed because mitmproxy doesn't stream reponses by default, which
		// blocks Kubernetes' watching mechanism.

		// Create a temporary server that listens on a random port.
		logutil.Debugf("creating a temporary server to test whether the proxy supports streaming")
		srv := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			logutil.Debugf("client connected, temporary server sending 'DONE'")
			w.Write([]byte("DONE"))

			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}

			// Pretend that the server is streaming data.
			time.Sleep(10 * time.Minute)
		}))
		l, err := net.Listen("tcp", ":0")
		if err != nil {
			logutil.Errorf("creating a temporary server: %s", err)
			os.Exit(1)
		}
		go func() {
			_ = http.Serve(l, srv)
		}()

		// Create a temporary client that connects to the temporary server.
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: func(r *http.Request) (*url.URL, error) {
					return url.Parse(proxy)
				},
			},
		}

		// The query parameter 'watch=true' is what I use in the mitmproxy
		// script to enable response streaming.
		req, err := http.NewRequest("GET", "http://"+l.Addr().String()+"?watch=true", nil)
		if err != nil {
			panic(err)
		}

		ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
		defer cancel()

		resp, err := client.Do(req.WithContext(ctx))
		operr := &net.OpError{}
		if errors.As(err, &operr) && operr.Op == "proxyconnect" {
			logutil.Errorf("the env var HTTPS_PROXY is set to %q, but the proxy doesn't seem to be running: %s", proxy, operr.Err)
			os.Exit(1)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			logutil.Errorf(strings.ReplaceAll(`the proxy does not supports streaming responses.
				If you are using mitmproxy, you can enable streaming by using a custom script with the flag '-s':
				    mitmproxy -s <(curl -L https://raw.githubusercontent.com/maelvls/kubectl-incluster/main/watch-stream.py)`, "\t", ""))
			os.Exit(1)
		}
		if err != nil {
			logutil.Errorf("checking whether proxy supports response streaming using a fake streaming server: %s", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			logutil.Errorf("checking whether proxy supports response streaming using a fake streaming server: the fake server returned a non-200 status code: %d", resp.StatusCode)
			os.Exit(1)
		}

		buf := make([]byte, 1024)
		for {
			d, err := resp.Body.Read(buf)
			logutil.Debugf("checking whether proxy supports response streaming: read %d bytes from the temporary server: %s", d, string(buf))
			if err == io.EOF {
				break
			}
			if err != nil {
				logutil.Errorf("checking whether proxy supports response streaming: while reading the response body: %s", err)
				os.Exit(1)
			}
			if bytes.Contains(buf, []byte("DONE")) {
				logutil.Debugf("the proxy supports streaming responses")
				break
			}
		}
		l.Close()
		resp.Body.Close()
	}

	// Go skips the HTTPS_PROXY env var if the host is a localhost address
	// (e.g., 127.0.0.1 or localhost). To work around that,
	if proxy != "" && (strings.Contains(c.Host, "localhost") || strings.Contains(c.Host, "127.0.0.1")) {
		// Let's figure out if we have an alias to 127.0.0.1 other than
		// "localhost" in /etc/hosts to work around the Go issue.

		addrs, err := hostsfile.ReverseLookup("127.0.0.1")
		if err != nil {
			logutil.Infof(strings.ReplaceAll(
				`while trying to figure out whether you will have a problem with
				Go ignoring HTTPS_PROXY when the host is "127.0.0.1" or "localhost",
				we encountered an error while reading /etc/hosts: %s.`, "\t", ""), err)
			os.Exit(1)
		}
		logutil.Debugf("aliases found for 127.0.0.1: %s", addrs)

		alias := ""
		for _, addr := range addrs {
			if addr != "localhost" {
				alias = addr
				break
			}
		}

		if alias == "" {
			logutil.Infof(strings.ReplaceAll(
				`no 127.0.0.1 alias found in /etc/hosts other than "localhost". If
				you run a Go program which tries to dial "127.0.0.1" or "localhost", Go
				will ignore the HTTPS_PROXY env var.
				
				To fix this issue, run the following command:
				    sudo tee -a /etc/hosts <<<"127.0.0.1 me"`, "\t", ""))
		}
		logutil.Debugf("using the alias '%s'", alias)

		c.Host = strings.ReplaceAll(c.Host, "localhost", alias)
		c.Host = strings.ReplaceAll(c.Host, "127.0.0.1", alias)
	}

	if proxyCACert != "" {
		c.TLSClientConfig.CAData = []byte(proxyCACert)
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
		kubeconfig, err := kubeconfigFromRestConfig(c, *replacecacert, proxyCACert)
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

func fetchCACertFromMitmproxy(proxy string) (pem string, _ error) {
	proxyURL, _ := url.Parse(proxy)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	resp, err := client.Get("http://mitm.it/cert/pem")
	if err != nil {
		return "", fmt.Errorf("while trying to fetch the CA cert at GET mitm.it/cert/pem: %s", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "application/x-x509-ca-cert" {
		logutil.Errorf("unexpected content type of GET mitm.it/cert/pem: %s", resp.Header.Get("Content-Type"))
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("while reading the body of GET mitm.it/cert/pem: %s", err)
	}

	return string(body), nil
}

func getServiceAccount(c *rest.Config) (token string, _ error) {
	splits := strings.Split(*serviceaccount, "/")
	if len(splits) != 2 {
		return "", fmt.Errorf("--serviceaccount: expected value of the form 'namespace/serviceaccount', got: %s", *serviceaccount)
	}

	namespace := splits[0]
	name := splits[1]

	cl, err := kubernetes.NewForConfig(c)
	if err != nil {
		return "", fmt.Errorf("while processing flag --serviceaccount: creating Kubernetes client: %s", err)
	}

	serviceaccount, err := cl.CoreV1().ServiceAccounts(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("getting serviceaccount %s in namespace %s: %v", name, namespace, err)
	}

	// By default, we try to use the default service account token. Since
	// Kubernetes 1.20, the default service account token is not created, so we
	// try to generate a token instead.
	if len(serviceaccount.Secrets) < 1 {
		logutil.Debugf("serviceaccount %s has no default service account secret, now trying to generate a token", serviceaccount.GetName())
		token, err := cl.CoreV1().ServiceAccounts(namespace).CreateToken(context.TODO(), name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to generate a token for serviceaccount %s in namespace %s: %v", name, namespace, err)
		}
		return token.Status.Token, nil
	}

	var secret *v1.Secret
	for _, secretRef := range serviceaccount.Secrets {
		secret, err = cl.CoreV1().Secrets(namespace).Get(context.TODO(), secretRef.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("failed to get the secret %s in namespace %s: %v", secretRef.Name, namespace, err)
		}

		if secret.Type == v1.SecretTypeServiceAccountToken {
			break
		}
	}

	if secret == nil {
		return "", fmt.Errorf("serviceaccount %s has no secret type %s", name, v1.SecretTypeServiceAccountToken)
	}

	tokenBytes, ok := secret.Data["token"]
	if !ok {
		return "", fmt.Errorf("key 'token' not found in %s", secret.GetName())
	}

	return string(tokenBytes), nil
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
func kubeconfigFromRestConfig(restconf *rest.Config, replaceCACertFile, replaceCAData string) (*clientcmdapi.Config, error) {
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

	if kubecontext == "" && apicfg.CurrentContext == "" {
		return nil, fmt.Errorf("no context was provided and no current context was found in the kubeconfig")
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
