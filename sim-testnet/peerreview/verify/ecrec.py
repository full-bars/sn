P=0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F
N=0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141
Gx=0x79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798
Gy=0x483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8
def inv(a,m=P): return pow(a,m-2,m)
def add(p,q):
    if p is None: return q
    if q is None: return p
    if p[0]==q[0] and (p[1]+q[1])%P==0: return None
    if p==q: l=(3*p[0]*p[0]*inv(2*p[1]))%P
    else:    l=((q[1]-p[1])*inv((q[0]-p[0])%P))%P
    x=(l*l-p[0]-q[0])%P
    return (x,(l*(p[0]-x)-p[1])%P)
def mul(k,p):
    r=None
    while k:
        if k&1: r=add(r,p)
        p=add(p,p); k>>=1
    return r
def recover(msghash:bytes, sig:bytes):
    r=int.from_bytes(sig[0:32],'big'); s=int.from_bytes(sig[32:64],'big'); v=sig[64]
    if v>=27: v-=27
    if not (0<r<N and 0<s<N) or v not in (0,1): return None
    x=r+ (N if v>=2 else 0)
    y2=(pow(x,3,P)+7)%P
    y=pow(y2,(P+1)//4,P)
    if (y%2)!=(v&1): y=P-y
    if (y*y-y2)%P: return None
    R=(x,y); e=int.from_bytes(msghash,'big')%N
    Q=mul(inv(r,N)%N, add(mul(s,R), mul((-e)%N, (Gx,Gy))))
    return Q
def addr(Q):
    from keccak import keccak256
    pub=Q[0].to_bytes(32,'big')+Q[1].to_bytes(32,'big')
    return '0x'+keccak256(pub)[-20:].hex()
