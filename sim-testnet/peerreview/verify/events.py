import os
_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", "..", ".."))
#!/usr/bin/env python3
import json,sys,os
sys.path.insert(0,os.path.dirname(os.path.abspath(__file__)))
from rpc import rpc,batch,h2i
from decode import decode_log
V="0x09d5d7a5c3e94b6ae42b09889a1cee50f970fc5e"; C="0x8e7d2f9a77fec95c7e4875b0bd858d5de2b6def8"
head=h2i(rpc("eth_blockNumber")); lo=7895249
def scan(addr):
    out=[]; b=lo
    while b<=head:
        e=min(b+4999,head)
        try: out+=rpc("eth_getLogs",[{"address":addr,"fromBlock":hex(b),"toBlock":hex(e)}])
        except Exception: pass
        b=e+1
    return out
vl=scan(V); cl=scan(C)
EV=[]
def add(blk,actor,kind,label,detail,ev,tx=None,epoch=None,no=None,amt=None):
    EV.append({"block":blk,"actor":actor,"kind":kind,"label":label,"detail":detail,
               "evidence":ev,"tx":tx,"epoch":epoch,"no":no,"amount":amt})
for l in vl:
    d=decode_log(l); b=h2i(l["blockNumber"]); tx=l["transactionHash"]
    if d["_event"]=="EmissionCaptured":
        add(b,"contract","capture",f"Emission captured e{d['epoch']} NO{d['noId']}",
            f"{d['amount']:,} alpha-rao","chain",tx,d["epoch"],d["noId"],d["amount"])
    elif d["_event"]=="EntitlementFinalized":
        add(b,"contract","entitlement",f"Entitlement finalized e{d['epoch']} NO{d['noId']}",
            f"total {d['total']:,} alpha-rao, expiry {d['expiryBlock']:,}","chain",tx,d["epoch"],d["noId"],d["total"])
    elif d["_event"]=="RootMissed":
        add(b,"operator","rootmissed",f"Root missed e{d['epoch']} NO{d['noId']}",
            f"carried {d['carried']:,}","fail",tx,d["epoch"],d["noId"],d["carried"])
    elif d["_event"]=="Claimed":
        add(b,"miner","claimed",f"Claim accepted e{d['epoch']} NO{d['noId']}",
            f"{d['shareBps']} bps -> {d['amount']:,} alpha-rao","chain",tx,d["epoch"],d["noId"],d["amount"])
    elif d["_event"]=="ClaimPaid":
        add(b,"miner","claimpaid","Claim paid",f"{d['amount']:,} alpha-rao to {d['coldkey'][:14]}..","chain",tx,None,None,d["amount"])
    elif d["_event"]=="EmissionDeferred":
        add(b,"contract","deferred",f"Emission deferred e{d['epoch']} NO{d['noId']}","below transfer minimum","chain",tx,d["epoch"],d["noId"])
    elif d["_event"]=="PoolRegistered":
        add(b,"contract","register",f"Pool registered NO{d['noId']}",f"UID {d['uid']}","chain",tx,None,d["noId"])
    elif d["_event"]=="EscrowRegistered":
        add(b,"contract","register","Escrow hotkey registered",f"UID {d['uid']}","chain",tx)
for l in cl:
    d=decode_log(l)
    if not d: continue
    b=h2i(l["blockNumber"]); tx=l["transactionHash"]
    if d["_event"]=="OperatorRootCommitted":
        add(b,"operator","rootcommit",f"Payout root committed e{d['epoch']} NO{d['noId']}",
            f"root {d['payoutRoot'][:14]}.. artifact {d['artifactHash'][:14]}..","chain",tx,d["epoch"],d["noId"])
    elif d["_event"]=="Deposit":
        add(b,"operator","deposit",f"Deposit e{d['epoch']} NO{d['noId']}",
            f"{d['amount']:,} alpha-rao, nonce {d['nonce']}","chain",tx,d["epoch"],d["noId"],d["amount"])
    elif d["_event"]=="Upgraded":
        add(b,"contract","upgrade","Coordinator implementation upgraded",f"-> {d['implementation']}","chain",tx)
    elif d["_event"]=="PolicyScheduled":
        add(b,"contract","policy",f"Policy v{d['index']} scheduled",
            f"effective epoch {d['effectiveEpoch']}, block {d['effectiveBlock']:,}","chain",tx)
    elif d["_event"]=="OperatorScheduled":
        add(b,"operator","opreg",f"Operator NO{d['noId']} registered",f"effective epoch {d['effectiveEpoch']}","chain",tx)
    elif d["_event"]=="ConvictionAdded":
        add(b,"operator","conviction",f"Conviction added e{d['epoch']} NO{d['noId']}",f"{d['amount']:,} alpha-rao","chain",tx,d["epoch"],d["noId"],d["amount"])
    elif d["_event"]=="ValidatorEvidenceFixed":
        add(b,"contract","register","Validator evidence contract fixed",d["evidence"],"chain",tx)
# native validator events
for lbl,inc,app,row in [("1401",7986181,7986499,"[7:65534, 8:65535]"),
                        ("1404",7987311,7987601,"[3:65517, 4:65535, 7:24071, 8:32094]"),
                        ("1405",7987774,7988052,"[7:65534, 8:65535]")]:
    add(inc,"validator","weightset",f"Native weights committed (epoch {lbl})","substrate extrinsic, BLAKE2b-256 located in block","chain")
    add(app,"validator","weightapply",f"Native weights applied (epoch {lbl})",f"UID255 row {row}","chain")
EV.sort(key=lambda e:(e["block"],e["actor"]))
json.dump(EV,open("events.json","w"),indent=1)
from collections import Counter
print("events:",len(EV))
print(Counter(e["kind"] for e in EV))
print("block range",EV[0]["block"],"->",EV[-1]["block"])
print("actors:",Counter(e["actor"] for e in EV))
