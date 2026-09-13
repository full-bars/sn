"""Second endpoint, for confirming a result on a node other than the primary.

Set $SN_RPC_URL_2 to the node to cross-check against. The default is the owners'
LAN node, so that a reviewer running with the public endpoint as primary
reproduces the published two-endpoint comparison in the opposite direction.
"""
import json, os, urllib.request

N2 = os.environ.get("SN_RPC_URL_2", "http://192.168.1.162:9944")

def rpc2(method, params=None, timeout=45):
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method,
                       "params": params or []}).encode()
    req = urllib.request.Request(N2, data=body, headers={"content-type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        d = json.load(r)
    if "error" in d:
        raise RuntimeError(f"{method}: {d['error']}")
    return d["result"]
