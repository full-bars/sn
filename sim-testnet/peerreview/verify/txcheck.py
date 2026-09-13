from rpc import *
import json

# (label, txhash, claimed_block, claimed_blockhash_or_None)
TXS = [
 ("repair.deploy",   "0x7459e328f865d14a7818757a57edd41655b35ba6a1fff0d06c0cfb0dd74c22ca", 7986577, "0x6a4a4dfafa4792e294c14f3b0545e8dc920917798be3291b574e1aa10ab6dda8"),
 ("repair.activate", "0x4255f99d94abecfb2a48804090890059d070ca8e66821038667e873fb20fc7b2", 7986580, "0xeb101aeb317fee5b3f27c44540b63c4f4eeb46882ead59654e36b37dd57f22f9"),
 # epoch304 -> epoch305 deposits
 ("op1.close.e304",  "0x3515b8f158e877d0fdec85b2af16f0a3f66a42fc4ce606cff6eeb4779d791210", 7986877, None),
 ("op2.close.e304",  "0x23d4e8e44bbb518ba4a9406b9d516f1812fa54969eecaf29b87dbd90c32f58d7", 7986877, None),
 ("op1.root.e304",   "0x6fa42bc0966361dcf3339e66073fd987a82423a144db6183f1572c3b7ac57fbf", 7986880, None),
 ("op2.root.e304",   "0xbbc659ea638688ee1d4a95f6fb9c28da22d87d26dc945a6d81ac02cefd392196", 7986880, None),
 ("op1.deposit.e305","0x83de36b625f56938e870054ff42fe995b810ca0866505ea27d3db1eac8301629", 7986883, None),
 ("op2.deposit.e305","0x4fd861a212206954d7653371bbcbdd423d884083fc2f8d83a251697b06709a03", 7986884, None),
 # expired 303 cancellations
 ("cancel303.a",     "0xfacbbdbafb2bbd42a22df6f6fd3eb800203b9218180688869c76c771fea1873d", 7986880, None),
 ("cancel303.b",     "0xb525d616f4aff2108ef8669e1073cf1ada1c68991369b73293d38f0315f0c72a", 7986881, None),
 # epoch305 -> epoch306 deposits
 ("op1.close.e305",  "0x2beb0b5f6b2aa44c98f9c9b443af8dc7171e34e6906d7a7ae1770d285ca1d404", 7987177, None),
 ("op2.close.e305",  "0x09a2907ecef69c779a3cb84fd71785488e6f80490c9a80fa162ce8b7269f3736", 7987178, None),
 ("op1.deposit.e306","0x90854876441e51523f1f7f68055e6d18a36e379e7296c6ec6f7f1e0569221137", 7987180, None),
 ("op1.root.e305",   "0x1a0474c1f2a2ab2bde029663fdeeb346da7e1790346b62c693a761dfe645f4cd", 7987180, None),
 ("op2.deposit.e306","0xb299d8668610be14d44c4e2f8f461a3fc8fc68ee3ec35fabfe31e94ed32ad97f", 7987181, None),
 ("op2.root.e305",   "0x970bae6a9c2562ce6b73707d762260043b6693e7778b984b16b63f5e8dcee132", 7987181, None),
 # epoch308 capture / epoch309 root+entitlement
 ("op1.capture.e308","0x0407e92951701d5d37ca7fe8806405b059b134189631c31e5313f6a8f31da9be", 7988077, "0x1dcd02a11b09acb82db3754bbce56f9f725ce51e6622b8f9cf45c01b3bc411b1"),
 ("op2.capture.e308","0x1dc4a426cd9c680bbee8c24667583be770368f233182d4ff9281dd57eea3a075", 7988077, "0x1dcd02a11b09acb82db3754bbce56f9f725ce51e6622b8f9cf45c01b3bc411b1"),
 ("op1.rootcommit.e309","0x2c2aebec65a93ff31733d2379accc69325a05259fd433f3b7bd665ecdb47ed02", 7988380, "0x0c33d6772165ae5d43bf92be9271e803ed256def1fca4f2e5c051fb92a8beb04"),
 ("op2.rootcommit.e309","0x7c4238ca0f7066412842792db04ab1f0c2bb3a96529de6600ee7e6a2c3ffabf9", 7988380, "0x0c33d6772165ae5d43bf92be9271e803ed256def1fca4f2e5c051fb92a8beb04"),
 ("op1.entitlement.e309","0x6255bbc039103fa0a23e9a6d52adf50097fd5d3cc44333389018e7b90204fde3", 7988527, "0xed1317804b8787d5d9f9bb27a0c54ea613de380c49bb5c5a27f602afe683b240"),
 ("op2.entitlement.e309","0x7347ec8d8a7cc81fc8bcb33f1447864c3cef75b33a2e2b8a403475ddd009479a", 7988527, "0xed1317804b8787d5d9f9bb27a0c54ea613de380c49bb5c5a27f602afe683b240"),
 # epoch310 staged deposits + finalization
 ("op1.deposit.e310","0x148d1227bc74f99e50113ea484b4624233f2452b9c6cf5fb3aed85d208c55319", 7988380, None),
 ("op2.deposit.e310","0x3d9b916104fe217455a6aa629f50c680cd647d5fd3f22c6415f64f0eb9aa3f59", 7988381, None),
 ("epoch310.finalize","0xbe6ff669419f7896bbe5285c7e927eaef9c3a1533529c2d9294e69aa46b73487", 7988827, "0xda2e1edb9d55312bc1cc93e0334ff531a335bef4d098c1466511b9a3a19496ca"),
]

