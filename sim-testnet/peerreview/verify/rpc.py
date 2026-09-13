"""JSON-RPC helper for the sim-testnet peer-review verification.

Endpoint selection, in order of precedence:
  1. $SN_RPC_URL                     - any chain-945 archive node
  2. https://test.finney.opentensor.ai  (default; Opentensor-operated, independent
     of the subnet owners, and the endpoint the published findings were confirmed on)

The owners' own node used for the first pass was http://192.168.1.162:9944; it is
reachable only from that LAN. Every check in this directory is endpoint-agnostic:
it must produce identical results on any archive node serving chain 945.

An archive node is required. Several checks read contract state and Substrate
storage at historical blocks (7,986,181 - 7,992,355); a pruned node returns
"State already discarded" for those.
"""
import json, os, urllib.request

N = os.environ.get("SN_RPC_URL", "https://test.finney.opentensor.ai")
TIMEOUT = float(os.environ.get("SN_RPC_TIMEOUT", "45"))
_id = [0]

def rpc(method, params=None):
    _id[0] += 1
    body = json.dumps({"jsonrpc": "2.0", "id": _id[0], "method": method,
                       "params": params or []}).encode()
    req = urllib.request.Request(N, data=body, headers={"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
        d = json.load(r)
    if "error" in d:
        raise RuntimeError(f"{method} {params}: {d['error']}")
    return d["result"]

def batch(calls, chunk=None):
    """calls = [(method, params), ...] -> results in the same order.

    Requests are chunked: public endpoints cap JSON-RPC batch size and answer an
    oversized batch with a single error object rather than an array. Override the
    chunk size with $SN_RPC_BATCH.
    """
    chunk = chunk or int(os.environ.get("SN_RPC_BATCH", "32"))
    out = []
    for off in range(0, len(calls), chunk):
        part = calls[off:off + chunk]
        body = [{"jsonrpc": "2.0", "id": i, "method": m, "params": p or []}
                for i, (m, p) in enumerate(part)]
        req = urllib.request.Request(N, data=json.dumps(body).encode(),
                                     headers={"content-type": "application/json"})
        with urllib.request.urlopen(req, timeout=TIMEOUT * 2) as r:
            d = json.load(r)
        if not isinstance(d, list):
            raise RuntimeError(
                f"endpoint {N} refused a batch of {len(part)}: {d.get('error', d)}. "
                f"Lower it with SN_RPC_BATCH.")
        d.sort(key=lambda x: x["id"])
        out.extend(x.get("result", {"__error__": x.get("error")}) for x in d)
    return out

def h2i(x):
    return int(x, 16) if isinstance(x, str) else x
