CSS = r"""
<title>SN 521 Testnet Verification</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@400;500;600&family=IBM+Plex+Sans+Condensed:wght@600;700&family=IBM+Plex+Mono:wght@400;500;600&display=swap">
<style>
:root{
 color-scheme:light;
 --bg:#fbfaf7; --surface:#ffffff; --surface-2:#f3f1ec;
 --ink-1:#1a1917; --ink-2:#4f4d48; --ink-3:#6f6c65;
 --rule:#dedbd3; --rule-2:#b9b5ab;
 --drop:rgba(26,25,23,.28); --lane-alt:rgba(26,25,23,.035);
 --ev-verify:#1f5fa8; --ev-verify-wash:#e6eef9;
 --ev-fail:#b3261e; --ev-fail-wash:#f9e5e3;
 --ev-glyph:#ffffff; --link:#1f5fa8;
 --container:1120px; --measure:74ch; --gutter:16px;
 --s1:4px; --s2:8px; --s3:12px; --s4:16px; --s5:24px; --s6:40px; --s7:64px;
 --radius:3px; --hair:1px;
 --font-sans:"IBM Plex Sans",ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Arial,sans-serif;
 --font-head:"IBM Plex Sans Condensed","IBM Plex Sans",ui-sans-serif,system-ui,-apple-system,"Segoe UI",Arial,sans-serif;
 --font-mono:"IBM Plex Mono",ui-monospace,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;
}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){
 color-scheme:dark;
 --bg:#141412; --surface:#1c1c1a; --surface-2:#24241f;
 --ink-1:#ecebe6; --ink-2:#b8b6ae; --ink-3:#918e86;
 --rule:#2e2e2b; --rule-2:#4a4944;
 --drop:rgba(236,235,230,.28); --lane-alt:rgba(236,235,230,.045);
 --ev-verify:#6aa0e4; --ev-verify-wash:#1e2a3a;
 --ev-fail:#e5736b; --ev-fail-wash:#3a2220;
 --ev-glyph:#141412; --link:#6aa0e4;
}}
:root[data-theme="dark"]{
 color-scheme:dark;
 --bg:#141412; --surface:#1c1c1a; --surface-2:#24241f;
 --ink-1:#ecebe6; --ink-2:#b8b6ae; --ink-3:#918e86;
 --rule:#2e2e2b; --rule-2:#4a4944;
 --drop:rgba(236,235,230,.28); --lane-alt:rgba(236,235,230,.045);
 --ev-verify:#6aa0e4; --ev-verify-wash:#1e2a3a;
 --ev-fail:#e5736b; --ev-fail-wash:#3a2220;
 --ev-glyph:#141412; --link:#6aa0e4;
}
html{font-family:var(--font-sans);font-size:15px;line-height:1.55;-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--ink-1);padding-inline:var(--gutter);padding-block:0 var(--s7)}
.page{max-width:var(--container);margin-inline:auto}
.prose{max-width:var(--measure)}
code,kbd,.mono,.hash,.blk,.amt{font-family:var(--font-mono);font-size:.92em}
.blk,.amt,table{font-variant-numeric:tabular-nums}
h1,h2,h3{font-family:var(--font-head);font-weight:700;letter-spacing:.005em;text-wrap:balance}
h1{font-size:34px;line-height:1.12;margin:0 0 var(--s2)}
h2{font-size:26px;line-height:1.15;margin:var(--s6) 0 var(--s4);padding-top:var(--s4);border-top:var(--hair) solid var(--rule)}
h3{font-size:20px;line-height:1.2;font-weight:600;margin:var(--s5) 0 var(--s3)}
h4{font-size:14px;font-weight:600;margin:var(--s4) 0 var(--s2);letter-spacing:.06em;text-transform:uppercase;color:var(--ink-2)}
th{font-family:var(--font-head);letter-spacing:.01em}
.actor__name{font-family:var(--font-head);letter-spacing:.005em}
a:focus-visible,summary:focus-visible{outline:2px solid var(--ev-verify);outline-offset:2px;border-radius:2px}
p{margin:0 0 var(--s3)}
a{color:var(--link);text-decoration:underline;text-underline-offset:2px}
a:hover{text-decoration-thickness:2px}
section{margin-block:var(--s6)}
.lead{font-size:17px;line-height:1.5}
.meta{font-size:12px;color:var(--ink-3)}
.scroll-x{overflow-x:auto;overscroll-behavior-x:contain;-webkit-overflow-scrolling:touch}
.topbar{position:sticky;top:0;z-index:20;background:var(--bg);border-bottom:var(--hair) solid var(--rule);
 display:flex;gap:var(--s3);align-items:center;justify-content:space-between;min-height:40px;flex-wrap:wrap;
 padding-block:var(--s2);margin-inline:calc(var(--gutter)*-1);padding-inline:var(--gutter)}
.topbar__title{font-size:12px;color:var(--ink-2)}
/* evidence tokens */
.sprite{position:absolute;width:0;height:0;overflow:hidden}
.evf-verify{fill:var(--ev-verify)} .evs-verify{stroke:var(--ev-verify);stroke-width:1.5;fill:none}
.evf-fail{fill:var(--ev-fail)} .evs-fail{stroke:var(--ev-fail);stroke-width:1.6;stroke-linecap:round;fill:none}
.evf-muted{fill:var(--ink-3)} .evs-muted{stroke:var(--ink-3);stroke-width:1.5;stroke-linecap:round;fill:none}
.evg-stroke{stroke:var(--ev-glyph);stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round;fill:none}
.evg-fill{fill:var(--ev-glyph)}
.ev{display:inline-flex;align-items:center;gap:4px;white-space:nowrap;vertical-align:-2px}
.ev__icon{width:14px;height:14px;flex:none}
.ev__label{font-size:12px;line-height:1.25;color:var(--ink-2)}
.ev--fail .ev__label,.ev--contra .ev__label{color:var(--ev-fail)}
.ev--chain .ev__label,.ev--artifact .ev__label{color:var(--ev-verify)}
/* tables */
table{border-collapse:collapse;width:100%;font-size:13px;line-height:1.35}
th{text-align:left;font-weight:600;border-bottom:var(--hair) solid var(--rule-2);padding:6px 10px 6px 0;vertical-align:bottom}
td{border-bottom:var(--hair) solid var(--rule);padding:7px 10px 7px 0;vertical-align:top}
tbody tr:nth-child(even){background:var(--lane-alt)}
th:last-child,td:last-child{padding-right:0}
caption{caption-side:bottom;text-align:left;font-size:12px;color:var(--ink-3);padding-top:var(--s2)}
figure{margin:var(--s4) 0}
figcaption{font-size:12px;color:var(--ink-3);margin-top:var(--s2);max-width:var(--measure)}
.fig-legend{display:flex;flex-wrap:wrap;gap:4px 14px;font-size:11px;color:var(--ink-3);margin-top:6px}
.fig-legend .ev__label{font-size:11px;color:var(--ink-3)}
svg.fig{display:block;background:var(--surface);border:var(--hair) solid var(--rule);border-radius:var(--radius)}
/* stat tiles */
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(158px,1fr));gap:var(--s3);margin:var(--s4) 0}
.stat{border-top:2px solid var(--rule-2);padding-top:var(--s2)}
.stat__label{font-size:12px;color:var(--ink-3)}
.stat__value{font-size:20px;font-weight:600;line-height:1.2;display:flex;align-items:center;gap:6px;margin-top:2px}
.stat__note{font-size:12px;color:var(--ink-2);margin-top:2px}
/* actor card */
.actor{border:var(--hair) solid var(--rule-2);border-radius:var(--radius);background:var(--surface);
 padding:var(--s4);margin-block:var(--s5)}
.actor__head{display:flex;align-items:baseline;gap:var(--s2);flex-wrap:wrap;justify-content:space-between}
.actor__name{font-size:20px;font-weight:600;display:flex;align-items:center;gap:var(--s2)}
.actor__mono{font:600 11px/1 var(--font-mono);padding:3px 5px;border:var(--hair) solid var(--rule-2);
 border-radius:var(--radius);color:var(--ink-2)}
.actor__wp{font:12px/1.4 var(--font-mono);color:var(--ink-3)}
.actor__role{font-size:13px;color:var(--ink-2);margin:var(--s2) 0 0;max-width:var(--measure)}
.chips{display:flex;flex-wrap:wrap;gap:6px;margin-top:var(--s2)}
.chip{font:11px/1 var(--font-mono);border:var(--hair) solid var(--rule-2);border-radius:var(--radius);
 padding:4px 6px;color:var(--ink-2);background:var(--bg)}
/* callouts */
.contra{display:grid;grid-template-columns:1fr 24px 1fr;gap:var(--s3);align-items:start;
 border:var(--hair) solid var(--rule-2);border-radius:var(--radius);padding:var(--s3);margin:var(--s4) 0}
.contra__cell{padding:var(--s3);border-radius:var(--radius);font-size:13px}
.contra__cell--claim{background:var(--ev-fail-wash)}
.contra__cell--obs{background:var(--ev-verify-wash)}
.contra__head{font-size:12px;color:var(--ink-2);margin-bottom:var(--s2)}
.contra__arrow{align-self:center;text-align:center;color:var(--ink-3)}
.note{border-left:3px solid var(--rule-2);padding:var(--s2) 0 var(--s2) var(--s3);margin:var(--s4) 0;
 font-size:13px;color:var(--ink-2);max-width:var(--measure)}
.hash{word-break:break-all;overflow-wrap:anywhere;font-size:12px}
svg text{font-family:var(--font-sans)}
.legend-tbl td{padding:6px 12px 6px 0}
.strip th{font-size:11px;font-weight:600;line-height:1.25;color:var(--ink-1)}
.strip th span{display:block;font-weight:400;color:var(--ink-3);font-size:10px}
.strip td{font-size:11px;padding:6px 8px 6px 0;line-height:1.3}
.strip .sticky{position:sticky;left:0;background:var(--bg);font-weight:600;padding-right:10px}
.strip tbody tr:nth-child(even) .sticky{background:var(--bg)}
.brk{border-right:2px solid var(--ev-fail)}
.tally{font:13px/1.6 var(--font-mono);color:var(--ink-2);font-variant-numeric:tabular-nums;margin:var(--s2) 0 0}
.toc{font-size:13px;columns:2;column-gap:var(--s6);max-width:var(--measure)}
.toc a{display:block;margin-bottom:4px}
@media (max-width:599px){.contra{grid-template-columns:1fr}.contra__arrow{transform:rotate(90deg)}.toc{columns:1}}
@media (min-width:720px){:root{--gutter:24px}}
@media (min-width:1120px){:root{--gutter:32px}}
@media (forced-colors:active){
 .evf-verify,.evf-fail,.evf-muted{fill:CanvasText}
 .evs-verify,.evs-fail,.evs-muted{stroke:CanvasText}
 .evg-stroke{stroke:Canvas}.evg-fill{fill:Canvas}
 .contra__cell{border:1px solid CanvasText}
}
@media print{
 .topbar{position:static}
 svg.fig{border:1px solid #999}
 .actor{break-inside:avoid}
}
</style>
"""