CLAIMS = [
 ("op1.miner-108", "0xa4c7c71e07d342927e65415e4ee52ab87cd7860ebd685c617bb6456d300e1b01", 7990845, 6492811278),
 ("op2.miner-15",  "0x7f1c3bc2b613a0f861a0b9d8501a862682470b6abc9e7288e405954189db4ea2", 7990855, 6463594645),
 ("op1.miner-153", "0xc3946bd6da75d006f9f14553dff8304568a470dd6279da56ea56ad8d609f9752", 7990858, 6270702380),
 ("op2.miner-173", "0xebdef59c41ff2d5bc0d966ffda8081d039fa23ff8c1f1a9c2e647ee5428f4c4e", 7990861, 6484261614),
 ("op1.miner-242", "0x50ae812cf312373f8da7a90da632371141cec38480477e183baf05db0556bcd3", 7990864, 6544464510),
 ("op2.miner-262", "0x8c7ca43fe4c8cd7b340eea16d7d7f6753286632d2f8979aa8ee97819924640ae", 7990867, 6396426995),
 ("op1.miner-441", "0x70b39656ef3e140ab3b9d659861df704a79cb5d542c2bfcb6f309ac2403a3c50", 7990870, 6513472571),
 ("op2.miner-350", "0xc4247c1d16c815549fc6656d89155503182a728a25c6fe3f2095d855045c3237", 7990873, 6396426995),
 ("op1.miner-467", "0x0adddac416007e09922d9ee7cc56583ce5c767a88554b55a25794050dba1b2f2", 7990876, 6477315309),
 ("op2.miner-399", "0xe1d5e0c564b37b649ca995ab9dd42222def0d7f295d045c84b6f0edbbdb6c9fe", 7990879, 6572096234),
 ("op1.miner-489", "0xe2076806e1a5fc997a136bbf475d4e7e2a1cb384bce59ef089864adcf70d8d22", 7990882, 6487645955),
 ("op2.miner-432", "0xe4beca7f56be327575410b49d14f32b5b3e55761435bbe5f911090a2274442c4", 7990885, 6546262522),
 ("op1.miner-779", "0x51cc2a5d20a71c16338611f242711287a05ddc7f597d60d4b02d62c749bfddfb", 7990888, 6446323369),
 ("op2.miner-902", "0xa4a0f09b0ec33ac349d715c84d030568f28a7371819b8ebbd17a43f31f1f1a9e", 7990891, 6463594645),
 ("op1.miner-817", "0xb2b30aa8e1cc97ffd00a0337b1702f9cb8d49ddd139d2db6dea8160ea468b173", 7990894, 6420496753),
 ("op2.miner-930", "0x078d894339066e170b8ed8d35b26be196e58bd126a4a4a9e72900842a3001fa7", 7990897, 6344759571),
]

ALL = [(l,h,b,bh) for (l,h,b,bh) in TXS] + [(l,h,b,None) for (l,h,b,_a) in CLAIMS]
res = batch([("eth_getTransactionReceipt",[h]) for (_l,h,_b,_bh) in ALL])
out = {}
fails = []
for (label,h,cb,cbh), r in zip(ALL, res):
    if not r or "__error__" in (r if isinstance(r,dict) else {}):
        fails.append(f"{label} {h}: NO RECEIPT"); continue
    gb = h2i(r["blockNumber"]); st = r["status"]; bh = r["blockHash"]
    ok = (gb==cb) and st=="0x1" and (cbh is None or bh.lower()==cbh.lower())
    out[label] = {"tx":h,"block":gb,"status":st,"blockHash":bh,"gasUsed":h2i(r["gasUsed"]),
                  "effectiveGasPrice":h2i(r.get("effectiveGasPrice","0x0")),"logs":len(r["logs"]),
                  "to":r.get("to"),"from":r.get("from"),"contractAddress":r.get("contractAddress")}
    if not ok:
        fails.append(f"{label} {h}: block got {gb} want {cb}; status {st}; bh {bh} want {cbh}")
json.dump(out, open("receipts.json","w"), indent=1)
print(f"checked {len(ALL)} transactions; {len(out)} receipts found")
print("FAILURES:", len(fails))
for f in fails: print("  ", f)
