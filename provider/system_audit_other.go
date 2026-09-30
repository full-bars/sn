//go:build !linux

package provider

func RunSystemAudit(skipDisk bool) (slowDisk bool, lowSpace bool) {
	return false, false
}
