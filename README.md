# kubectl-incluster

I wrote this kubectl plugin in order to create a kubeconfig file out of an
in-cluster configuration, i.e., using the mounted service account token and
CA cert:

```sh
/var/run/secrets/kubernetes.io/serviceaccount/token
/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
```

Running `kubectl incluster` from inside a running pod will print a working
kubeconfig that you can use somewhere else.

**Content:**

- [Use-case: telepresence + mitmproxy for debugging cert-manager](#use-case-telepresence--mitmproxy-for-debugging-cert-manager)
- [Use-case: telepresence + mitmproxy for debugging the preflight agent](#use-case-telepresence--mitmproxy-for-debugging-the-preflight-agent)
- [Use-case: mitmproxy inside the cluster (as opposed to using telepresence)](#use-case-mitmproxy-inside-the-cluster-as-opposed-to-using-telepresence)
- [Use-case: mitmproxy without kubectl-incluster](#use-case-mitmproxy-without-kubectl-incluster)

## Use-case: telepresence + mitmproxy for debugging cert-manager

In order to inspect the egress traffic coming from a Kubernetes controller
(here, [cert-manager](https://github.com/jetstack/cert-manager)), I want to
be able to use mitmproxy through a Telepresence `--run-shell` session. For
example, let's imagine you have a cert-manager deployment already running
and that you want to see what requests it makes.

Let's first start mitmproxy. We use [watch-stream.py](/watch-stream.py), a
script that makes sure the streaming GET requests are properly streamed by
mitmproxy:

```sh
curl -L https://raw.githubusercontent.com/maelvls/kubectl-incluster/main/watch-stream.py >/tmp/watch-stream.py
mitmproxy -p 9090 --ssl-insecure -s /tmp/watch-stream.py --set client_certs=<(kubectl incluster --print-client-cert)
```

> Note that we could avoid using `--ssl-insecure` by replacing it with
> something like:
>
> ```sh
> mitmproxy -p 9090 --set ssl_verify_upstream_trusted_ca=<(kubectl incluster --print-ca-cert)
> ```
>
> But since I don't run mitmproxy from inside the Telepresence shell, I
> don't have access to the `$TELEPRESENCE_ROOT` variable. So I don't bother
> and use `--ssl-insecure` instead.

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
T: Forwarding remote port 9402 to local port 9402.
T: Connected. Flushing DNS cache.
T: Setup complete. Launching your command.

@boring_wozniak|bash-3.2$
```

Now, from this shell, let us run cert-manager:

```sh
HTTPS_PROXY=localhost:9090 go run ./cmd/controller --leader-elect=false --kubeconfig <(kubectl incluster --root $TELEPRESENCE_ROOT --replace-ca-cert ~/.mitmproxy/mitmproxy-ca.pem) -v=4
```

And TADA! We see all the requests made by our controller:

<img alt="An mitmproxy screenshot when debugging cert-manager-controller. Screenshot stored in the issue https://github.com/maelvls/kubectl-incluster/issues/1" src="https://user-images.githubusercontent.com/2195781/100645025-64f89880-333c-11eb-9a3f-b6aa8cde497d.png">

## Use-case: telepresence + mitmproxy for debugging the preflight agent

The preflight agent is a binary that runs in your Kubernetes cluster and
reports information about certificates to the
<https://platform.jetstack.io> dashboard. The free tier allows you to see
if any of your certificates managed by cert-manager has an issue.

To debug the agent, the first step is to have the agent built:

```sh
git clone https://github.com/jetstack/preflight
cd preflight
make install
```

Then, you want to run telepresence:

```sh
telepresence --namespace jetstack-secure --swap-deployment agent --run-shell
```

Run the mitmproxy instance:

```sh
# In another shell, not in the telepresence shell.
mitmproxy -p 9090 --ssl-insecure --set client_certs=<(kubectl incluster --print-client-cert)
```

Finally you can run the agent:

> **🔰 Tip:** to know which command-line arguments are used by a given deployment,
> you can use `kubectl-args` that extracts the `args` for the deployment.
> Imagining that you have `~/bin` in your PATH, you can install it with:
>
> ```sh
> cat <<'EOF' > /tmp/kubectl-args
> #! /bin/bash
> set -e -o pipefail
> kubectl get deploy -ojsonpath='{.spec.template.spec.containers[0].args}' "$@" | jq -r '.[]' | awk '{if($2 != ""){print "\"" $0 "\""}else{print $0}}' |  tr '\n' ' '; echo
> EOF
> install /tmp/kubectl-args ~/bin
> ```
>
> Then, use it with:
>
> ```sh
> % kubectl args -n jetstack-secure agent
> agent -c /etc/jetstack-secure/agent/config/config.yaml -k /etc/jetstack-secure/agent/credentials/credentials.json -p 0h1m0s
> ```

```sh
# Inside the telepresence shell.
HTTPS_PROXY=127.0.0.1:9090 KUBECONFIG=$(kubectl incluster --root $TELEPRESENCE_ROOT --replace-ca-cert ~/.mitmproxy/mitmproxy-ca.pem >/tmp/foo && echo /tmp/foo) preflight agent -c $TELEPRESENCE_ROOT/etc/jetstack-secure/agent/config/config.yaml -k $TELEPRESENCE_ROOT/etc/jetstack-secure/agent/credentials/credentials.json -p 0h1m0s
```

You will see:

<img alt="An mitmproxy screenshot when debugging the preflight agent that reports to https://platform.jetstack.io. Screenshot stored in the issue https://github.com/maelvls/kubectl-incluster/issues/1" src="https://user-images.githubusercontent.com/2195781/110499573-aa292500-80f8-11eb-8570-c90b56475f27.png">

## Use-case: mitmproxy inside the cluster (as opposed to using telepresence)

First, we need to have an instance of mitmproxy running:

```sh
kubectl apply -n jetstack-secure -f <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mitmproxy
  labels:
    app: mitmproxy
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mitmproxy
  template:
    metadata:
      labels:
        app: mitmproxy
    spec:
      containers:
        - name: mitmproxy
          image: mitmproxy/mitmproxy:latest
          args: [mitmweb, -p, "9090"]
          imagePullPolicy: Always
          ports:
            - containerPort: 8081
              name: ui
            - containerPort: 9090
              name: proxy
          resources:
            limits:
              memory: "460Mi"
              cpu: "200m"
---
kind: Service
apiVersion: v1
metadata:
  name: mitmproxy
spec:
  ports:
    - name: ui
      port: 8081
    - name: proxy
      port: 9090
  selector:
    app: mitmproxy
EOF
```

Then, let us see the mitmweb UI:

```sh
kubectl port-forward -n jetstack-secure $(kubectl get pod -n jetstack-secure -l app.kubernetes.io/name=agent -oname) 8081:8081
```

and head to <http://localhost:8081>.

Then, we need to add that to the running deployment that we want to debug:

```sh
kubectl edit deploy your-deployment
```

and add the following to the container's `env`:

```yaml
spec:
  containers:
    - env:
        - name: HTTPS_PROXY
          value: http://mitmproxy:9090
```

⚠️ IMPORTANT ⚠️ : you also have to make sure the container's binary can
disable TLS verification. Otherwise, no way to do that...

## Use-case: mitmproxy without kubectl-incluster

Let us imagine we want to trace what `kubectl get pods` is doing under the
hood.

First, let us work around the fact that Go binaries do not honor the
`HTTPS_PROXY` variable for the `127.0.0.1` and `localhost` domains. Instead
of `127.0.0.1`, we will use the domain `me`:

```sh
grep "127.0.0.1[ ]*me$" /etc/hosts || sudo tee --append /etc/hosts <<<"127.0.0.1 me"
```

Then, let us make sure our system trusts Mitmproxy's root CA:

```sh
# Linux
sudo mkdir -p /usr/share/ca-certificates/mitmproxy
sudo cp ~/.mitmproxy/mitmproxy-ca-cert.pem /usr/share/ca-certificates/mitmproxy/mitmproxy-ca-cert.crt
grep mitmproxy/mitmproxy-ca-cert.crt /etc/ca-certificates.conf \
  || sudo tee --append /etc/ca-certificates.conf <<<mitmproxy/mitmproxy-ca-cert.crt
sudo update-ca-certificates

# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.mitmproxy/mitmproxy-ca-cert.pem
```

Let us start `mitmproxy`. We have to use `--ssl-insecure` due to the fact that
we don't want to bother having `mitmproxy` to trust the apiserver. We need to
give the correct client certificate to the proxy (if you are using a client
certificate):

```sh
curl -L https://raw.githubusercontent.com/maelvls/kubectl-incluster/main/watch-stream.py >/tmp/watch-stream.py
kubectl config view --minify --flatten -o=go-template='{{(index ((index .users 0).user) "client-key-data")}}' | base64 -d >/tmp/client.pem
kubectl config view --minify --flatten -o=go-template='{{(index ((index .users 0).user) "client-certificate-data")}}' | base64 -d >>/tmp/client.pem
mitmproxy -p 9090 --ssl-insecure --set client_certs=/tmp/client.pem -s /tmp/watch-stream.py
```

Finally, let us run the command we want to HTTP-inspect:

```sh
HTTPS_PROXY=:9090 KUBECONFIG=<(kubectl config view --minify --flatten \
    | sed "s|certificate-authority-data:.*|certificate-authority-data: $(base64 -w0 < ~/.mitmproxy/mitmproxy-ca-cert.pem)|g" \
    | sed "s|127.0.0.1|me|") \
  kubectl get pods
```

> 🔰 The command `kubectl config view --minify` prints the kube config for
> the current context, which comes in very handy here.
