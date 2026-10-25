package miner

// proxy_v6_probe_socks.go - recover the RFC 1928 reply code from a dial error.
//
// golang.org/x/net/proxy does not expose the reply code of a refused CONNECT.
// It wraps it as errors.New("unknown error " + <reply description>), where the
// description is the fixed text of internal/socks Reply.String. The miner's own
// logs show it verbatim, for example:
//
//	socks connect tcp6 203.0.113.9:1080->connect-v6.bringyour.com:443: unknown error host unreachable
//
// Without this step classifyV6Probe sees socksNoReply for every refusal and
// reports unknown for the very replies that prove a proxy cannot carry IPv6.

import "strings"

// socksReplyPrefix is what x/net puts before the reply description. Matching it
// is what keeps a TCP-level "connect: connection refused" to the proxy itself
// from being read as SOCKS reply 0x05 (the target refused).
const socksReplyPrefix = "unknown error "

// socksReplyByDescription maps Reply.String of golang.org/x/net/internal/socks
// to the RFC 1928 section 6 code. "succeeded" never arrives as an error.
var socksReplyByDescription = map[string]int{
	"general SOCKS server failure":      socksRepGeneralFailure,
	"connection not allowed by ruleset": socksRepNotAllowed,
	"network unreachable":               socksRepNetworkUnreachable,
	"host unreachable":                  socksRepHostUnreachable,
	"connection refused":                socksRepConnectionRefused,
	"TTL expired":                       socksRepTTLExpired,
	"command not supported":             socksRepCommandNotSupported,
	"address type not supported":        socksRepAddrTypeNotSupported,
}

// socksReplyFromError returns the SOCKS5 reply code carried in err, or
// socksNoReply when the failure did not come from a proxy reply (a dial error,
// an EOF, a timeout, a nil error).
func socksReplyFromError(err error) int {
	if err == nil {
		return socksNoReply
	}
	text := err.Error()
	index := strings.LastIndex(text, socksReplyPrefix)
	if index < 0 {
		return socksNoReply
	}
	description := strings.TrimSpace(text[index+len(socksReplyPrefix):])
	if code, ok := socksReplyByDescription[description]; ok {
		return code
	}
	return socksNoReply
}

// classifyV6ProbeError classifies a probe that failed with err, recovering the
// reply code from the error text. A successful probe has no error to inspect
// and is classified with classifyV6Probe(nil, socksRepSucceeded) by the caller.
func classifyV6ProbeError(err error) v6Verdict {
	return classifyV6Probe(err, socksReplyFromError(err))
}
