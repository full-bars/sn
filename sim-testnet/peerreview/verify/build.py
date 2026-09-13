# -*- coding: utf-8 -*-
import os
_HERE = os.path.dirname(os.path.abspath(__file__))
_REPO = os.path.abspath(os.path.join(_HERE, "..", "..", ".."))
import json, datetime, sys
sys.path.insert(0,".")
from gen_css import CSS, SPRITE, ev, legend_line, EVN
from gen_fig import (master_ruler, fig_validator, fig_operator, fig_contract, fig_miner,
                     strip_settlement, strip_weights, esc, fmt, EVENTS)
from content import MINER, VALIDATOR, OPERATOR, CONTRACT

R = json.load(open("results.json"))
CH = R["checks"]; M = R["meta"]
npass = sum(1 for c in CH if c["pass"]); ntot = len(CH)
def byactor(a): return [c for c in CH if c["actor"]==a]
GEN = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d %H:%M UTC")

O=[]
def w(x): O.append(x)

w(CSS); w(SPRITE)
w('<header class="topbar"><span class="topbar__title">SN sim-testnet · independent verification of '
  '<code>sim-testnet/FINAL.md</code> against Bittensor testnet netuid 521</span>'
  f'<span class="ev ev--fail"><svg class="ev__icon"><use href="#ev-fail"/></svg>'
  f'<span class="ev__label"><code>final_acceptance = false</code></span></span></header>')
w('<main class="page">')

# ---------------- front ----------------
w('<section id="front">')
w('<h1>Verification of the sim-testnet final report</h1>')
w('<p class="lead prose">This document re-derives the factual claims in <code>sim-testnet/FINAL.md</code> '
  'directly from the Bittensor testnet chain the exercise ran on — subnet <strong>netuid 521</strong>, '
  'EVM chain <strong>945</strong> — and maps the observed behaviour, clause by clause, to '
  '<code>WHITEPAPER.md</code>. It is written for the subnet owners to put in front of the owners of '
  'Bittensor. Nothing below is taken from the report on trust: every number was read back from the node '
  'or recomputed locally.</p>')
w(f'<p class="prose"><strong>Headline.</strong> The report\'s on-chain claims hold. '
  f'Of <strong>{ntot}</strong> independently testable assertions, <strong>{npass}</strong> reproduce exactly '
  f'and <strong>{ntot-npass}</strong> do not — and all three failures are substantive findings about the '
  'deployment, not arithmetic errors in the report. The settlement mechanism the whitepaper specifies was '
  'demonstrated end to end on a real chain: sixteen provider payments, every amount forced by the '
  'operator\'s committed Merkle root, with conservation holding to the alpha-rao. The load-bearing checks '
  'were re-run against a <strong>public Bittensor endpoint outside the subnet owners\' control</strong> '
  'and reproduced identically (§8.1), so the chain facts here do not rest on the owners\' own node.</p>')
w('<p class="prose">The run as a whole did not pass, and should not be presented as if it had. One '
  'validator never applied a weight vector, so decentralised weight-setting was never exercised. '
  'The only epoch that ever captured positive emission missed its payout root. And the cadence the '
  'whitepaper names for testnet acceptance was never scheduled on chain, so its stated acceptance bar — '
  'three consecutive fully observed epochs — was not cleared. '
  '<code>final_acceptance = false</code> is the correct verdict and the report states it.</p>')

w('<h3>Run identity</h3>')
w('<div class="scroll-x"><table style="min-width:560px"><tbody>')
for k,v in [("Subnet","netuid <strong>521</strong> — confirmed from <code>vault.netuid()</code> and <code>coordinator.netuid()</code>"),
            ("EVM chain","<code>945</code> (<code>0x3b1</code>)"),
            ("Chain",f'<code>{esc(M["system_chain"])}</code> · Subtensor Node'),
            ("Endpoints read",f'<code>{esc(M["endpoint"])}</code> (owners\' node, <code>nodeRoles=["Full"]</code>)<br><code>test.finney.opentensor.ai</code> (public, Opentensor-operated — independent re-check, §8.1)'),
            ("Genesis",f'<code class="hash">{esc(M["genesis"])}</code>'),
            ("Coordinator (proxy, upgradeable)","<code>0x8e7d2f9a77fec95c7e4875b0bd858d5de2b6def8</code>"),
            ("Settlement vault (immutable)","<code>0x09d5d7a5c3e94b6ae42b09889a1cee50f970fc5e</code>"),
            ("Reserve sink (immutable)","<code>0x376f98bd7c6b334f7f1cb2685E0970a18bfe7d28</code>"),
            ("On-chain run window","blocks <span class='blk'>7,983,577</span> – <span class='blk'>7,990,906</span> · 2026-09-11 16:46Z – 2026-09-12 17:12Z (7,329 blocks, 24.4 h)"),
            ("Node head at verification",f'<span class="blk">{fmt(M["head"])}</span>'),
            ("Verification performed",GEN)]:
    w(f'<tr><td style="width:230px;color:var(--ink-3)">{k}</td><td>{v}</td></tr>')
w('</tbody></table></div>')

w('<h3>How to read this</h3>')
w('<p class="prose">Every claim carries an evidence token. The tokens are distinguished by '
  '<em>shape and glyph</em> as well as colour, so the document survives greyscale printing. '
  'A verified success and a recorded failure are drawn at identical size and weight, and appear in the '
  'same tables — failures are never relegated to a footnote.</p>')
w('<div class="scroll-x"><table class="legend-tbl" style="min-width:620px"><tbody>')
for k,d in [("chain","We read the node ourselves — receipt, extrinsic, storage, or historical state call — and it matches the claim."),
            ("artifact","A retained, hash-pinned artifact supports the claim; the chain cannot independently confirm it."),
            ("asserted","Stated in the report; no artifact or chain read available to us."),
            ("contra","Our evidence disagrees with the claim, or with the specification it cites."),
            ("fail","A required behaviour did not occur. The failure is itself the evidenced fact."),
            ("none","The step was not attempted, or the path was never exercised. Distinct from failure.")]:
    w(f'<tr><td style="width:190px">{ev(k)}</td><td style="color:var(--ink-2)">{esc(d)}</td></tr>')
