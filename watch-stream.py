"""
This is a tiny mitmproxy script that enables the streaming mode [1] whenever a
request contains the query parameter ?watch=true. The watch=true parameter is
passed by Kubernertes clients (such as client-go) when they intend to be
notified of object updates.

Use with:
   mitmproxy -p 9090 -s watch-stream.py

[1]: https://docs.mitmproxy.org/stable/overview-features/#streaming

Note: mitmproxy crashes with "ConnectionInputs.RECV_RST_STREAM in state
ConnectionState.CLOSED" when vault tries to establish a connection with the
kube-apiserver. This issue seems to be a problem with "flow.request.stream =
True": https://github.com/mitmproxy/mitmproxy/issues/5897.
"""

from mitmproxy import ctx, http
import mitmproxy.coretypes.multidict


def responseheaders(flow: http.HTTPFlow):
    """
    Enables streaming for all requests that contain the query
    param?watch=true.
    """

    q: mitmproxy.coretypes.multidict.MultiDictView = flow.request.query
    ctx.log.info("query params: " + q.__str__())

    watch = ""
    try:
        watch = q["watch"]
    except KeyError as _:
        pass

    flow.response.stream = False
    if watch == "true":
        flow.response.stream = True
        ctx.log.info("streaming request")
