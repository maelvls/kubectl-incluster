"""
This custom pretty-printer for mitmproxy will decode the base64url-encoded
'payload' and 'protected' fields. This pretty-printer is useful for
understanding the POST requests made to an ACME server, since these requests are
made using JWS as JSON, which, contrary to JWT tokens that are very easy to
decode, aren't as common. The whole JWS as JSON and application/jose+json are
detailed in https://tools.ietf.org/html/rfc8555. To use this pretty-printer, run
mitmproxy with

  mitmproxy -s josejson.py

For example, the following body of an HTTP request:

  {
    "protected": "eyJhbGciOiJSUzI1Ni...E1MzY1Mjg1MCJ9",
    "payload": "eyJjc3IiOiJNSUlDblRDQ0FZ...EU3lHQ3BjLTlfanVBIn0",
    "signature": "qqYGqZDSSUwuLLxm6-...nygkb5S8igKPrw"
  }

will be displayed as:

  {
      "payload": {
          "csr": "MIICnTCCAYUCAQAwADCCAS...duroowkXh3tqgVFDSyGCpc-9_juA"
      },
      "protected": {
          "alg": "RS256",
          "kid": "https://acme-v02.api.letsencrypt.org/acme/acct/204416270",
          "nonce": "01017mM9r6R_TpKL-5zxAmMF5JmTCBI-v6AsLlGedj3pD1E",
          "url": "https://acme-v02.api.letsencrypt.org/acme/finalize/204416270/25153652850"
      },
      "signature": "qqYGqZDSSUwuLLxm6-...nygkb5S8igKPrw"
  }

Note that only the 'payload' and 'protected' fields are base64-decoded inline.
The signature is not decoded.
"""

from mitmproxy import contentviews, ctx, flow, http
from mitmproxy.contentviews import base
import json
import base64
import typing
import re


def format_json(data: typing.Any) -> typing.Iterator[base.TViewLine]:
    encoder = json.JSONEncoder(indent=4, sort_keys=True, ensure_ascii=False)
    current_line: base.TViewLine = []
    for chunk in encoder.iterencode(data):
        if "\n" in chunk:
            rest_of_last_line, chunk = chunk.split("\n", maxsplit=1)
            # rest_of_last_line is a delimiter such as , or [
            current_line.append(("text", rest_of_last_line))
            yield current_line
            current_line = []
        if re.match(r'\s*"', chunk):
            current_line.append(("json_string", chunk))
        elif re.match(r"\s*\d", chunk):
            current_line.append(("json_number", chunk))
        elif re.match(r"\s*(true|null|false)", chunk):
            current_line.append(("json_boolean", chunk))
        else:
            current_line.append(("text", chunk))
    yield current_line


class ViewJoseJson(contentviews.Contentview):
    def prettify(
        self,
        data: bytes,
        metadata: contentviews.Metadata,
    ) -> str:
        data = json.loads(data.decode("utf-8"))
        decoded_data = josejson(data)
        return json.dumps(decoded_data, indent=4, sort_keys=True, ensure_ascii=False)

    def render_priority(self, data: bytes, metadata: contentviews.Metadata) -> float:
        if metadata.content_type and metadata.content_type.startswith(
            "application/jose+json"
        ):
            return 2
        else:
            return 0


def josejson(data: typing.Any) -> typing.Any:
    if len(data["protected"]) == 0:
        data["protected"] = "e30"  # '{}' in base64

    # https://stackoverflow.com/questions/2941995/python-ignore-incorrect-padding-error-when-base64-decoding
    data["protected"] += "==="

    try:
        data["protected"] = json.loads(base64.urlsafe_b64decode(data["protected"]))
    except Exception as e:
        ctx.log.info(e)

    if len(data["payload"]) == 0:
        data["payload"] = "e30"  # '{}' in base64

    # https://stackoverflow.com/questions/2941995/python-ignore-incorrect-padding-error-when-base64-decoding
    data["payload"] += "==="

    try:
        data["payload"] = json.loads(base64.urlsafe_b64decode(data["payload"]))
    except Exception as e:
        ctx.log.info(e)

    return data


view = ViewJoseJson()


def load(l):
    contentviews.add(view)


def done():
    contentviews.remove(view)