w('</tbody></table></div>')
w('</section>')

# ---------------- verification result ----------------
w('<section id="result"><h2>1. Verification result</h2>')
from collections import Counter
cnt=Counter(e["evidence"] for e in EVENTS)
w('<div class="stats">')
for lab,val,note,tok in [
  ("Assertions tested",str(ntot),"drawn from FINAL.md","chain"),
  ("Reproduced exactly",str(npass),"re-derived from chain or local crypto","chain"),
  ("Did not reproduce",str(ntot-npass),"all three are findings, below","contra"),
  ("On-chain events indexed",str(len(EVENTS)),"complete contract log history","chain"),
  ("Evidence bundles","6 of 6","SHA-256 matches file on disk","chain"),
  ("Run verdict","false",'<code>final_acceptance</code> — concurred',"fail")]:
    w(f'<div class="stat"><div class="stat__label">{esc(lab)}</div>'
      f'<div class="stat__value">{esc(val)} {ev(tok,False)}</div>'
      f'<div class="stat__note">{note}</div></div>')
w('</div>')

w('<h3>What was checked, and how</h3>')
w('<div class="scroll-x"><table style="min-width:760px"><thead><tr>'
  '<th>Class of claim</th><th>Independent check performed</th><th>What it does <em>not</em> prove</th></tr></thead><tbody>')
for a,b,c in [
 ("EVM transaction","Receipt re-fetched; <code>status</code>, block height and block hash compared.",
  "Nothing about intent or off-chain correctness. Inclusion is not finality — that needs the separate canonical-hash check."),
 ("Event amounts","Logs re-decoded from the committed ABIs; every field compared to the report's tables.",
  "That the amount was <em>economically</em> correct — only that the contract emitted it."),
 ("Contract state","<code>eth_call</code> replayed at the exact historical block the report pins.",
  "Anything about blocks the report does not pin."),
 ("Native extrinsic","Every extrinsic in the claimed block hashed with BLAKE2b-256 and matched.",
  "Who authored it, beyond the signature the runtime accepted."),
 ("Substrate storage","Storage keys rebuilt from twox128 locally; values decoded from raw SCALE.",
  "State at blocks the node has pruned."),
 ("Off-chain artifact","Content hash recomputed from canonical JSON; signature recovered with a local secp256k1 implementation; Merkle root rebuilt with a local keccak-256.",
  "That the usage figures inside the artifact are true. The chain binds the artifact's <em>hash</em>, not its honesty."),
 ("Financial totals","Recomputed from the individual events and reconciled against the contract's own lifetime counters.",
  "Costs outside the vault — gas, registrations and transfers are accounted separately and less completely."),
 ("Test-suite / process claims","Not independently verifiable from chain state.",
  "These rest entirely on the run's own receipts and are marked as such.")]:
    w(f'<tr><td style="width:150px">{a}</td><td>{b}</td><td style="color:var(--ink-2)">{c}</td></tr>')
w('</tbody></table><caption>Every check in this document reproduces the claim from a source '
  'independent of the report\'s prose. Where that is impossible, the claim is marked '
  '<em>artifact only</em> or <em>asserted</em> rather than verified.</caption></div>')

w('<h3>Conservation identities</h3>')
w('<p class="prose">These are the arithmetic identities a reader can recompute from the numbers in '
  'this document. All four hold exactly.</p>')
w('<div class="scroll-x"><table style="min-width:700px"><thead><tr><th>Identity</th><th>Evaluated</th><th></th></tr></thead><tbody>')
for idn,val,tok in [
 ("Σ 16 <code>ClaimPaid</code> amounts = contract <code>totalPaid()</code>","103,320,655,346 = 103,320,655,346","chain"),
 ("<code>totalCaptured</code> − <code>totalPaid</code> = <code>outstandingLiability</code>","103,320,655,354 − 103,320,655,346 = 8","chain"),
 ("<code>outstandingLiability</code> = <code>escrowAccounted</code>, <code>conservationHolds</code> true","8 = 8, true","chain"),
 ("Σ per-operator floor-rounding residue = outstanding residue","5 + 3 = 8 alpha-rao","chain"),
 ("Σ (gasUsed × effectiveGasPrice) over 16 claims = reported fee","49,882,627,183,941,739 wei = 0.049882627183941739 TAO","chain"),
 ("Σ leaf <code>share_bps</code> per operator = 10,000","10,000 and 10,000","chain")]:
    w(f'<tr><td>{idn}</td><td class="mono">{val}</td><td style="width:180px">{ev(tok)}</td></tr>')
w('</tbody></table></div>')
w('</section>')

# ---------------- master timeline ----------------
w('<section id="timeline"><h2>2. The run, to scale</h2>')
w('<p class="prose">The on-chain exercise occupied 7,329 blocks — 24.4 hours at the measured '
  '12.000-second cadence, from 2026-09-11 16:46Z to 2026-09-12 17:12Z — although the substantive '
  'settlement activity falls inside about 14.5 hours on 2026-09-12. The epoch grid behind the marks is the contract\'s own 300-block settlement epoch, '
  'measured across 24 consecutive epochs rather than assumed.</p>')
w('<figure><div class="scroll-x">'+master_ruler()+'</div>')
w('<figcaption>Block-height ruler, linear and uncompressed. The vertical dashed red line is the '
  'fleet stop; the dashed grey line is the epoch-309 claim expiry, which every payment beat. '
  'The bottom rug plots one tick per indexed on-chain event — it is the honest density picture, '
  'and it shows how tightly the settlement and payment bursts cluster.'+legend_line()+'</figcaption></figure>')
w(f'<p class="tally">{len(EVENTS)} indexed on-chain events · '
  + " · ".join(f"{k} {v}" for k,v in sorted(cnt.items())) + '</p>')

