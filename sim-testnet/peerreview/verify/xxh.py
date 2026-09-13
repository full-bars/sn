P1=11400714785074694791; P2=14029467366897019727; P3=1609587929392839161
P4=9650029242287828579;  P5=2870177450012600261;  M=(1<<64)-1
def rotl(x,r): return ((x<<r)|(x>>(64-r)))&M
def _round(acc,inp): return (rotl((acc+inp*P2)&M,31)*P1)&M
def _merge(acc,val):
    acc^=_round(0,val); return (acc*P1+P4)&M
def xxh64(data,seed=0):
    n=len(data); i=0
    if n>=32:
        v1=(seed+P1+P2)&M; v2=(seed+P2)&M; v3=seed&M; v4=(seed-P1)&M
        while i+32<=n:
            v1=_round(v1,int.from_bytes(data[i:i+8],'little')); i+=8
            v2=_round(v2,int.from_bytes(data[i:i+8],'little')); i+=8
            v3=_round(v3,int.from_bytes(data[i:i+8],'little')); i+=8
            v4=_round(v4,int.from_bytes(data[i:i+8],'little')); i+=8
        h=(rotl(v1,1)+rotl(v2,7)+rotl(v3,12)+rotl(v4,18))&M
        h=_merge(h,v1); h=_merge(h,v2); h=_merge(h,v3); h=_merge(h,v4)
    else:
        h=(seed+P5)&M
    h=(h+n)&M
    while i+8<=n:
        h^=_round(0,int.from_bytes(data[i:i+8],'little'))
        h=(rotl(h,27)*P1+P4)&M; i+=8
    if i+4<=n:
        h^=(int.from_bytes(data[i:i+4],'little')*P1)&M
        h=(rotl(h,23)*P2+P3)&M; i+=4
    while i<n:
        h^=(data[i]*P5)&M; h=(rotl(h,11)*P1)&M; i+=1
    h^=h>>33; h=(h*P2)&M; h^=h>>29; h=(h*P3)&M; h^=h>>32
    return h
def twox128(b): return xxh64(b,0).to_bytes(8,'little')+xxh64(b,1).to_bytes(8,'little')
def twox64(b):  return xxh64(b,0).to_bytes(8,'little')