SPRITE = r"""
<svg class="sprite" width="0" height="0" aria-hidden="true" focusable="false"><defs>
<marker id="arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
 <path d="M0 .8 L7.2 4 L0 7.2 Z" fill="currentColor"/></marker>
</defs>
<symbol id="ev-chain" viewBox="0 0 16 16"><circle cx="8" cy="8" r="7" class="evf-verify"/>
 <path d="M4.6 8.3 L7 10.7 L11.6 5.6" class="evg-stroke"/></symbol>
<symbol id="ev-artifact" viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.4" class="evs-verify"/>
 <path d="M8 1.6 A6.4 6.4 0 0 0 8 14.4 Z" class="evf-verify"/></symbol>
<symbol id="ev-asserted" viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.4" class="evs-muted"/>
 <path d="M5.8 6.3 C5.8 4.7 6.8 3.8 8.1 3.8 C9.5 3.8 10.4 4.7 10.4 5.9 C10.4 7.5 8.1 7.6 8.1 9.4" class="evs-muted"/>
 <circle cx="8.1" cy="11.7" r="1" class="evf-muted"/></symbol>
<symbol id="ev-contra" viewBox="0 0 16 16"><path d="M8 1.4 L14.6 8 L8 14.6 L1.4 8 Z" class="evs-fail"/>
 <path d="M5.7 5.7 L10.3 10.3 M10.3 5.7 L5.7 10.3" class="evs-fail"/></symbol>
<symbol id="ev-fail" viewBox="0 0 16 16"><rect x="1.5" y="1.5" width="13" height="13" rx="1.5" class="evf-fail"/>
 <path d="M8 4.3 V9.3" class="evg-stroke"/><circle cx="8" cy="11.8" r="1.1" class="evg-fill"/></symbol>
<symbol id="ev-none" viewBox="0 0 16 16"><circle cx="8" cy="8" r="6.4" class="evs-muted" stroke-dasharray="2.4 1.9"/>
 <path d="M5 8 H11" class="evs-muted"/></symbol>
</svg>
"""

EVN = {"chain":"re-verified on chain","artifact":"artifact only","asserted":"asserted, unverified",
       "contra":"contradicted","fail":"failed","none":"not attempted"}
SHORT = {"chain":"chain","artifact":"artifact","asserted":"asserted","contra":"contradicted",
         "fail":"failed","none":"not attempted"}

def ev(kind, label=True, title=None, short=False):
    t = f' title="{title}"' if title else ""
    lab = f'<span class="ev__label">{SHORT[kind] if short else EVN[kind]}</span>' if label else ""
    return (f'<span class="ev ev--{kind}"{t}>'
            f'<svg class="ev__icon" aria-hidden="true"><use href="#ev-{kind}"/></svg>{lab}</span>')

def legend_line():
    return '<div class="fig-legend">' + "".join(ev(k, short=True) for k in
           ["chain","artifact","asserted","contra","fail","none"]) + '</div>'