w('<h3>Settlement lifecycle, epoch by epoch</h3>')
w('<p class="prose">The whitepaper\'s §5.2 lifecycle is: deposit → traffic → close → root commit '
  '→ capture at the boundary → entitlement → claim. The strip below shows where each epoch\'s chain '
  'completed and where it broke. A red right edge marks the break; everything to its right in that row '
  'was never reached. A verified zero is shown as verified evidence, not as a failure.</p>')
w('<figure>'+strip_settlement())
w('<figcaption>Epochs 302–316. Epoch 308 is the only epoch that ever captured positive emission — '
  'and it is one of the epochs for which no payout root was committed, so its value carried to '
  'epoch 309, which did commit roots and was funded. Epochs 311 onward ran after the fleet stopped: '
  'boundaries continued, roots did not. Offsets are measured against the contract\'s own '
  '<code>epochEndBlock()</code>.'+legend_line()+'</figcaption></figure>')
w('</section>')

from gen_fig import fig_chain
# ---------------- the settlement proof ----------------
w('<section id="settlement"><h2>3. The settlement mechanism, demonstrated end to end</h2>')
w('<p class="prose">This is the central result. The whitepaper\'s claim is that a provider is paid '
  '<code>amount = s(n,p) · poolTotal(n)</code> — a deterministic function of an immutable on-chain '
  'entitlement and the operator\'s committed share — and that <em>the operator never holds anyone '
  'else\'s funds</em> (§8.3). We reproduced that entire chain independently.</p>')
w('<figure><div class="scroll-x">'+fig_chain()+'</div>'
  '<figcaption>Chain of custody for epoch 309. Every link was re-derived locally: the Merkle roots were '
  'rebuilt from the artifact leaves with our own keccak-256, the artifact signatures recovered with our own '
  'secp256k1, and each payment amount recomputed from the leaf share and the on-chain entitlement.'
  +legend_line()+'</figcaption></figure>')
w('<p class="prose">Two consequences are worth stating plainly to a Bittensor reviewer. First, the '
  '<strong>payout arithmetic is not discretionary</strong>: given the committed root and the captured '
  'emission, every one of the sixteen amounts is forced, and all sixteen matched to the alpha-rao. '
  'Second, the <strong>rounding residue is accounted, not lost</strong> — the 5 and 3 alpha-rao that '
  'floor-rounding leaves behind sum to exactly the 8 alpha-rao the vault still reports as outstanding '
  'liability and accounted escrow, and the contract\'s own <code>conservationHolds()</code> returns true.</p>')
w('<div class="scroll-x"><table style="min-width:820px"><thead><tr><th>Operator</th><th>Entitlement (alpha-rao)</th>'
  '<th>Committed payout root</th><th>Artifact content hash</th><th>Σ leaf bps</th><th>Paid</th><th>Residue</th></tr></thead><tbody>')
for no,tot,root,ah,paid,res in [
 (1,"51,653,232,130","0x9bbab5462796712a3af4dafc5b32086842d7a72eca97b263154c8dbc8f32262b",
     "c5fe8a8e28157016987ed64171d62a63f98bf4007a55065a9f0f6f02204fecef","51,653,232,125","5"),
 (2,"51,667,423,224","0xdc36a9982c1d53e90fdf5bb431fe92610a505dc92da07096bd3126d3748a6376",
     "e55d709f29f57ef75e4880e3e358f180720f8f4f58706e8aa5f6de676170e707","51,667,423,221","3")]:
    w(f'<tr><td>NO{no} · pool UID {no+2}</td><td class="amt">{tot}</td>'
      f'<td><code class="hash">{root[:14]}…{root[-6:]}</code></td>'
      f'<td><code class="hash">{ah[:12]}…{ah[-6:]}</code></td><td class="amt">10,000</td>'
      f'<td class="amt">{paid}</td><td class="amt">{res}</td></tr>')
w('</tbody></table><caption>Roots rebuilt locally match both the artifact and the chain. '
  'Content hashes recompute from canonical JSON and equal the <code>artifactHash</code> committed on chain.</caption></div>')

w('<h3>3.1 The missed-root carry — the link the report does not narrate</h3>')
w('<p class="prose">There is an apparent contradiction in the chain data that is worth resolving '
  'explicitly, because a careful reviewer will find it and because FINAL.md does not address it: '
  '<strong>epoch 309 captured zero emission, yet epoch 309 is the epoch that paid out 103.32 α.</strong> '
  'The money came from epoch 308 via the whitepaper\'s same-operator carry.</p>')
w('<div class="scroll-x"><table style="min-width:800px"><thead><tr><th style="width:92px">Block</th>'
  '<th>Event</th><th>NO1 (alpha-rao)</th><th>NO2 (alpha-rao)</th><th><code>carry()</code> after</th></tr></thead><tbody>')
for bn,evt,a,b,car in [
 ("7,988,077","<code>EmissionCaptured</code> epoch 308","51,653,232,130","51,667,423,224","0 — root still open"),
 ("7,988,227","<code>RootMissed</code> epoch 308 — no root committed","51,653,232,130 carried","51,667,423,224 carried","full captured amount"),
 ("7,988,377","<code>EmissionCaptured</code> epoch 309","0","0","unchanged"),
 ("7,988,380","<code>OperatorRootCommitted</code> epoch 309","root committed","root committed","unchanged"),
 ("7,988,527","<code>EntitlementFinalized</code> epoch 309","51,653,232,130","51,667,423,224","0 — carry consumed")]:
    w(f'<tr><td class="blk">{bn}</td><td>{evt}</td><td class="amt">{a}</td><td class="amt">{b}</td>'
      f'<td style="color:var(--ink-2)">{car}</td></tr>')
w('</tbody></table><caption>Read directly from the vault\'s <code>carry(noId)</code> getter at four '
  'historical blocks, plus the event payloads. This is whitepaper §8.3 — '
  '<code>poolTotal(n) = captured_emission(n) + same_operator_carry(n)</code> — executing exactly as written.'
  '</caption></div>')
