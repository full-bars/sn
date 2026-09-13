# -*- coding: utf-8 -*-
import os
_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", "..", ".."))
import json, datetime
from gen_css import ev, legend_line

EVENTS = json.load(open("events.json"))
BT = {int(k):v for k,v in json.load(open("blocktimes.json")).items()}

def utc(ts): return datetime.datetime.fromtimestamp(ts, datetime.timezone.utc)
def tlabel(b):
    """Wall-clock label for a block, interpolated at the measured 12.000 s cadence."""
    anchor = 7986000; ts = BT[anchor] + (b-anchor)*12
    return utc(ts).strftime("%H:%M") + "Z"
def esc(s): return (str(s).replace("&","&amp;").replace("<","&lt;").replace(">","&gt;"))
def fmt(n): return f"{n:,}"

B0, B1 = 7983500, 7991150
PX0, PX1 = 104, 1176
def X(b): return PX0 + (b-B0)*(PX1-PX0)/(B1-B0)

# ---------------- master ruler ----------------
def master_ruler():
    H = 268
    s = [f'<svg class="fig" viewBox="0 0 1200 {H}" style="min-width:1000px;width:100%" role="img" '
         f'aria-label="Block-height ruler of the on-chain run, blocks {fmt(B0)} to {fmt(B1)}">']
    s.append('<style>'
      '.tk{stroke:var(--rule-2);stroke-width:1}.gl{stroke:var(--rule);stroke-width:1}'
      '.tl{font:11px var(--font-mono);fill:var(--ink-3)}'
      '.ul{font:10px var(--font-sans);fill:var(--ink-3)}'
      '.lb{font:10.5px var(--font-sans);fill:var(--ink-2)}'
      '.lbk{font:10.5px var(--font-sans);fill:var(--ink-1);font-weight:600}'
      '.dp{stroke:var(--drop);stroke-width:1}'
      '.rug{stroke:var(--ink-2);stroke-width:1;opacity:.55}'
      '.rugf{stroke:var(--ev-fail);stroke-width:2;opacity:.9}'
      '.ldr{stroke:var(--ink-3);stroke-width:1}'
      '.epg{stroke:var(--rule);stroke-width:1}'
      '.ep{font:9px var(--font-mono);fill:var(--ink-3)}'
      '</style>')
    AX = 186
    # --- UTC row
    t0 = BT[7986000] + (B0-7986000)*12
    first = (t0//7200+1)*7200
    tt = first
    while tt <= BT[7986000] + (B1-7986000)*12:
        b = 7986000 + (tt-BT[7986000])//12
        if B0 <= b <= B1:
            x = X(b)
            s.append(f'<line class="tk" x1="{x:.1f}" y1="20" x2="{x:.1f}" y2="26"/>')
            s.append(f'<text class="ul" x="{x:.1f}" y="15" text-anchor="middle">{utc(tt).strftime("%H:%M")}Z</text>')
        tt += 7200
    s.append(f'<text class="ul" x="{PX0}" y="15" text-anchor="start">UTC (2026-09-11 → 09-12)</text>')
    s.append(f'<line class="gl" x1="0" y1="30" x2="1200" y2="30"/>')

    # --- epoch grid (the 300-block spine)
    s.append(f'<text class="ep" x="8" y="46">epoch</text>')
    ep_start = 7983577  # epoch 293 boundary (measured)
    e = 293; b = ep_start
    while b <= B1:
        if b >= B0:
            x = X(b)
            s.append(f'<line class="epg" x1="{x:.1f}" y1="38" x2="{x:.1f}" y2="{AX}"/>')
            if e % 2 == 1:
                s.append(f'<text class="ep" x="{x+2:.1f}" y="46">{e}</text>')
        b += 300; e += 1
    s.append(f'<text class="ep" x="{PX1-2}" y="46" text-anchor="end">300-block epochs (measured)</text>')

    # --- milestone marks
    marks = [
      (7986580,"chain","repair activated"),
      (7986880,"chain","e304 close\u00b7root\u00b7deposit"),
      (7987180,"chain","e305 close\u00b7root\u00b7deposit"),
      (7988077,"chain","e308 only positive capture"),
      (7988227,"fail","e308 root missed \u2192 carried"),
      (7988380,"chain","e309 roots + e310 deposits"),
      (7988527,"chain","e309 entitlements funded"),
      (7988827,"fail","e310 finalized, 0 leaves"),
      (7990616,"fail","fleet stopped"),
      (7990871,"chain","16 claims paid"),
      (7990906,"chain","conservation holds"),
    ]
    # racks: (baseline_y, leader_anchor_y, is_above)
    RACKS=[(62,None,True),(78,None,True),(150,None,False),(166,None,False),(180,None,False)]
    used={i:[] for i in range(len(RACKS))}
    for b,kind,label in marks:
        x=X(b)
        s.append(f'<line class="dp" x1="{x:.1f}" y1="{116}" x2="{x:.1f}" y2="{AX}"/>')
        s.append(f'<g transform="translate({x-8:.1f},108)"><use href="#ev-{kind}" width="16" height="16">'
                 f'<title>{esc(label)} at block {fmt(b)}</title></use></g>')
        w=6.05*len(label)
        lx=min(max(x-w/2, PX0), PX1-w)
        placed=None
        for ri,(by_,_a,_ab) in enumerate(RACKS):
            if all(lx+w+8 < a or lx > bq+8 for a,bq in used[ri]):
                used[ri].append((lx,lx+w)); placed=ri; break
        if placed is None: continue
        by_,_a,above=RACKS[placed]
        cls="lbk" if kind=="fail" or "only positive" in label else "lb"
        s.append(f'<text class="{cls}" x="{lx:.1f}" y="{by_}">{esc(label)}</text>')
        cx=lx+w/2
        if abs(cx-x)>5:
            ly = by_+3 if above else by_-9
            ty = 106 if above else 126
            s.append(f'<line class="ldr" x1="{cx:.1f}" y1="{ly}" x2="{x:.1f}" y2="{ty}"/>')
    # expiry deadline marker
    xd = X(7991073)
    s.append(f'<line x1="{xd:.1f}" y1="38" x2="{xd:.1f}" y2="{AX}" stroke="var(--ink-3)" stroke-width="1" stroke-dasharray="2 3"/>')
    s.append(f'<text class="ep" x="{xd-3:.1f}" y="60" text-anchor="end">claim expiry 7,991,073</text>')
    # fleet-stop cut line
    xc = X(7990616)
    s.append(f'<line x1="{xc:.1f}" y1="30" x2="{xc:.1f}" y2="242" stroke="var(--ev-fail)" stroke-width="1.5" stroke-dasharray="5 3"/>')

    # --- axis
    s.append(f'<line class="tk" x1="{PX0}" y1="{AX}" x2="{PX1}" y2="{AX}"/>')
    b = ((B0//500)+1)*500
    while b <= B1:
        x = X(b); major = (b % 1000 == 0)
        s.append(f'<line class="tk" x1="{x:.1f}" y1="{AX}" x2="{x:.1f}" y2="{AX+(6 if major else 3)}"/>')
        if major:
            s.append(f'<text class="tl" x="{x:.1f}" y="{AX+18}" text-anchor="middle">{fmt(b)}</text>')
        b += 500
    s.append(f'<text class="ul" x="{PX0}" y="{AX+18}" text-anchor="start">block</text>')

    # --- rug
    RY = 216
    s.append(f'<text class="ep" x="8" y="{RY+9}">events</text>')
    for e_ in EVENTS:
        b = e_["block"]
        if not (B0 <= b <= B1): continue
        x = X(b); fail = e_["evidence"] == "fail"
        s.append(f'<line class="{"rugf" if fail else "rug"}" x1="{x:.1f}" y1="{RY}" x2="{x:.1f}" y2="{RY+18}"/>')
    s.append(f'<text class="ep" x="{PX1}" y="{RY+30}" text-anchor="end">'
             f'{len([e for e in EVENTS if B0<=e["block"]<=B1])} recorded on-chain events; red = recorded failure</text>')
    s.append("</svg>")
    return "".join(s)

# ---------------- per-actor sequence grid ----------------
def seq(stages, rows, colw=92, aria=""):
    """stages: [(name, wpref)]  rows: [(tag, [cell,...])]
       cell: None -> dash (never applicable) | dict(ev=, name=, blk=, out=)"""
    n=len(stages); W=120+colw*n+16; H=30+66*len(rows)+10
    s=[f'<svg class="fig" viewBox="0 0 {W} {H}" style="min-width:{W}px" role="img" aria-label="{esc(aria)}">']
    s.append('<style>'
      '.ch{font:600 11px var(--font-sans);fill:var(--ink-1)}'
      '.cw{font:9px var(--font-mono);fill:var(--ink-3)}'
      '.rt{font:11px var(--font-mono);fill:var(--ink-2)}'
      '.sp{stroke:var(--rule-2);stroke-width:1}'
      '.spd{stroke:var(--ink-3);stroke-width:1;stroke-dasharray:3 3}'
      '.n1{font:11px var(--font-sans);fill:var(--ink-2)}'
      '.n2{font:10px var(--font-mono);fill:var(--ink-3)}'
      '.n3{font:9px var(--font-sans);fill:var(--ink-3)}'
      '.dsh{stroke:var(--rule-2);stroke-width:1.5}'
      '.gsep{stroke:var(--rule);stroke-width:1}'
      '.term{stroke:var(--ev-fail);stroke-width:2}'
      '</style>')
    def cx(i): return 120+colw*i+colw/2
    for i,(nm,wp) in enumerate(stages):
        s.append(f'<text class="ch" x="{cx(i):.0f}" y="12" text-anchor="middle">{esc(nm)}</text>')
        if wp: s.append(f'<text class="cw" x="{cx(i):.0f}" y="23" text-anchor="middle">{esc(wp)}</text>')
    for r,(tag,cells) in enumerate(rows):
        top=30+66*r; spine=top+16
        s.append(f'<line class="gsep" x1="0" y1="{top-4}" x2="{W}" y2="{top-4}"/>')
        s.append(f'<text class="rt" x="108" y="{spine+4}" text-anchor="end">{esc(tag)}</text>')
        # spine: solid until last real cell, dashed after a terminal failure
        last=-1; termi=-1
        for i,c in enumerate(cells):
            if c: last=i
            if c and c.get("term"): termi=i; break
        endx = cx(last) if last>=0 else 120
        s.append(f'<line class="sp" x1="120" y1="{spine}" x2="{endx:.0f}" y2="{spine}"/>')
        if termi>=0 and termi<n-1:
            s.append(f'<line class="spd" x1="{cx(termi):.0f}" y1="{spine}" x2="{W-16}" y2="{spine}"/>')
            s.append(f'<line class="term" x1="{W-18}" y1="{spine-5}" x2="{W-18}" y2="{spine+5}"/>')
        elif last<n-1:
            s.append(f'<line class="spd" x1="{endx:.0f}" y1="{spine}" x2="{W-16}" y2="{spine}"/>')
        for i,c in enumerate(cells):
            x=cx(i)
            if not c:
                s.append(f'<line class="dsh" x1="{x-3:.0f}" y1="{spine}" x2="{x+3:.0f}" y2="{spine}"/>')
                continue
            s.append(f'<g transform="translate({x-8:.0f},{spine-8})"><use href="#ev-{c["ev"]}" width="16" height="16">'
                     f'<title>{esc(c.get("name",""))}</title></use></g>')
            if c.get("name"): s.append(f'<text class="n1" x="{x:.0f}" y="{top+40}" text-anchor="middle">{esc(c["name"])}</text>')
            if c.get("blk"):  s.append(f'<text class="n2" x="{x:.0f}" y="{top+52}" text-anchor="middle">{esc(c["blk"])}</text>')
            if c.get("out"):  s.append(f'<text class="n3" x="{x:.0f}" y="{top+62}" text-anchor="middle">{esc(c["out"])}</text>')
    s.append("</svg>")
    return "".join(s)

def C(evk,name,blk="",out="",term=False):
    return {"ev":evk,"name":name,"blk":blk,"out":out,"term":term}

def fig_validator():
    st=[("1401 committed","§5.1"),("1401 applied","§10"),("1403 input","§10"),("CLI48 redeploy",""),
        ("1404 committed","§5.1"),("1404 applied","§10"),("1405 committed","§5.1"),("1405 applied","§10"),
        ("restarts","§9")]
    v2=[C("chain","extrinsic","7,986,181","included"),
        C("chain","row applied","7,986,519","[7:65534, 8:65535]"),
        C("chain","input sealed","7,986,866","cut recorded"),
        C("artifact","restarted","≈7,987,080","CLI48"),
        C("chain","extrinsic","7,987,311","included"),
        C("chain","row applied","7,987,601","4 UIDs, θ=0.3"),
        C("chain","extrinsic","7,987,774","included"),
        C("chain","row applied","7,988,052","[7:65534, 8:65535]"),
        C("fail","exhausted","≈7,988,828","5 of 5",term=True)]
    v1=[None,None,
        C("fail","cut mismatch","7,986,856","record 48613"),
        C("artifact","restarted","≈7,987,080","CLI48"),
        None,None,None,None,
        C("fail","exhausted","≈7,990,626","5 of 5",term=True)]
    return seq(st,[("V1",v1),("V2 · UID255",v2)],aria="Validator sequence: V2 applied three native weight vectors; V1 applied none.")

def fig_operator():
    st=[("deposit e298","§7.5"),("root e304","§8.2"),("deposit e305","§7.5"),("root e305","§8.2"),
        ("deposit e306","§7.5"),("root e308","§8.2"),("root e309","§8.2"),("deposit e310","§7.5"),
        ("artifact e310","§11.1"),("commit e310","§8.2")]
    def row(no, dep298, dep305, dep306, r304,r305,r309, dep310):
        return [C("chain","Deposit",dep298[0],dep298[1]),
                C("chain","root","7,986,880","8 leaves"),
                C("chain","Deposit",dep305[0],dep305[1]),
                C("chain","root",r305,"8 leaves"),
                C("chain","Deposit",dep306[0],dep306[1]),
                C("fail","no root","—","RootMissed"),
                C("chain","root","7,988,380","8 leaves"),
                C("chain","Deposit",dep310,"204,950,031"),
                C("fail","0 leaves","≈7,988,674","root 0x00…00"),
                C("none","skipped","—","nothing to commit",term=True)]
    op1=row(1,("7,984,784","256,187,532"),("7,986,883","205,051,017"),("7,987,180","204,495,792"),"7,986,880","7,987,180","7,988,380","7,988,380")
    op2=row(2,("7,984,784","256,187,540"),("7,986,884","256,313,850"),("7,987,181","255,690,716"),"7,986,880","7,987,181","7,988,380","7,988,381")
    return seq(st,[("op1",op1),("op2",op2)],colw=98,
        aria="Operator sequence: both operators committed roots for epochs 304, 305 and 309, missed epoch 308, and produced a zero-leaf artifact for epoch 310.")

def fig_contract():
    st=[("impl deployed","§6.4"),("impl activated","§6.4"),("capture e308","§8.3"),("roots e309","§8.2"),
        ("entitlement e309","§8.3"),("capture e310","§8.3"),("finalize e310","§5.2"),
        ("state read","§6.1"),("16 ClaimPaid","§8.3"),("state read","§6.1")]
    row=[C("chain","deploy","7,986,577","code 0xc8837d…"),
         C("chain","Upgraded","7,986,580","→ 0x40e5ab…"),
         C("chain","+103.32 α","7,988,077","2 pools"),
         C("chain","2 roots","7,988,380","bound by hash"),
         C("chain","funded","7,988,527","expiry 7,991,073"),
         C("chain","0 α","7,988,677","verified zero"),
         C("chain","RootMissed ×2","7,988,827","carried 0"),
         C("chain","liability","7,990,697","103,320,655,354"),
         C("chain","paid out","7,990,845–897","103.320655346 α"),
         C("chain","conserved","7,990,906","residue 8 rao")]
    return seq(st,[("coordinator + vault",row)],colw=104,
        aria="Contract sequence: one positive capture, one funded entitlement, sixteen payments, conservation holding.")

def fig_miner():
    st=[("in artifact e309","§8.2"),("root committed","§11.2"),("entitlement","§8.3"),
        ("claim 1","§11.2"),("claim 2",""),("claim 3",""),("claim 4",""),
        ("claim 5",""),("claim 6",""),("claim 7",""),("claim 8","")]
    p1=[C("chain","8 leaves","7,988,374","Σ 10,000 bps"),
        C("chain","root bound","7,988,380","0x9bbab5…"),
        C("chain","51.653 α","7,988,527","funded"),
        C("chain","m-108","7,990,845","6.492811278 α"),
        C("chain","m-153","7,990,858","6.270702380 α"),
        C("chain","m-242","7,990,864","6.544464510 α"),
        C("chain","m-441","7,990,870","6.513472571 α"),
        C("chain","m-467","7,990,876","6.477315309 α"),
        C("chain","m-489","7,990,882","6.487645955 α"),
        C("chain","m-779","7,990,888","6.446323369 α"),
        C("chain","m-817","7,990,894","6.420496753 α")]
    p2=[C("chain","8 leaves","7,988,374","Σ 10,000 bps"),
        C("chain","root bound","7,988,380","0xdc36a9…"),
        C("chain","51.667 α","7,988,527","funded"),
        C("chain","m-15","7,990,855","6.463594645 α"),
        C("chain","m-173","7,990,861","6.484261614 α"),
        C("chain","m-262","7,990,867","6.396426995 α"),
        C("chain","m-350","7,990,873","6.396426995 α"),
        C("chain","m-399","7,990,879","6.572096234 α"),
        C("chain","m-432","7,990,885","6.546262522 α"),
        C("chain","m-902","7,990,891","6.463594645 α"),
        C("chain","m-930","7,990,897","6.344759571 α")]
    return seq(st,[("p1 · pool UID 3",p1),("p2 · pool UID 4",p2)],colw=96,
        aria="Miner sequence: eight providers per pool, each paid the exact share its artifact leaf allocated.")

# ---------------- settlement lifecycle strip ----------------
def strip_settlement():
    E=EVENTS
    def pick(kind, ep, no=None):
        return [e for e in E if e["kind"]==kind and e["epoch"]==ep and (no is None or e["no"]==no)]
    eps=list(range(302,317))
    head=[("Epoch",""),("deposit for e","§7.5"),("root committed","§8.2 · ≤ end+50"),
          ("capture at boundary","§8.3"),("entitlement","§8.3 · end+150"),("claims","§8.3 / §11.2")]
    s=['<div class="scroll-x"><table class="strip" style="min-width:880px"><thead><tr>']
    for h,sub in head:
        s.append(f'<th>{esc(h)}{f"<span>{esc(sub)}</span>" if sub else ""}</th>')
    s.append('</tr></thead><tbody>')
    for ep in eps:
        dep=pick("deposit",ep); rc=pick("rootcommit",ep); cap=pick("capture",ep)
        ent=pick("entitlement",ep); rm=pick("rootmissed",ep)
        clm=[e for e in E if e["kind"]=="claimed" and e["epoch"]==ep]
        cells=[]
        # deposit
        if dep: cells.append((ev("chain",False),f'{len(dep)} Deposit · {fmt(dep[0]["block"])}',False))
        else:   cells.append((ev("none",False),"none for this epoch",False))
        # root
        if rc:  cells.append((ev("chain",False),f'2 roots · {fmt(rc[0]["block"])} · +{rc[0]["block"]-(7983577+300*(ep-293)):d} blk',False))
        else:   cells.append((ev("fail",False),"no root committed",True))
        # capture
        if cap:
            tot=sum(c["amount"] or 0 for c in cap)
            cells.append((ev("chain",False), (f'{tot:,} alpha-rao' if tot else "0 — verified zero")+f' · {fmt(cap[0]["block"])}', False))
        else: cells.append((ev("none",False),"—",False))
        # entitlement
        if ent:
            tot=sum(x["amount"] or 0 for x in ent)
            cells.append((ev("chain",False),f'{"funded " if tot else "zero "}· {fmt(ent[0]["block"])}',False))
        elif rm: cells.append((ev("chain",False),f'RootMissed ×{len(rm)} · carried 0',False))
        else: cells.append((ev("none",False),"—",False))
        # claims
        if clm:
            paid=[c for c in clm if (c["amount"] or 0)>0]
            cells.append((ev("chain",False),f'{len(clm)} accepted · {len(paid)} paid',False))
        else: cells.append((ev("none",False),"—",False))
        # break propagation
        broke=False; out=[]
        for tok,txt,isbrk in cells:
            if broke: out.append((ev("none",False),"—",False))
            else:
                out.append((tok,txt,isbrk))
                if isbrk: broke=True
        s.append(f'<tr><td class="sticky">e{ep}</td>')
        for tok,txt,isbrk in out:
            s.append(f'<td{" class=\'brk\'" if isbrk else ""}>{tok} {esc(txt)}</td>')
        s.append('</tr>')
    s.append('</tbody></table></div>')
    return "".join(s)

def strip_weights():
    rows=[("V1",[("1401",None),("1403","fail|cut mismatch, record 48613"),("1404",None),("1405",None),
                 ("after","fail|restarts exhausted 5 of 5")]),
          ("V2 · UID255",[("1401","chain|incl 7,986,181 · applied 7,986,519"),
                 ("1403","artifact|input sealed at cut 7,986,866"),
                 ("1404","chain|incl 7,987,311 · applied 7,987,601"),
                 ("1405","chain|incl 7,987,774 · applied 7,988,052"),
                 ("after","fail|restarts exhausted 5 of 5")])]
    s=['<div class="scroll-x"><table class="strip" style="min-width:760px"><thead><tr><th>Validator<span>§5.1 each tempo, every validator submits commit-reveal weights</span></th>']
    for c in ["native 1401","native 1403","native 1404","native 1405","after 1405"]:
        s.append(f'<th>{c}</th>')
    s.append('</tr></thead><tbody>')
    for tag,cells in rows:
        s.append(f'<tr><td class="sticky">{esc(tag)}</td>')
        for _n,v in cells:
            if v is None:
                s.append(f'<td>{ev("none",False)} not applied</td>')
            else:
                k,t=v.split("|",1)
                s.append(f'<td>{ev(k,False)} {esc(t)}</td>')
        s.append('</tr>')
    s.append('</tbody></table></div>')
    return "".join(s)

# ---------------- settlement chain of custody ----------------
def fig_chain():
    steps=[("Provider usage","off-chain","8 providers per pool,\nusage + reliability","artifact",
            "artifact providers[]"),
           ("Leaf shares","off-chain","share_bps, Σ = 10,000\nlargest-remainder","chain",
            "recomputed from usage"),
           ("Merkle root","local","keccak-256 OZ double hash,\nsorted-pair fold","chain",
            "rebuilt independently"),
           ("Root committed","on-chain","OperatorRootCommitted\nby registered rootSigner","chain",
            "block 7,988,380"),
           ("Entitlement","on-chain","EntitlementFinalized\n51.653 + 51.667 α","chain",
            "block 7,988,527"),
           ("Claim + proof","on-chain","Merkle proof verified\nby the vault","chain",
            "16 of 16 proofs"),
           ("Payment","on-chain","ClaimPaid = floor(bps × total / 10⁴)\n103.320655346 α","chain",
            "blocks 7,990,845–897"),
           ("Conservation","on-chain","residue 8 rao = liability\nconservationHolds()","chain",
            "block 7,990,906")]
    n=len(steps); CW=134; W=16+CW*n; H=190
    s=[f'<svg class="fig" viewBox="0 0 {W} {H}" style="min-width:{W}px" role="img" '
       f'aria-label="Chain of custody from off-chain provider usage to on-chain payment and conservation">']
    s.append('<style>.st{font:600 11.5px var(--font-sans);fill:var(--ink-1)}'
      '.sk{font:9px var(--font-mono);fill:var(--ink-3)}'
      '.sd{font:10px var(--font-sans);fill:var(--ink-2)}'
      '.sv{font:9px var(--font-mono);fill:var(--ink-3)}'
      '.bx{fill:var(--surface-2);stroke:var(--rule-2);stroke-width:1}'
      '.ar{stroke:var(--ink-2);stroke-width:1;fill:none}</style>')
    for i,(t,kind,desc,tok,note) in enumerate(steps):
        x=8+CW*i
        s.append(f'<rect class="bx" x="{x}" y="30" width="{CW-18}" height="96" rx="3"/>')
        s.append(f'<text class="sk" x="{x+8}" y="22">{esc(kind)}</text>')
        s.append(f'<text class="st" x="{x+8}" y="48">{esc(t)}</text>')
        for j,ln in enumerate(desc.split("\n")):
            s.append(f'<text class="sd" x="{x+8}" y="{66+j*13}">{esc(ln)}</text>')
        s.append(f'<g transform="translate({x+8},98)"><use href="#ev-{tok}" width="14" height="14"/></g>')
        s.append(f'<text class="sv" x="{x+26}" y="109">{esc(note)}</text>')
        if i<n-1:
            s.append(f'<path class="ar" d="M{x+CW-18} 78 L{x+CW-3} 78" marker-end="url(#arrow)"/>')
    s.append(f'<text class="sv" x="8" y="150">Each arrow is a binding a third party can re-derive: '
             f'the hash committed on chain fixes the artifact bytes, the artifact bytes fix the leaves, '
             f'the leaves fix the root, and the root plus the on-chain entitlement fix every payment amount.</text>')
    s.append(f'<text class="sv" x="8" y="166">Reproduced here for all 16 payments with an independent '
             f'keccak-256 and secp256k1 implementation — no library from the project under test.</text>')
    s.append("</svg>")
    return "".join(s)
