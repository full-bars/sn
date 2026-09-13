from rpc import *
import json
import os
_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", "..", ".."))


ABIDIR=os.path.join(_REPO,"evm","abi")+os.sep
def load(n): return json.load(open(ABIDIR+n+".abi.json"))
EVENTS={}
def sig(e):
    return e["name"]+"("+",".join(i["type"] for i in e["inputs"])+")"
allev=[]
for n in ["STCoordinator","STSettlementVault","STReserveSink"]:
    for e in load(n):
        if e.get("type")=="event": allev.append((n,e))
topics = batch([("web3_sha3",["0x"+sig(e).encode().hex()]) for (_n,e) in allev])
for (n,e),t in zip(allev,topics):
    EVENTS[t.lower()]=(n,e)
json.dump({k:(v[0],v[1]["name"],sig(v[1])) for k,v in EVENTS.items()}, open("topics.json","w"), indent=1)

def dec_word(w):  # 32-byte hex chunk -> int
    return int(w,16)
def decode_log(log):
    t0=log["topics"][0].lower()
    if t0 not in EVENTS: return None
    name_c, e = EVENTS[t0]
    idx=[i for i in e["inputs"] if i.get("indexed")]
    non=[i for i in e["inputs"] if not i.get("indexed")]
    out={"_event":e["name"],"_contract":name_c}
    for i,inp in enumerate(idx):
        raw=log["topics"][i+1]
        if inp["type"]=="address": out[inp["name"]]="0x"+raw[-40:]
        elif inp["type"].startswith(("uint","int")): out[inp["name"]]=int(raw,16)
        elif inp["type"]=="bool": out[inp["name"]]=bool(int(raw,16))
        else: out[inp["name"]]=raw
    data=log["data"][2:]
    words=[data[i*64:(i+1)*64] for i in range(len(data)//64)]
    for i,inp in enumerate(non):
        if i>=len(words): break
        w=words[i]
        if inp["type"]=="address": out[inp["name"]]="0x"+w[-40:]
        elif inp["type"].startswith(("uint","int")): out[inp["name"]]=int(w,16)
        elif inp["type"]=="bool": out[inp["name"]]=bool(int(w,16))
        elif inp["type"].startswith("bytes") and inp["type"]!="bytes": out[inp["name"]]="0x"+w
        else: out[inp["name"]]="0x"+w
    return out