w('<p class="prose">Two properties of this are worth putting in front of a Bittensor reviewer. '
  'The carry is <strong>value-preserving</strong>: not one alpha-rao of the captured emission was lost to '
  'the missed root. And it is <strong>operator-isolated</strong>: NO1\'s carry reached only NO1\'s '
  'entitlement and NO2\'s only NO2\'s, which is precisely the isolation §8.3 promises so that one '
  'operator\'s failure cannot socialise onto another\'s providers. Both were verified by reading the '
  'per-operator carry counter before and after each transition, not inferred from the narrative.</p>')
w(f'<p class="prose">{ev("contra")} That said, FINAL.md never mentions <code>RootMissed(308)</code> or the '
  'carry at all. Its tables jump from the epoch-308 capture to the epoch-309 entitlement without the step '
  'that connects them. The underlying facts are right and the mechanism behaved correctly — but the '
  'omission leaves the report\'s central financial claim looking unexplained, and it should be added '
  'before this is presented externally.</p>')
w('</section>')

# ---------------- actors ----------------
ACTORS = [
 ("miner","MI","Miner (providers)","§3 · §8.2 · §8.3 · §11.2",
  "A provider is a <code>client_id</code> inside a network operator's pool — not a subnet UID. It carries "
  "traffic and claims its α directly from the contract with a Merkle proof against its operator's "
  "committed payout root. It is the on-ramp tier; the head tier holds its own UID and is paid natively.",
  ["p1 · pool UID 3","p2 · pool UID 4","16 paid coldkeys"], fig_miner, MINER,
  [("Providers paid","16","8 per pool, distinct coldkeys","chain"),
   ("Total paid","103.320655346 α","reproduced from leaf shares","chain"),
   ("Merkle proofs","16 of 16","verified against rebuilt root","chain"),
   ("Amount formula","exact","floor(bps × total / 10,000)","chain")],
  "Every payment reproduced exactly from the off-chain artifact and on-chain entitlement. "
  "The identity defect that caused the original payout failure — each daemon using its operator's shared "
  "network JWT, which resolved the last shared wallet rather than the individual provider client — is "
  "recorded in the report and was corrected before these claims were made; that defect and its recovery "
  "are off-chain facts we cannot independently replay."),
 ("validator","VA","Validator","§3 · §5.1 · §9 · §10 · §15.1",
  "An independent Bittensor validator UID staking its own α. Each tempo it scores every operator pool "
  "<code>implied_usage × quality</code> and every head fleet on routable-IP breadth, and submits one "
  "commit-reveal weight vector. Native Yuma dividends are its only reward; the contract custodies nothing for it.",
  ["V1 — no applied vector","V2 · UID 255","reserve hotkey · UID 254"], fig_validator, VALIDATOR,
  [("Applied vectors, V2","3","1401 · 1404 · 1405","chain"),
   ("Applied vectors, V1","0","no receipt at any block","fail"),
   ("θ head/tail split","0.29999947","governed θ = 3/10","chain"),
   ("Reserve share","61.449%","≥ 60% floor, < 65% target","chain")],
  "The one applied vector that carried both channels splits head against pool at θ = 3/10 to better than "
  "one part in a million, and all three vectors respect the signed policy weight cap. That is strong "
  "evidence the deployed validator implements §10 as written. But weight-setting is specified as "
  "<em>decentralised across independent validators</em>, and only one validator ever applied a vector — "
  "so Yuma consensus, clipping and vtrust were never exercised against a second independent scorer. "
  "This is the single largest gap between the exercise and the specification."),
 ("operator","OP","Network operator","§3 · §5.2 · §7.5 · §8.2 · §11.1",
  "A contract registration (<code>noId</code>) with one miner-pool UID owned outright by the immutable "
  "vault. It deposits conviction stake, runs the verify server, and commits the Merkle payout root that "
  "splits its pool. It directs flow; it holds no emission.",
  ["op1 · pool UID 3","op2 · pool UID 4","2 registered, both active"], fig_operator, OPERATOR,
  [("Roots committed","24","12 epochs × 2 operators","chain"),
   ("Root window","+6 blocks","enforced window is +0…+50","chain"),
   ("Deposits published","10","each with policy hash + nonce","chain"),
   ("Roots missed","67 events","including epoch 308","fail")],
  "Both operators met the on-chain root-commit window on every epoch they committed for, and the "
  "coordinator enforces both the window and the authorised signer — a root cannot be committed late or "
  "by the wrong key. The failures are real and symmetric: both operators missed epoch 308, the only "
  "epoch that captured positive emission, and both produced a zero-leaf artifact for epoch 310."),
 ("contract","CT","ST contract set","§3 · §6.1 · §6.3 · §6.4 · §8.3",
  "Three distinct mapped coldkeys: an upgradeable policy coordinator, an immutable settlement vault that "
  "owns the pool UIDs and claim escrow, and an immutable reserve sink that owns permanently locked "
  "conviction. The contract computes no weight; it custodies and settles only.",
  ["coordinator 0x8e7d2f…def8","vault 0x09d5d7…fc5e","reserve sink 0x376f98…7d28"], fig_contract, CONTRACT,
  [("Vault upgradeable","no","zero impl slot, zero Upgraded","chain"),
   ("Coordinator upgrades","9","last activated 7,986,580","chain"),
   ("Positive captures","1","epoch 308, whole lifetime","chain"),
   ("Conservation","holds","residue 8 rao accounted","chain")],
  "The governance claim that matters most to a reviewer — that finalized claims are un-clawbackable even "
  "by a hostile coordinator upgrade — checks out: the vault and reserve sink carry real code, a zero "
  "EIP-1967 implementation slot, and no <code>Upgraded</code> event in their entire history, while the "
  "coordinator is a proxy that has in fact been upgraded nine times."),
]

w('<section id="actors"><h2>4. Evidence by actor</h2>')
w('<p class="prose">Each actor is assessed against the whitepaper clauses it is responsible for. '
  'The sequence timelines are ordered by stage, not drawn to time — block heights are printed under each '
  'event so the real spacing is never implied by the geometry.</p>')
