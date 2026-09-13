#!/usr/bin/env python3
"""Independent re-verification of sim-testnet/FINAL.md against Bittensor testnet netuid 521.
Reads only. Emits results.json. Every check re-derives the claim from chain state or
from local cryptography -- no value is taken from the report on trust."""
import json, hashlib, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
_HERE = os.path.dirname(os.path.abspath(__file__))
from rpc import rpc, batch, h2i
from decode import decode_log
from keccak import keccak256
from ecrec import recover, addr
from xxh import twox128

EV = os.path.join(_HERE, "..", "evidence") + os.sep
R = {"checks": [], "meta": {}}
def chk(actor, cid, claim, expected, observed, method, tier, whitepaper=None):
    ok = (expected == observed)
    R["checks"].append({"actor":actor,"id":cid,"claim":claim,"expected":str(expected),
        "observed":str(observed),"pass":ok,"method":method,"tier":tier,"whitepaper":whitepaper})
    return ok

# ---------- node identity ----------
R["meta"]["endpoint"]="http://192.168.1.162:9944"
R["meta"]["eth_chainId"]=rpc("eth_chainId")
R["meta"]["system_chain"]=rpc("system_chain")
R["meta"]["head"]=h2i(rpc("eth_blockNumber"))
R["meta"]["genesis"]=rpc("chain_getBlockHash",[0])
chk("contract","chain-id","EVM chain ID is 945","0x3b1",rpc("eth_chainId"),"eth_chainId","T1")

V="0x09d5d7a5c3e94b6ae42b09889a1cee50f970fc5e"; C="0x8e7d2f9a77fec95c7e4875b0bd858d5de2b6def8"
RS="0x376f98bd7c6b334f7f1cb2685E0970a18bfe7d28"
def sel(s): return batch([("web3_sha3",["0x"+s.encode().hex()])])[0][:10]
def call(to,sig,arg=None,tag="latest"):
    d=sel(sig)+("%064x"%arg if arg is not None else "")
    return rpc("eth_call",[{"to":to,"data":d},tag])
