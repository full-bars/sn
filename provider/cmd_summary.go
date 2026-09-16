package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func proxySummary() {
	state, _ := readProxyState()

	up, dead, degraded, connecting := 0, 0, 0, 0
	if healthDir, ok := proxyHealthDir(); ok {
		if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, " Up:") {
					var down int
					fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
				}
			}
		}
	}
	fileCount := 0
	urlCount := 0
	internalCount := 0
	total := 0
	if state != nil {
		total = len(state.Proxies)
		for _, e := range state.Proxies {
			switch e.Source {
			case "url":
				urlCount++
			case "file":
				fileCount++
			case "internal":
				internalCount++
			default:
				if state.Source != "" {
					fileCount++
				} else {
					internalCount++
				}
			}
		}
		connecting = total - up - dead - degraded
		if connecting < 0 {
			connecting = 0
		}
	}

	urlState, _ := readProxyURLState()
	urlSources := 0
	urlCached := 0
	urlBlacklisted := 0
	if urlState != nil {
		urlSources = len(urlState.Sources)
		urlCached = len(urlState.Cache)
		urlBlacklisted = len(urlState.Blacklist)
	}

	healthDir, _ := proxyHealthDir()

	fmt.Println("=========================================================================")
	fmt.Println(" PROXY SUMMARY")
	fmt.Printf(" Updated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Println("=========================================================================")
	fmt.Println()
	fmt.Printf("  Total proxies:      %d\n", total)
	fmt.Printf("  Up:                 %d\n", up)
	fmt.Printf("  Connecting:         %d\n", connecting)
	fmt.Printf("  Degraded:           %d\n", degraded)
	fmt.Printf("  Dead:               %d\n", dead)
	fmt.Println()
	fmt.Println(" --- Sources ---")
	fileSource := "(internal)"
	if state != nil && state.Source != "" {
		fileSource = state.Source
	}
	fmt.Printf("  File proxies:       %d  (%s)\n", fileCount, fileSource)
	fmt.Printf("  URL proxies:        %d\n", urlCount)
	fmt.Printf("  Internal proxies:   %d\n", internalCount)
	fmt.Println()
	fmt.Println(" --- URL Sources ---")
	fmt.Printf("  Source URLs:        %d\n", urlSources)
	fmt.Printf("  Cached addresses:   %d\n", urlCached)
	fmt.Printf("  Blacklisted:        %d\n", urlBlacklisted)
	if urlState != nil && len(urlState.ExcludePatterns) > 0 {
		fmt.Printf("  Exclude patterns:   %s\n", strings.Join(urlState.ExcludePatterns, ", "))
	}
	if urlSources > 0 {
		fmt.Println()
		for _, s := range urlState.Sources {
			fmt.Printf("    %s\n", s)
		}
	}
	fmt.Println()
	if state != nil {
		fmt.Printf("  Provider started:   %s\n", state.StartedAt.Format(time.RFC3339))
	}
	if state != nil {
		if p, err := proxyStatePath(); err == nil {
			fmt.Printf("  Proxy state file:   %s\n", p)
		}
	}
	fmt.Printf("  Health state:       %s/proxy_health.state\n", healthDir)
	if p, err := proxyURLStatePath(); err == nil {
		fmt.Printf("  URL state:          %s\n", p)
	}

	totalAcquired, totalDenied := globalContractMetrics.totals()
	a15, d15 := globalContractMetrics.windowTotals(15 * time.Minute)
	a60, d60 := globalContractMetrics.windowTotals(60 * time.Minute)
	a1440, d1440 := globalContractMetrics.windowTotals(1440 * time.Minute)

	fmt.Println()
	fmt.Println(" --- Contract Stats ---")
	cTotal := totalAcquired + totalDenied
	winRate := 0.0
	if cTotal > 0 {
		winRate = float64(totalAcquired) / float64(cTotal) * 100
	}
	fmt.Printf("  Acquired:           %d\n", totalAcquired)
	fmt.Printf("  Denied:             %d\n", totalDenied)
	fmt.Printf("  Win rate:           %.1f%%\n", winRate)
	fmt.Printf("  15m:  %d acquired / %d denied\n", a15, d15)
	fmt.Printf("  1h:   %d acquired / %d denied\n", a60, d60)
	fmt.Printf("  24h:  %d acquired / %d denied\n", a1440, d1440)
	fmt.Println("=========================================================================")
}