for slug,mono,name,wp,role,chips,figfn,table,stats,verdict in ACTORS:
    w(f'<div class="actor" id="actor-{slug}">')
    w(f'<div class="actor__head"><div class="actor__name"><span class="actor__mono">{mono}</span>{esc(name)}</div>'
      f'<div class="actor__wp">{esc(wp)}</div></div>')
    w(f'<p class="actor__role">{role}</p>')
    w('<div class="chips">'+"".join(f'<span class="chip">{esc(c)}</span>' for c in chips)+'</div>')
    w('<div class="stats">')
    for lab,val,note,tok in stats:
        w(f'<div class="stat"><div class="stat__label">{esc(lab)}</div>'
          f'<div class="stat__value">{esc(val)} {ev(tok,False)}</div>'
          f'<div class="stat__note">{esc(note)}</div></div>')
    w('</div>')
    w('<figure><div class="scroll-x">'+figfn()+'</div>'
      f'<figcaption>Sequence order, not time; block heights are printed under each event. '
      f'A short dash on the spine means the stage never became applicable to that instance; a dashed '
      f'spine after a failure token means activity ended there.'+legend_line()+'</figcaption></figure>')
    w('<h4>Whitepaper conformance</h4>')
    w('<div class="scroll-x"><table style="min-width:820px"><thead><tr><th style="width:74px">Clause</th>'
      '<th style="width:26%">Requirement</th><th>Observed</th><th style="width:150px">Evidence</th></tr></thead><tbody>')
    for ref,req,obs,tok,how in table:
        w(f'<tr><td class="mono">{esc(ref)}</td><td>{esc(req)}</td>'
          f'<td>{esc(obs)}<div class="meta" style="margin-top:4px">{esc(how)}</div></td>'
          f'<td>{ev(tok)}</td></tr>')
    w('</tbody></table></div>')
    w(f'<div class="note"><strong>Assessment.</strong> {verdict}</div>')
    w('</div>')
w('</section>')

# ---------------- findings ----------------
w('<section id="findings"><h2>5. Findings</h2>')
w('<p class="prose">Three assertions did not reproduce. None is an arithmetic error in the report; '
  'each is a real gap between the deployment and the specification, and the first is the most '
  'consequential for a reader assessing testnet acceptance.</p>')

w('<h3>F1 — The whitepaper\'s testnet acceptance cadence was never scheduled on chain</h3>')
w('<div class="contra"><div class="contra__cell contra__cell--claim">'
  f'<div class="contra__head">{ev("contra")} WHITEPAPER.md §5</div>'
  '<p>"Testnet acceptance uses a deliberately shortened, future-effective <strong>360-block</strong> UR epoch '
  '(approximately 72 minutes), a <strong>60-block</strong> root window, a <strong>180-block</strong> finalize '
  'offset, and a <strong>6-block</strong> close grace. It must complete <strong>three consecutive fully '
  'observed epochs</strong>."</p></div><div class="contra__arrow" aria-hidden="true">→</div>'
  f'<div class="contra__cell contra__cell--obs"><div class="contra__head">{ev("chain")} Observed on netuid 521</div>'
  '<p>The coordinator has carried exactly <strong>two</strong> policy versions in its entire history '
  '(<code>policyCount() == 2</code>), both at <strong>300 / 50 / 150 / 5</strong> — the accelerated '
  'bootstrap cadence. Measured epoch length across epochs 293–316 is exactly 300 blocks with a 150-block '
  'finalize offset. The 360-block production cadence was never scheduled.</p></div></div>')
w('<p class="prose">The deployment\'s own <code>deploy/testnet/policy-v1.yml</code> is consistent with this: '
  'it carries a <code>production_cadence</code> block of 360 / 60 / 180 / 6 to be scheduled for '
  '<code>currentEpoch+1</code> only <em>after the five accelerated epochs reconcile</em>. That transition '
  'never happened. The state machine exercised is the same one; the cadence is not the one the whitepaper '
  'names for acceptance, and the three-consecutive-fully-observed-epochs condition was therefore not met '
  'at the stated cadence. The report does not claim otherwise — it reports '
  '<code>final_acceptance=false</code> — but the whitepaper\'s acceptance condition and the exercise\'s '
  'actual configuration should not be conflated when this is presented.</p>')
w('<p class="prose">For completeness: at the accelerated cadence, epochs <strong>302–307</strong> form six '
  'consecutive epochs in which both operators committed roots and both entitlements were finalized. '
  'Those six carried zero value, because no positive emission was captured until epoch 308.</p>')

w('<h3>F2 — <code>max_allowed_validators</code> is 64, above the ≤ 56 target</h3>')
w(f'<p class="prose">{ev("contra")} Whitepaper §15.1 targets <strong>≤ 56</strong> so that roughly 200 '
  'top-level miner UIDs fit inside the 256-UID metagraph. The live value on netuid 521 is <strong>64</strong>. '
  'The deployment\'s own compatibility gate in <code>deploy/testnet/hyperparams.yml</code> records this '
  'explicitly as accepted for the bounded two-operator topology, with production governance required to '
  'lower it before targeting 200 head UIDs. It is a disclosed, deliberate deviation rather than a '
  'surprise — but it is untested at the density the whitepaper describes.</p>')

w('<h3>F3 — The reserve is below its 65% repair target</h3>')
w(f'<p class="prose">{ev("chain")} At finalized native block <span class="blk">7,992,355</span> the reserve '
  'hotkey at UID 254 holds <strong>49,410.992336897 α</strong> of the <strong>80,409.602927411 α</strong> '
  'held across all 256 UIDs — <strong>61.449%</strong>. That is above the configured 60% minimum and below '
  'the 65% repair target. The report states this accurately and does not claim the target is met. We '
  'decoded the full 256-UID census from raw Substrate storage independently, and all eight recorded '
  'storage queries — including a 278 KB Merkle read proof — replay byte-identical against the live node today.</p>')

w('<h3>Where the run itself failed</h3>')
w('<p class="prose">Distinct from the three findings above, these are failures the report records and '
  'that we confirmed on chain. They are the reason the correct verdict is provisional.</p>')