def W(r): r=r[2:]; return [r[i*64:(i+1)*64] for i in range(len(r)//64)]

chk("contract","netuid-vault","settlement vault is bound to netuid 521",521,int(call(V,"netuid()"),16),"eth_call netuid()","T1","§3")
chk("contract","netuid-coord","coordinator is bound to netuid 521",521,int(call(C,"netuid()"),16),"eth_call netuid()","T1","§3")

# ---------- 1. evidence bundle hashes ----------
for f,want in [("epoch309-paid-claims-20260912.json","2aa27bfb3efa15bb87f93fc9fbe6adbd8d64a1e57e5ff68fab6d5a1a2deb18ec"),
               ("onchain-receipts-20260912.json","4e491a2d543e13c17ba1fdf44fb766bc2b3b838e9f4b2ea650ec3e47a7da59de"),
               ("reserve-alpha-20260912T220217Z.json","cf8963f7603cbb09909a36ea944004295dbe920dc36ec39030984642258ca061"),
               ("retained-ledger-capacity-20260912.json","e61e6d64347f8dde98d08d52464440a1a03104c874526e59a3c6056a453f3ea2"),
               ("short-run-corrections-20260912.json","e6372f5bd2bb5de16d4e911f8a1b2a254e8ac3c237c0ef3b7eb3c98f1f98e5f7"),
               ("native1404-1405-historical-weights.json","6373f2a7d8b2d6c8c1dcedcf28d0dba5ee6bb9d339d8fdcd9d66d7faaf102592")]:
    got=hashlib.sha256(open(EV+f,"rb").read()).hexdigest()
    chk("contract","sha-"+f[:18],f"evidence bundle {f} sha256",want,got,"sha256 of file on disk","T1")

# ---------- 2. contract topology / upgradeability (WP 6.4) ----------
SLOT="0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc"
impl=rpc("eth_getStorageAt",[C,SLOT,"latest"])
chk("contract","proxy-impl","coordinator proxy implementation","0x40e5abde2bc4ba84d966842cbab98c0c88894aaf","0x"+impl[-40:],"EIP-1967 slot read","T1","§6.4")
chk("contract","impl-codehash","activated implementation runtime keccak",
    "0xc8837dcf6ebb607277f140677c73ad91f3359ef25bfda17e578ad52e2d4f5179",
    "0x"+keccak256(bytes.fromhex(rpc("eth_getCode",["0x40e5abde2bc4ba84d966842cbab98c0c88894aaf","latest"])[2:])).hex(),
    "local keccak256 of eth_getCode","T1","§6.4")
for nm,a in [("vault",V),("reserve-sink",RS)]:
    s=rpc("eth_getStorageAt",[a,SLOT,"latest"])
    chk("contract",f"nonupgradeable-{nm}",f"{nm} has no EIP-1967 implementation slot (non-upgradeable)",0,int(s,16),"EIP-1967 slot read","T1","§6.4")

# ---------- 3. deployed policy vs whitepaper 5 ----------
n=int(call(C,"policyCount()"),16)
R["meta"]["policyCount"]=n; R["meta"]["currentEpoch"]=int(call(C,"currentEpoch()"),16)
pols=[]
for i in range(n):
    w=W(rpc("eth_call",[{"to":C,"data":sel("policyByIndex(uint256)")+"%064x"%i},"latest"]))
    pols.append({"policyHash":"0x"+w[0],"effectiveEpoch":int(w[1],16),"epochBlocks":int(w[3],16),
                 "rootCommitWindowBlocks":int(w[4],16),"finalizeOffsetBlocks":int(w[5],16),
                 "closeGraceBlocks":int(w[6],16),"claimTTLEpochs":int(w[7],16)})
R["meta"]["policies"]=pols
chk("contract","policy-count","number of policy versions ever scheduled on chain",2,n,"eth_call policyCount()","T1","§5")
chk("contract","epoch-blocks","active policy epoch length (blocks)",300,pols[-1]["epochBlocks"],"eth_call policyByIndex()","T1","§5")
chk("contract","wp-cadence","whitepaper §5 testnet cadence (360/60/180/6) scheduled on chain",
    True, any(p["epochBlocks"]==360 for p in pols),"enumerate all on-chain policy versions","T1","§5")

# ---------- 4. subnet hyperparameters vs whitepaper 15.1 ----------
r=rpc("subnetInfo_getSubnetHyperparamsV2",[521]); b=bytes(r) if isinstance(r,list) else bytes.fromhex(r[2:])
i=[0]
def cu():
    f=b[i[0]]&3
    if f==0: v=b[i[0]]>>2; i[0]+=1
    elif f==1: v=int.from_bytes(b[i[0]:i[0]+2],"little")>>2; i[0]+=2
    elif f==2: v=int.from_bytes(b[i[0]:i[0]+4],"little")>>2; i[0]+=4
    else:
        k=(b[i[0]]>>2)+4; v=int.from_bytes(b[i[0]+1:i[0]+1+k],"little"); i[0]+=1+k
    return v
def bo():
    v=bool(b[i[0]]); i[0]+=1; return v
hp={}
for nm,f in [("rho",cu),("kappa",cu),("immunity_period",cu),("min_allowed_weights",cu),
  ("max_weights_limit",cu),("tempo",cu),("min_difficulty",cu),("max_difficulty",cu),
  ("weights_version",cu),("weights_rate_limit",cu),("adjustment_interval",cu),("activity_cutoff",cu),
  ("registration_allowed",bo),("target_regs_per_interval",cu),("min_burn",cu),("max_burn",cu),
  ("bonds_moving_avg",cu),("max_regs_per_block",cu),("serving_rate_limit",cu),("max_validators",cu),
  ("adjustment_alpha",cu),("difficulty",cu),("commit_reveal_period",cu),
  ("commit_reveal_weights_enabled",bo),("alpha_high",cu),("alpha_low",cu),("liquid_alpha_enabled",bo)]:
    hp[nm]=f()
R["meta"]["hyperparams"]=hp
P=twox128(b"SubtensorModule"); NET=(521).to_bytes(2,"little")
mau=rpc("state_getStorage",["0x"+(P+twox128(b"MaxAllowedUids")+NET).hex()])
hp["max_allowed_uids"]=int.from_bytes(bytes.fromhex(mau[2:]),"little")
chk("validator","hp-tempo","tempo",360,hp["tempo"],"subnetInfo_getSubnetHyperparamsV2","T1","§15.1")
chk("validator","hp-uids","max_allowed_uids",256,hp["max_allowed_uids"],"SubtensorModule::MaxAllowedUids storage","T1","§15.1")
chk("validator","hp-cr","commit_reveal_weights_enabled",True,hp["commit_reveal_weights_enabled"],"runtime API","T1","§15.1")
chk("validator","hp-la","liquid_alpha_enabled",True,hp["liquid_alpha_enabled"],"runtime API","T1","§15.1")
chk("validator","hp-maw","min_allowed_weights",1,hp["min_allowed_weights"],"runtime API","T1","§15.1")
chk("validator","hp-imm","immunity_period > commit_reveal_period x tempo",True,
    hp["immunity_period"]>hp["commit_reveal_period"]*hp["tempo"],"runtime API","T1","§15.1")
chk("validator","hp-maxval","max_allowed_validators <= 56 target",True,hp["max_validators"]<=56,"runtime API","T1","§15.1")

# ---------- 5. transaction receipts ----------
from txcheck import TXS, CLAIMS
allt=[(l,h,bn,bh) for (l,h,bn,bh) in TXS]+[(l,h,bn,None) for (l,h,bn,_a) in CLAIMS]
recs=batch([("eth_getTransactionReceipt",[h]) for (_l,h,_b,_bh) in allt])
nrec=0; nblk=0
for (l,h,cb,cbh),rc in zip(allt,recs):
    if rc and rc.get("status")=="0x1": nrec+=1
    if rc and h2i(rc["blockNumber"])==cb: nblk+=1
chk("contract","receipts","EVM receipts with status=0x1 at the claimed height",len(allt),min(nrec,nblk),
    "eth_getTransactionReceipt for all 41 cited transactions","T1")
# canonical block hashes
cano=0; tot=0
for bn,want in [(7990697,"0x8f6ad48eb003878747eea7fb4050070d20fb84ecfc304e7541282c41ceae5178"),
    (7990906,"0xf11d60fc8e9c13fc7b51f0b848e7e98df51bd492a6cb8ee2d53e1ee369f46607"),
    (7988077,"0x1dcd02a11b09acb82db3754bbce56f9f725ce51e6622b8f9cf45c01b3bc411b1"),
    (7988380,"0x0c33d6772165ae5d43bf92be9271e803ed256def1fca4f2e5c051fb92a8beb04"),
    (7988527,"0xed1317804b8787d5d9f9bb27a0c54ea613de380c49bb5c5a27f602afe683b240"),
    (7988827,"0xda2e1edb9d55312bc1cc93e0334ff531a335bef4d098c1466511b9a3a19496ca"),
    (7986577,"0x6a4a4dfafa4792e294c14f3b0545e8dc920917798be3291b574e1aa10ab6dda8"),
    (7986580,"0xeb101aeb317fee5b3f27c44540b63c4f4eeb46882ead59654e36b37dd57f22f9")]:
    tot+=1
    if rpc("eth_getBlockByNumber",[hex(bn),False])["hash"].lower()==want.lower(): cano+=1
chk("contract","canonical","cited EVM block hashes canonical at that height",tot,cano,"eth_getBlockByNumber","T1")

# ---------- 6. vault accounting ----------
fns=["totalCaptured()","totalPaid()","pendingFunding()","outstandingLiability()","escrowAccounted()","conservationHolds()"]
sels=[s[:10] for s in batch([("web3_sha3",["0x"+f.encode().hex()]) for f in fns])]
def vstate(tag):
    return {f:int(x,16) for f,x in zip(fns,batch([("eth_call",[{"to":V,"data":s},tag]) for s in sels]))}
s697=vstate(hex(7990697)); s906=vstate(hex(7990906)); slat=vstate("latest")
R["meta"]["vault_7990697"]=s697; R["meta"]["vault_7990906"]=s906; R["meta"]["vault_latest"]=slat
chk("contract","cap-697","totalCaptured @7,990,697",103320655354,s697["totalCaptured()"],"eth_call at historical block","T1","§8.3")
chk("contract","paid-697","totalPaid @7,990,697",0,s697["totalPaid()"],"eth_call at historical block","T1","§8.3")
chk("contract","liab-697","outstandingLiability @7,990,697",103320655354,s697["outstandingLiability()"],"eth_call","T1","§8.3")
chk("contract","cap-906","totalCaptured @7,990,906",103320655354,s906["totalCaptured()"],"eth_call","T1","§8.3")
chk("contract","paid-906","totalPaid @7,990,906",103320655346,s906["totalPaid()"],"eth_call","T1","§8.3")
chk("contract","liab-906","outstandingLiability @7,990,906",8,s906["outstandingLiability()"],"eth_call","T1","§8.3")
chk("contract","esc-906","escrowAccounted @7,990,906",8,s906["escrowAccounted()"],"eth_call","T1","§8.3")
chk("contract","cons-906","conservationHolds @7,990,906",1,s906["conservationHolds()"],"eth_call","T1","§8.3")
chk("contract","conserve","captured - paid == outstandingLiability",
    s906["totalCaptured()"]-s906["totalPaid()"],s906["outstandingLiability()"],"arithmetic identity on chain state","T1","§8.3")

json.dump(R, open("results.json","w"), indent=1)
print(f"stage 1: {sum(1 for c in R['checks'] if c['pass'])}/{len(R['checks'])} checks passed")
for c in R["checks"]:
    if not c["pass"]: print("   FAIL:", c["id"], c["claim"], "expected",c["expected"],"observed",c["observed"])
