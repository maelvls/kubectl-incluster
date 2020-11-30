## kubectl-incluster

I wrote this kubectl plugin in order to create a kubeconfig file out of an
in-cluster configuration, i.e., using the mounted service account token and
CA cert:

```sh
/var/run/secrets/kubernetes.io/serviceaccount/token
/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
```

Running `kubectl incluster` from inside a running pod will print a working
kubeconfig that you can use somewhere else.

## Usecase: telepresence + mitmproxy

I want to be able to use mitmproxy through a Telepresence `--run-shell`
session. For example, let's imagine you have a cert-manager deployment
already running and that you want to see what requests it makes.

Let's first start mitmproxy:

```sh
mitmproxy -p 9090 --ssl-insecure
```

> Note that we could avoid using `--ssl-insecure` by replacing it with
> something like:
>
> ```sh
> mitmproxy -p 9090 --set ssl_verify_upstream_trusted_ca=$TELEPRESENCE_ROOT/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
> ```
>
> But since I don't run mitmproxy from inside the Telepresence shell, I
> don't have access to the `$TELEPRESENCE_ROOT` variable. So I just don't
> bother and use `--ssl-insecure` instead.

Let us now install `kubectl-incluser`:

```sh
(cd && GO111MODULE=on go get github.com/maelvls/kubectl-incluster@latest)
```

> Note: the reason I use the `(cd && ...)` notation is detailed in [this
> blog
> post](https://maelvls.dev/go111module-everywhere/#the-pitfall-of-gomod-being-silently-updated).

Now, let's run cert-manager locally inside a Telepresence shell:

```sh
% git clone https://github.com/jetstack/cert-manager && cd cert-manager
% telepresence --namespace cert-manager --swap-deployment cert-manager --run-shell
T: Using a Pod instead of a Deployment for the Telepresence proxy. If you experience problems, please file an issue!
T: Set the environment variable TELEPRESENCE_USE_DEPLOYMENT to any non-empty value to force the old behavior, e.g.,
T:     env TELEPRESENCE_USE_DEPLOYMENT=1 telepresence --run curl hello
T: Starting proxy with method 'vpn-tcp', which has the following limitations: All processes are affected, only one telepresence can run per machine, and you can't use
T: other VPNs. You may need to add cloud hosts and headless services with --also-proxy. For a full list of method limitations see
T: https://telepresence.io/reference/methods.html
T: Volumes are rooted at $TELEPRESENCE_ROOT. See https://telepresence.io/howto/volumes.html for details.
T: Starting network proxy to cluster by swapping out Deployment cert-manager with a proxy Pod
T: Forwarding remote port 9402 to local port 9402.
T: Connected. Flushing DNS cache.
T: Setup complete. Launching your command.

The default interactive shell is now zsh.
To update your account to use zsh, please run `chsh -s /bin/zsh`.
For more details, please visit https://support.apple.com/kb/HT208050.
@boring_wozniak|bash-3.2$
```

Now, from this shell, let us run cert-manager:

```sh
HTTPS_PROXY=localhost:9090 go run ./cmd/controller --leader-elect=false --kubeconfig <(kubectl incluster --root $TELEPRESENCE_ROOT --replace-cacert ~/.mitmproxy/mitmproxy-ca.pem)
```

And TADA! We see all the requests made by our controller:

![mitmproxy-screenshot](https://user-images.githubusercontent.com/2195781/100598017-758a1e00-32fe-11eb-8cda-da7048b8e709.png)