w('<div class="scroll-x"><table style="min-width:780px"><thead><tr><th style="width:34%">Failure</th>'
  '<th>Confirmed how</th><th style="width:150px">Evidence</th></tr></thead><tbody>')
for f,h,t in [
 ("Only one validator ever applied a weight vector.",
  "Scanning all 256 UID weight rows at each of the three application blocks returns exactly one row, UID 255.","fail"),
 ("Both validators exhausted their five supervisor restarts and the fleet was force-stopped.",
  "Not independently verifiable from chain state — recorded in the run's own supervisor state and stop receipt. No weight row appears after block 7,988,052.","artifact"),
 ("Epoch 308 — the only epoch with positive emission — had no payout root committed.",
  "Two <code>RootMissed</code> events at block 7,988,227; no <code>OperatorRootCommitted</code> for epoch 308.","chain"),
 ("Epoch 310's authoritative artifact had zero leaves, zero usage and an all-zero root; the commit was skipped.",
  "Two <code>EmissionCaptured</code> with amount 0 and two <code>RootMissed</code> with carried 0 in the finalization range; no claim events.","chain"),
 ("The full producer and aggregate release gates failed, and the full campaign is incomplete.",
  "Off-chain test-suite results. Not verifiable from chain state; they rest on the run's own receipts.","asserted"),
 ("Epochs 311–316 produced boundaries but no roots.",
  "Six further epochs of <code>RootMissed</code> after the fleet stop, visible in the lifecycle strip.","chain")]:
    w(f'<tr><td>{f}</td><td style="color:var(--ink-2)">{h}</td><td>{ev(t)}</td></tr>')
w('</tbody></table></div>')
w('<figure>'+strip_weights()+
  '<figcaption>Native weight lifecycle. V1\'s row is a line of failures and dashes; that is the finding.'
  +legend_line()+'</figcaption></figure>')
w('</section>')

# ---------------- corrections ----------------
w('<section id="corrections"><h2>6. On the report\'s own corrections</h2>')
w('<p class="prose">FINAL.md corrects several of its own earlier statements. Because a reader may reasonably '
  'ask whether the corrections are self-serving, we checked the two that change the headline result. Both '
  'corrections move the report <em>toward</em> the chain, and the corrected version is the accurate one.</p>')
w('<div class="contra"><div class="contra__cell contra__cell--claim">'
  f'<div class="contra__head">{ev("contra")} Superseded, earlier report</div>'
  '<p>Generalised epoch 310\'s zero result to the whole run — reporting zero captured emission and zero '
  'paid claims overall.</p></div><div class="contra__arrow" aria-hidden="true">→</div>'
  f'<div class="contra__cell contra__cell--obs"><div class="contra__head">{ev("chain")} Observed</div>'
  '<p>Epoch 308 captured 51,653,232,130 + 51,667,423,224 alpha-rao at block 7,988,077, and epoch 309 '
  'entitlements were funded at block 7,988,527. The correction is right and the earlier statement was wrong.</p>'
  '</div></div>')
w('<p class="prose">The second correction — that the payout failure was an identity-configuration defect, '
  'each daemon using its operator\'s shared network JWT rather than the individual provider credential — '
  'is an off-chain claim we cannot replay. What we can confirm is the outcome: all sixteen claims '
  'subsequently finalized before the expiry block, at amounts forced by the committed root.</p>')
w(f'<p class="prose">{ev("chain")} We also confirmed the report\'s wall-clock narrative against block '
  'timestamps. The pinned post-payment checkpoint at block <span class="blk">7,990,906</span> carries '
  'timestamp <strong>17:12:00Z</strong>, matching the report\'s "payment recovery completed 17:12 UTC"; '
  'block <span class="blk">7,992,355</span> carries <strong>22:01:48Z</strong> against the reported 22:02 '
  'reserve observation; and the epoch-309 artifact\'s <code>created_at</code> of <strong>08:45:36Z</strong> '
  'is exactly the timestamp of its epoch-end block <span class="blk">7,988,374</span>.</p>')
w('</section>')

# ---------------- evidence appendix ----------------
import txcheck
w('<section id="evidence"><h2>7. Evidence appendix</h2>')
w('<h3>7.1 Evidence bundles</h3>')
w('<p class="prose">Every SHA-256 the report cites for its evidence bundles matches the file on disk.</p>')
w('<div class="scroll-x"><table style="min-width:820px"><thead><tr><th>Bundle</th><th>SHA-256</th><th style="width:150px">Evidence</th></tr></thead><tbody>')
for f,h in [("epoch309-paid-claims-20260912.json","2aa27bfb3efa15bb87f93fc9fbe6adbd8d64a1e57e5ff68fab6d5a1a2deb18ec"),
            ("onchain-receipts-20260912.json","4e491a2d543e13c17ba1fdf44fb766bc2b3b838e9f4b2ea650ec3e47a7da59de"),
            ("reserve-alpha-20260912T220217Z.json","cf8963f7603cbb09909a36ea944004295dbe920dc36ec39030984642258ca061"),
            ("retained-ledger-capacity-20260912.json","e61e6d64347f8dde98d08d52464440a1a03104c874526e59a3c6056a453f3ea2"),
            ("short-run-corrections-20260912.json","e6372f5bd2bb5de16d4e911f8a1b2a254e8ac3c237c0ef3b7eb3c98f1f98e5f7"),
            ("native1404-1405-historical-weights.json","6373f2a7d8b2d6c8c1dcedcf28d0dba5ee6bb9d339d8fdcd9d66d7faaf102592")]:
    w(f'<tr><td><code>{esc(f)}</code></td><td><code class="hash">{h}</code></td><td>{ev("chain")}</td></tr>')
w('</tbody></table></div>')

w('<h3>7.2 Every transaction cited, in full</h3>')
w('<p class="prose">All 41 transactions below returned <code>status=0x1</code> at the block height the '
  'report claims, and every block hash the report pins is canonical at that height. '
  'The 16 claim transactions follow in 7.3.</p>')
