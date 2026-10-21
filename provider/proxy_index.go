package provider

import (
	"strconv"
	"strings"
)

func parseProxyIndex(key string) int {
	idx := strings.IndexByte(key, ']')
	if idx <= 0 {
		return -1
	}
	start := strings.IndexByte(key[:idx], '[')
	if start < 0 {
		return -1
	}
	n, err := strconv.Atoi(key[start+1 : idx])
	if err != nil {
		return -1
	}
	return n
}
