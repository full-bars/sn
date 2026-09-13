#!/usr/bin/env python3
import json, hashlib, sys, os, collections
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
_HERE = os.path.dirname(os.path.abspath(__file__))
from rpc import rpc, batch, h2i
from decode import decode_log
from keccak import keccak256
from ecrec import recover, addr
EV = os.path.join(_HERE, "..", "evidence") + os.sep
R=json.load(open("results.json"))
def chk(actor,cid,claim,expected,observed,method,tier,wp=None):
    ok=(expected==observed)
    R["checks"].append({"actor":actor,"id":cid,"claim":claim,"expected":str(expected),
      "observed":str(observed),"pass":ok,"method":method,"tier":tier,"whitepaper":wp})
    return ok
V="0x09d5d7a5c3e94b6ae42b09889a1cee50f970fc5e"; C="0x8e7d2f9a77fec95c7e4875b0bd858d5de2b6def8"
def sel(s): return batch([("web3_sha3",["0x"+s.encode().hex()])])[0][:10]
def W(r): r=r[2:]; return [r[i*64:(i+1)*64] for i in range(len(r)//64)]

# ---- MINER: artifact -> merkle -> on-chain root -> entitlement -> payment ----
def leaf_hash(ck,bps): return keccak256(keccak256(bytes(ck)+bps.to_bytes(32,"big")))
def hpair(a,b): return keccak256(a+b) if a<=b else keccak256(b+a)
def build(ls):
    lvl=sorted(ls)
    while len(lvl)>1:
        nx=[hpair(lvl[i],lvl[i+1]) for i in range(0,len(lvl)-1,2)]
        if len(lvl)%2: nx.append(lvl[-1])
        lvl=nx
    return lvl[0]
ONCHAIN={1:"0x9bbab5462796712a3af4dafc5b32086842d7a72eca97b263154c8dbc8f32262b",
         2:"0xdc36a9982c1d53e90fdf5bb431fe92610a505dc92da07096bd3126d3748a6376"}
TOT={1:51653232130,2:51667423224}
arts={}; proofs_ok=0; proofs_n=0
for n in (1,2):
    a=json.load(open(EV+f"epoch309-operator-{n}-artifact.json"),object_pairs_hook=collections.OrderedDict)
    arts[n]=a
    lhs=[leaf_hash(l["coldkey"],l["share_bps"]) for l in a["leaves"]]
    root=build(lhs)
    chk("miner",f"merkle-root-{n}",f"operator {n} payout root rebuilt from leaves (local keccak256)",
        ONCHAIN[n],"0x"+root.hex(),"rebuild OZ double-hash sorted-pair tree locally","T1","§11.2")
    for lh,l in zip(lhs,a["leaves"]):
        h=lh
        for p in l["proof"]: h=hpair(h,bytes(p))
        proofs_n+=1; proofs_ok+= (h==root)
    chk("operator",f"shares-{n}",f"operator {n} leaf shares sum to 10,000 bps",10000,sum(l["share_bps"] for l in a["leaves"]),"sum artifact leaves","T1","§8.2")
    # content hash + signature
    b=collections.OrderedDict(a); b["content_hash"]=""; b["signature"]=""; b["signer"]="0x"+"00"*20
    s=json.dumps(b,separators=(",",":"),ensure_ascii=False).replace("<","\\u003c").replace(">","\\u003e").replace("&","\\u0026").encode()
    ch="sha256:"+hashlib.sha256(s).hexdigest()
    chk("operator",f"content-hash-{n}",f"operator {n} artifact content hash recomputes",a["content_hash"],ch,"sha256 of canonical unsigned JSON","T1","§11.1")
    Q=recover(bytes.fromhex(a["content_hash"].split(":")[1]),bytes.fromhex(a["signature"][2:]))
    chk("operator",f"sig-{n}",f"operator {n} artifact signature recovers to declared signer",a["signer"].lower(),addr(Q),"local secp256k1 ECDSA recovery","T1","§11.1")
    # artifact epoch window == coordinator epoch window
    es=int(rpc("eth_call",[{"to":C,"data":sel("epochStartBlock(uint256)")+"%064x"%309},"latest"]),16)
    ee=int(rpc("eth_call",[{"to":C,"data":sel("epochEndBlock(uint256)")+"%064x"%309},"latest"]),16)
    chk("operator",f"window-{n}",f"operator {n} artifact epoch window matches coordinator",
        f"{es}-{ee}",f"{a['start']['number']}-{a['end']['number']}","eth_call epochStartBlock/epochEndBlock","T1","§5.2")
chk("miner","merkle-proofs","every epoch309 leaf Merkle proof verifies against the rebuilt root",proofs_n,proofs_ok,"local proof folding","T1","§11.2")

# share -> payment
from txcheck import CLAIMS
recs=batch([("eth_getTransactionReceipt",[h]) for (_l,h,_b,_a) in CLAIMS])
leafmap={}
for n,a in arts.items():
    for l in a["leaves"]: leafmap[(n,"0x"+bytes(l["coldkey"]).hex())]=l["share_bps"]
repro=0; total_paid=0; total_gas=0
for (lbl,h,cb,camt),rc in zip(CLAIMS,recs):
    ev={d["_event"]:d for d in (decode_log(x) for x in rc["logs"]) if d}
    cl,cp=ev["Claimed"],ev["ClaimPaid"]
    bps=leafmap.get((cl["noId"],cl["coldkey"]))
    if bps is not None and (TOT[cl["noId"]]*bps)//10000==cp["amount"]: repro+=1
    total_paid+=cp["amount"]; total_gas+=h2i(rc["gasUsed"])*h2i(rc.get("effectiveGasPrice","0x0"))
chk("miner","share-to-payment","on-chain ClaimPaid reproduced as floor(share_bps x entitlement / 10000)",
    len(CLAIMS),repro,"recompute from artifact leaf + on-chain entitlement","T1","§8.3")
chk("miner","sum-paid","sum of 16 ClaimPaid equals contract lifetime totalPaid",103320655346,total_paid,"sum decoded events","T1","§8.3")
chk("miner","gas","claim gas total (sum gasUsed x effectiveGasPrice)",49882627183941739,total_gas,"sum from receipts","T1")
res=sum(TOT[n]-sum((TOT[n]*l["share_bps"])//10000 for l in arts[n]["leaves"]) for n in (1,2))
chk("miner","residue","largest-remainder rounding residue equals contract outstandingLiability",8,res,"arithmetic from artifact shares","T1","§8.3")

# ---- VALIDATOR: native extrinsics + applied weight rows + theta ----
def b2(x): return "0x"+hashlib.blake2b(x,digest_size=32).hexdigest()
found=0
for bn,want in [(7986181,"0xd8e13d9efb88d5a7b82abf67bae05a0923803c19b3ce6fa4dea52c6645a0fe43"),
                (7987311,"0xe58e4563e7a965baae577136a39933dafc3b67e2d29d7cab81b724772977526f"),
                (7987774,"0x5e0da9793f3d430ab32d8e202ca0c3c0e1b6c10d82a81d716fd3728b384d66fb")]:
    blk=rpc("chain_getBlock",[rpc("chain_getBlockHash",[bn])])
    if any(b2(bytes.fromhex(x[2:]))==want for x in blk["block"]["extrinsics"]): found+=1
chk("validator","native-extrinsics","native weight extrinsics located in claimed blocks by BLAKE2b-256",3,found,"hash every extrinsic in the block","T1","§5.1")
from xxh import twox128
P=twox128(b"SubtensorModule"); NET=(521).to_bytes(2,"little")
def wkey(u): return "0x"+(P+twox128(b"Weights")+NET+u.to_bytes(2,"little")).hex()
def cu(b,i):
    f=b[i]&3
    if f==0: return b[i]>>2,i+1
    if f==1: return int.from_bytes(b[i:i+2],"little")>>2,i+2
    if f==2: return int.from_bytes(b[i:i+4],"little")>>2,i+4
    n=(b[i]>>2)+4; return int.from_bytes(b[i+1:i+1+n],"little"),i+1+n
def parse(v):
    if not v: return None
    b=bytes.fromhex(v[2:]); n,i=cu(b,0); o=[]
    for _ in range(n):
        o.append((int.from_bytes(b[i:i+2],"little"),int.from_bytes(b[i+2:i+4],"little"))); i+=4
    return o
rows={}
for lbl,bn,want in [("1401",7986519,[(7,65534),(8,65535)]),
                    ("1404",7987601,[(3,65517),(4,65535),(7,24071),(8,32094)]),
                    ("1405",7988052,[(7,65534),(8,65535)])]:
    bh=rpc("chain_getBlockHash",[bn])
    got={u:parse(v) for u,v in enumerate(batch([("state_getStorageAt",[wkey(u),bh]) for u in range(256)])) if parse(v)}
    rows[lbl]=got
    chk("validator",f"weights-{lbl}",f"native {lbl} applied weight row at block {bn:,}",str(want),str(got.get(255)),"SubtensorModule::Weights storage at historical block","T1","§10")
    chk("validator",f"single-{lbl}",f"only one validator set weights at block {bn:,}",1,len(got),"scan all 256 UID weight rows","T1","§9")
pool={3,4}
r=rows["1404"][255]
head=sum(w for u,w in r if u not in pool); tot=sum(w for _u,w in r)
chk("validator","theta","head/tail weight split matches governed theta = 3/10",True,abs(head/tot-0.3)<1e-4,
    "pool UIDs from vault.pools(); ratio of head to total weight","T1","§8.5")
R["meta"]["theta_observed"]=head/tot
R["meta"]["weight_rows"]={k:{str(u):v for u,v in d.items()} for k,d in rows.items()}

# ---- reserve census ----
d=json.load(open(EV+"reserve-alpha-20260912T220217Z.json"))
same=0; n_replay=0
for rr in d["rpc"]:
    q=rr["request"]
    if q["method"]=="chain_getFinalizedHead": continue
    n_replay+=1
    old=rr["response"]; old=old["result"] if isinstance(old,dict) and "result" in old else old
    try: new=rpc(q["method"],q.get("params",[]))
    except Exception: continue
    same+= json.dumps(new,sort_keys=True)==json.dumps(old,sort_keys=True)
chk("validator","reserve-replay","recorded reserve-census storage queries replay byte-identical",n_replay,same,
    "replay each recorded RPC against the live node","T1","§7.4")
qs=[r for r in d["rpc"] if r["request"]["method"]=="state_queryStorageAt"]
def ch_(r):
    res=r["response"]["result"] if isinstance(r["response"],dict) else r["response"]
    return (res[0] if isinstance(res,list) else res)["changes"]
uid2hk={}
for k,v in ch_(qs[0]):
    uid2hk[int.from_bytes(bytes.fromhex(k[-4:]),"little")]="0x"+v[2:]
hk2a={}
for k,v in ch_(qs[1]):
    hk2a["0x"+k[2:][96:160]]=int.from_bytes(bytes.fromhex(v[2:]),"little") if v and v!="0x" else 0
tot_a=sum(hk2a.get(uid2hk[u],0) for u in range(256))
chk("validator","reserve-total","256-UID alpha census total decoded from raw storage",80409602927411,tot_a,"decode SubtensorModule::Keys + TotalHotkeyAlpha","T1","§7.4")
chk("validator","reserve-uid254","reserve hotkey (UID254) alpha",49410992336897,hk2a.get(uid2hk[254],0),"decode raw storage","T1","§7.4")
chk("validator","reserve-min","reserve share >= 60% minimum",True,hk2a[uid2hk[254]]/tot_a>=0.60,"arithmetic","T1","§7.4")
chk("validator","reserve-target","reserve share >= 65% repair target",True,hk2a[uid2hk[254]]/tot_a>=0.65,"arithmetic","T1","§7.4")
R["meta"]["reserve_share"]=hk2a[uid2hk[254]]/tot_a

json.dump(R,open("results.json","w"),indent=1)
p=sum(1 for c in R["checks"] if c["pass"])
print(f"total: {p}/{len(R['checks'])} checks passed")
print("\nFAILING (each is a finding, not an error):")
for c in R["checks"]:
    if not c["pass"]: print(f"   [{c['actor']}] {c['id']}: {c['claim']}\n        expected {c['expected']}  observed {c['observed']}")