w('<div class="scroll-x"><table style="min-width:900px"><thead><tr><th style="width:130px">Action</th>'
  '<th style="width:90px">Block</th><th>Transaction</th><th style="width:150px">Evidence</th></tr></thead><tbody>')
for label,h,bn,_bh in txcheck.TXS:
    w(f'<tr><td class="mono">{esc(label)}</td><td class="blk">{fmt(bn)}</td>'
      f'<td><code class="hash">{h}</code></td><td>{ev("chain")}</td></tr>')
w('</tbody></table></div>')

w('<h3>7.3 The sixteen epoch-309 payments</h3>')
w('<div class="scroll-x"><table style="min-width:900px"><thead><tr><th>Pool</th><th>Provider</th>'
  '<th style="width:64px">bps</th><th>Alpha-rao paid</th><th style="width:90px">Block</th>'
  '<th>Transaction</th></tr></thead><tbody>')
BPS={"miner-108":1257,"miner-15":1251,"miner-153":1214,"miner-173":1255,"miner-242":1267,"miner-262":1238,
     "miner-441":1261,"miner-350":1238,"miner-467":1254,"miner-399":1272,"miner-489":1256,"miner-432":1267,
     "miner-779":1248,"miner-902":1251,"miner-817":1243,"miner-930":1228}
for label,h,bn,amt in txcheck.CLAIMS:
    op,mn=label.split(".")
    w(f'<tr><td>{"p1" if op=="op1" else "p2"}</td><td class="mono">{esc(mn)}</td>'
      f'<td class="amt">{BPS[mn]}</td><td class="amt">{fmt(amt)}</td><td class="blk">{fmt(bn)}</td>'
      f'<td><code class="hash">{h}</code></td></tr>')
w('</tbody></table><caption>Each amount equals <code>floor(bps × entitlement / 10,000)</code> against the '
  'operator\'s on-chain entitlement. All sixteen reproduced exactly; all landed before the expiry block '
  '<span class="blk">7,991,073</span>.</caption></div>')

w('<h3>7.4 Reproducing this verification</h3>')
w('<p class="prose">Any reviewer with read access to a chain-945 archive node can reproduce every '
  '<em>re-verified on chain</em> row. The three commands below cover the three classes of read used.</p>')
w('<pre style="background:var(--surface-2);border:var(--hair) solid var(--rule);border-radius:var(--radius);'
  'padding:var(--s3);overflow-x:auto;font-size:12px;line-height:1.5"><code>'
  '# 1. an EVM receipt (inclusion + execution status)\n'
  'curl -sS $NODE -H \'content-type: application/json\' --data \\\n'
  '  \'{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt",\n'
  '    "params":["0x6255bbc039103fa0a23e9a6d52adf50097fd5d3cc44333389018e7b90204fde3"]}\'\n\n'
  '# 2. vault accounting at the pinned historical block (totalPaid())\n'
  'curl -sS $NODE -H \'content-type: application/json\' --data \\\n'
  '  \'{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[\n'
  '    {"to":"0x09d5d7a5c3e94b6ae42b09889a1cee50f970fc5e","data":"0xe7b0f666"},"0x79ee7a"]}\'\n'
  '#    -> 0x…180e6415f2  =  103,320,655,346 alpha-rao\n\n'
  '# 3. the validator weight row applied at native block 7,987,601\n'
  'curl -sS $NODE -H \'content-type: application/json\' --data \\\n'
  '  \'{"jsonrpc":"2.0","id":1,"method":"state_getStorageAt","params":[\n'
  '    "0x658faa385070e074c85bf6b568cf0555a1e7036061f484bc3048a64b64e35ec40902ff00",\n'
  '    "0xde5db784894d846de13bc8ada9fe76fc5e58cc31574e24fdd8adca2610e83f1a"]}\'\n'
  '#    -> 0x100300edff0400ffff0700075e08005e7d\n'
  '#       = 4 entries: UID3 65517, UID4 65535, UID7 24071, UID8 32094'
  '</code></pre>')
w('<p class="meta prose">Canonical finality is confirmed separately, by comparing '
  '<code>eth_getBlockByNumber</code> at the claimed height against the block hash the report pins — a '
  'receipt alone proves inclusion and successful execution, not that the block is on the canonical chain.</p>')
w('</section>')

# ---------------- limitations ----------------
w('<section id="method"><h2>8. Scope and limitations of this verification</h2>')
w('<p class="prose">The value of this document depends on a reader believing the method, so the '
  'boundaries are stated explicitly rather than left to inference.</p>')

w('<h3>8.1 Independence of the chain data</h3>')
w('<p class="prose">The verification was first performed against <code>192.168.1.162:9944</code>, a node '
  'on the subnet owners\' own LAN — the same node the exercise used. On its own that would be the single '
  'largest caveat in this document, so we repeated the load-bearing checks against a '
  '<strong>public Bittensor testnet endpoint operated by Opentensor</strong>, '
  '<code>test.finney.opentensor.ai</code>, which is outside the subnet owners\' control.</p>')
w('<div class="scroll-x"><table style="min-width:780px"><thead><tr><th style="width:42%">Re-checked on the '
  'public endpoint</th><th>Result</th><th style="width:150px">Evidence</th></tr></thead><tbody>')
for a,b,t in [
 ("Network identity","<code>eth_chainId 0x3b1</code>, <code>system_chain Bittensor</code>, and the same "
  "genesis hash <code class='hash'>0x8f9cf856…263105</code> the artifacts embed — the same network, not a look-alike.","chain"),
 ("All 41 cited transactions","41 of 41 return <code>status=0x1</code> at the claimed block height.","chain"),
 ("Vault accounting at both pinned blocks","Identical to the owned node and to the report: 103,320,655,354 captured, "
  "103,320,655,346 paid, residue 8, conservation true, <code>netuid 521</code>.","chain"),
 ("The three applied validator weight rows","Identical at blocks 7,986,519 / 7,987,601 / 7,988,052.","chain"),
 ("The reserve census","All 8 recorded storage queries — including the 278 KB Merkle read proof — reproduce "
  "byte-identical.","chain"),
 ("Coordinator policy versions","<code>policyCount() == 2</code>, confirming finding F1 off the owners' infrastructure.","chain")]:
    w(f'<tr><td>{a}</td><td style="color:var(--ink-2)">{b}</td><td>{ev(t)}</td></tr>')
