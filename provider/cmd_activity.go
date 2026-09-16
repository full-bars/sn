package provider

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

func proxyActivity() {
	shmLog := os.Getenv("URNETWORK_SHM_LOG")
	if shmLog == "" {
		shmLog = "/dev/shm/urnetwork.log"
	}

	statePath, err := proxyStatePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not determine state path: %v\n", err)
		return
	}

	_ = statePath
	_ = shmLog

	fmt.Println("Proxy Activity Monitor")
	fmt.Println("Press Ctrl+C to exit")
	fmt.Println()

	// Use terminal directly for live updates
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// Non-interactive: just take one snapshot
		activitySnapshot(shmLog)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	reader := bufio.NewReader(os.Stdin)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		for {
			b, _ := reader.ReadByte()
			if b == 'q' || b == 0x03 {
				close(done)
				return
			}
		}
	}()

	fmt.Print("\033[?25l")       // hide cursor
	defer fmt.Print("\033[?25h") // show cursor

	// Scroll window tracking recent contract events
	const maxEvents = 20
	var recentContracts []string
	var contractMu sync.Mutex

	// Goroutine to tail the log for contract events
	go func() {
		f, err := os.Open(shmLog)
		if err != nil {
			return
		}
		defer f.Close()

		// Seek to end
		_, _ = f.Seek(0, io.SeekEnd)

		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := f.Read(buf)
			if err != nil || n == 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			lines := strings.Split(string(buf[:n]), "\n")
			for _, line := range lines {
				if strings.Contains(line, "[contract] acquired") || strings.Contains(line, "[contract] denied") {
					// Extract timestamp and message
					if idx := strings.Index(line, "[contract]"); idx >= 0 {
						ts := ""
						if len(line) > 19 {
							ts = strings.TrimSpace(line[:19])
						}
						contractMu.Lock()
						recentContracts = append(recentContracts, fmt.Sprintf("%s %s", ts, line[idx:]))
						if len(recentContracts) > maxEvents {
							recentContracts = recentContracts[1:]
						}
						contractMu.Unlock()
					}
				}
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		// Read health state for up/down counts
		up, connecting, degraded := 0, 0, 0
		healthDir, ok := proxyHealthDir()
		if ok {
			if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state")); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, " Up:") {
						var down, dead int
						fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
					}
				}
			}
		}

		// Parse traffic state for active proxies
		type activeProxy struct {
			id      string
			addr    string
			rx      string
			tx      string
			clients int
			age     string
			bill    string
		}
		var active []activeProxy
		if ok {
			if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_traffic.state")); err == nil {
				lines := strings.Split(string(data), "\n")
				for _, line := range lines {
					if !strings.Contains(line, "proxy[") || strings.Contains(line, "PROXY ID") {
						continue
					}
					// Parse: | proxy[42] | 1.2.3.4:1080 | 2 | 5m | 10 MB / 20 MB | 100 MB / 200 MB |
					parts := strings.Split(line, "|")
					if len(parts) >= 6 {
						id := strings.TrimSpace(parts[1])
						addr := strings.TrimSpace(parts[2])
						clientsStr := strings.TrimSpace(parts[3])
						age := strings.TrimSpace(parts[4])
						bill := strings.TrimSpace(parts[5])
						total := strings.TrimSpace(parts[6])

						var cl int
						fmt.Sscanf(clientsStr, "%d", &cl)
						if cl > 0 {
							active = append(active, activeProxy{
								id: id, addr: addr, clients: cl,
								age: age, bill: bill, rx: total, tx: total,
							})
						}
					}
				}
			}
		}

		// Build output
		var out strings.Builder

		// Header
		out.WriteString("\033[H\033[J") // clear screen
		now := time.Now()
		out.WriteString(fmt.Sprintf("Proxy Activity — %s (refreshing every 1s)\n", now.Format(time.RFC3339)))
		out.WriteString(fmt.Sprintf("Up: %d | Degraded: %d | Connecting: %d\n", up, degraded, connecting))
		out.WriteString(fmt.Sprintf("Active proxies with clients: %d\n", len(active)))
		out.WriteString("\n")

		// Active proxies table
		if len(active) > 0 {
			out.WriteString(fmt.Sprintf("%-18s %-22s %9s %9s %5s  %s\n", "PROXY", "ADDRESS", "RX", "TX", "CLI", "AGE"))
			out.WriteString(strings.Repeat("-", 70) + "\n")
			for _, p := range active {
				out.WriteString(fmt.Sprintf("%-18s %-22s %9s %9s %5d  %s\n",
					p.id, p.addr, p.rx, p.tx, p.clients, p.age))
			}
		} else {
			out.WriteString("No proxies with active clients.\n")
		}

		// Recent contracts
		contractMu.Lock()
		if len(recentContracts) > 0 {
			out.WriteString(fmt.Sprintf("\nRecent Contracts:\n"))
			start := len(recentContracts) - 10
			if start < 0 {
				start = 0
			}
			for _, c := range recentContracts[start:] {
				out.WriteString("  " + c + "\n")
			}
		}
		contractMu.Unlock()

		out.WriteString("\n[q] quit\n")
		fmt.Print(out.String())
	}
}

func activitySnapshot(shmLog string) {
	// Single snapshot for non-interactive use
	healthDir, ok := proxyHealthDir()
	if !ok {
		return
	}

	up, degraded := 0, 0
	if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state")); err == nil {
		var down, dead int
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, " Up:") {
				fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
			}
		}
	}
	fmt.Printf("Up: %d | Degraded: %d\n", up, degraded)

	if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_traffic.state")); err == nil {
		fmt.Println(string(data))
	}
}