w('</tbody></table><caption>Every check above was re-run end to end against the public endpoint after the '
  'owned-node pass. None disagreed.</caption></div>')
w('<p class="prose">This removes the structural objection: the chain facts in this document do not depend '
  'on the subnet owners\' node. Two residual points should still be stated. The owned node reports '
  '<code>system_nodeRoles = ["Full"]</code>, so the reporting party cannot author or finalize blocks on '
  'this chain in any case. And the cryptographic reconstructions — Merkle roots, content hashes, signature '
  'recovery, storage-key derivation — were done with implementations written for this verification and '
  'validated against published test vectors, so they depend on neither node nor on the project\'s own code.</p>')
w('<p class="prose">What remains outside any endpoint\'s reach is everything off-chain: the truth of the '
  'usage figures, the test-gate outcomes, and the process history. Those are addressed below.</p>')
w('<h3>8.2 What was not verified</h3>')
w('<div class="scroll-x"><table style="min-width:780px"><thead><tr><th style="width:34%">Not verified</th>'
  '<th>Why, and what would be needed</th><th style="width:150px">Status</th></tr></thead><tbody>')
for a,b,t in [
 ("Whether the usage figures inside the payout artifacts are true.",
  "The chain binds the artifact's hash, not its honesty. Confirming that 275,079,276,039 bytes were really carried requires the validator trail evidence and the provider-side records, which is the whitepaper's §11.3 dispute path — not exercised here.","none"),
 ("Historical authorization of the artifact signers.",
  "Both signatures recover to their declared signers, but neither signer is the coordinator-registered <code>rootSigner</code>. The on-chain authorization is carried by the commitment transaction, not the artifact signature. A full evidence-graph replay would be needed to establish what authorized those keys.","contra"),
 ("The test-suite, release-gate and process claims.",
  "Producer and aggregate gate outcomes, restart counts, service start/stop and fleet health are off-chain. They rest entirely on the run's own receipts and are reported here as asserted.","asserted"),
 ("Lifetime fee and registration accounting.",
  "The three recovered alpha transfers were confirmed on chain by BLAKE2b-256 and sum correctly to the 31,250 α lifetime figure, but the 30,999.499999975 α prior subtotal is carried forward from the run's own records. The report itself flags that source and destination balance deltas were never replayed.","artifact"),
 ("The 65% reserve repair and the approved 35,000 α cap.",
  "No further transfer has been applied, so there is nothing on chain to verify. The reserve stands at 61.449%.","none"),
 ("Behaviour at the whitepaper's stated testnet cadence.",
  "The 360-block policy was never scheduled, so no epoch ran at it. See finding F1.","contra"),
 ("Behaviour with more than one active validator, or more than two operators.",
  "The exercise ran two operators and one effective validator. Yuma consensus, median clipping, vtrust and the anti-self-dealing defence in §12 are untested by this run.","none")]:
    w(f'<tr><td>{a}</td><td style="color:var(--ink-2)">{b}</td><td>{ev(t)}</td></tr>')
w('</tbody></table></div>')

w('<h3>8.3 How to read the verdict</h3>')
w('<p class="prose">The honest summary for a Bittensor reviewer is narrower than "the subnet works", and '
  'stronger than "a test ran".</p>')
w(f'<div class="note"><p style="margin:0 0 8px"><strong>What this exercise demonstrates.</strong> '
  'The settlement mechanism described in whitepaper §8.3 and §11.2 — contract-custodied pool emission, '
  'operator-committed Merkle payout roots, direct provider claims with cryptographic proof, and '
  'conservation of the captured total — executed correctly on a real chain, end to end, for sixteen '
  'providers across two operators, with every amount forced by the committed root and the arithmetic '
  'closing to the alpha-rao. The contract topology and the governance guarantee in §6.4 are as specified: '
  'the vault and reserve sink are genuinely immutable. The live subnet hyperparameters match §15.1. '
  'The one weight vector that carried both channels implements the §8.5 θ split to within one part in a million.</p>'
  '<p style="margin:0"><strong>What it does not demonstrate.</strong> Sustained operation — there was exactly '
  'one positive-emission epoch in the contract\'s entire lifetime. Decentralised weight-setting — one '
  'validator, never two. Acceptance at the cadence the whitepaper names. Release qualification — the full '
  'gates failed. The whitepaper\'s own acceptance bar, three consecutive fully observed epochs at the '
  'testnet cadence, was not cleared.</p></div>')
w('<p class="prose">Both halves should be presented together. The first half is a genuine result and is '
  'unusually well evidenced for a testnet exercise; the second is the reason '
  '<code>final_acceptance = false</code> is the right verdict, and the report says so.</p>')
w('</section>')

w(f'<footer class="colophon meta" style="border-top:var(--hair) solid var(--rule);padding-top:var(--s4);margin-top:var(--s6)">'
  f'Generated {esc(GEN)} · {ntot} assertions tested, {npass} reproduced · '
  f'source report <code>sim-testnet/FINAL.md</code> · specification <code>WHITEPAPER.md</code> · '
  f'node <code>{esc(M["endpoint"])}</code> at head <span class="blk">{fmt(M["head"])}</span> · '
  f'chain <code>945</code>, netuid <code>521</code>.<br>'
  f'Evidence tokens follow the document legend. Claims marked <em>asserted</em> or <em>artifact only</em> '
  f'were not independently re-derived and should not be presented as verified.'
  f'</footer>')
w('</main>')

html = "\n".join(O)
open(os.path.join(_REPO,"sim-testnet","final.html"),"w").write(html)
print("wrote sim-testnet/final.html:", len(html), "bytes")
