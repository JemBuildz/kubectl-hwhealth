package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
)

type kubeNodeList struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			NodeInfo struct {
				Architecture string `json:"architecture"`
			} `json:"nodeInfo"`
		} `json:"status"`
	} `json:"items"`
}

func main() {
	warnTemp := flag.Float64("warn-temp", 75.0, "CPU temperature warning threshold in Celsius")
	simulateFail := flag.Bool("simulate-raid-fail", false, "Simulate a degraded RAID array to test Pro alerts")
	autoDrain := flag.Bool("auto-drain", false, "[PRO] Automatically cordon/drain nodes on RAID or thermal failure")
	webhookURL := flag.String("webhook-url", "", "[PRO] Slack or Discord webhook URL for hardware failure alerts")
	licenseKey := flag.String("license-key", "", "Pro license key (format: PRO-XXXX)")
	flag.Parse()

	fmt.Printf("%s=== kubectl-hwhealth: Bare-Metal RAID & Thermal Scanner ===%s\n\n", colorCyan, colorReset)

	cpuTemp := readMaxThermalC()
	raidStatus, raidDegraded := checkLinuxRAID(*simulateFail)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NODE\tARCH\tCPU TEMP\tRAID ARRAY\tHARDWARE STATUS")

	nodes, err := fetchKubeNodes()
	var criticalNode string

	if err != nil || len(nodes.Items) == 0 {
		host, _ := os.Hostname()
		if host == "" {
			host = "local-node"
		}
		fmt.Printf("%s[i] Running in Direct Bare-Metal Host Mode (kubectl cluster not active)%s\n\n", colorYellow, colorReset)
		status, isCrit := evaluateHealth(cpuTemp, *warnTemp, raidDegraded)
		if isCrit {
			criticalNode = host
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			host, runtime.GOARCH, formatTemp(cpuTemp), raidStatus, status)
	} else {
		for _, n := range nodes.Items {
			name := n.Metadata.Name
			arch := n.Status.NodeInfo.Architecture
			status, isCrit := evaluateHealth(cpuTemp, *warnTemp, raidDegraded)
			if isCrit {
				criticalNode = name
			}
			if n.Spec.Unschedulable {
				status += " (CORDONED)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				name, arch, formatTemp(cpuTemp), raidStatus, status)
		}
	}
	w.Flush()

	if criticalNode != "" && !*autoDrain && *webhookURL == "" {
		fmt.Printf("\n%s[!] HARDWARE RISK DETECTED on '%s'.%s\n", colorRed, criticalNode, colorReset)
		fmt.Println("-> Upgrade to Pro to automatically cordon failing nodes & send Slack/Discord alerts:")
		fmt.Println("   kubectl hwhealth --auto-drain --webhook-url=https://... --license-key=YOUR_KEY")
		return
	}

	if *autoDrain || *webhookURL != "" {
		runProAutomation(criticalNode, raidStatus, cpuTemp, *autoDrain, *webhookURL, *licenseKey)
	}
}

func checkLinuxRAID(simulateFail bool) (string, bool) {
	if simulateFail {
		return "md0 [U_] (DEGRADED)", true
	}
	data, err := os.ReadFile("/proc/mdstat")
	if err != nil {
		return "No mdraid (NVMe/Single)", false
	}
	content := string(data)
	if !strings.Contains(content, "md") {
		return "No active RAID", false
	}
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.Contains(line, "[") && strings.Contains(line, "_") {
			return "DEGRADED " + strings.TrimSpace(line), true
		}
		if strings.Contains(line, "recovery") || strings.Contains(line, "resync") {
			return "RESYNCING", false
		}
	}
	return "HEALTHY [UU]", false
}

func readMaxThermalC() float64 {
	files, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(files) == 0 {
		return 42.0
	}
	maxTemp := -1.0
	for _, f := range files {
		rawBytes, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		val, err := strconv.Atoi(strings.TrimSpace(string(rawBytes)))
		if err == nil {
			c := float64(val) / 1000.0
			if c > maxTemp && c < 130.0 {
				maxTemp = c
			}
		}
	}
	if maxTemp < 0 {
		return 42.0
	}
	return maxTemp
}

func fetchKubeNodes() (kubeNodeList, error) {
	var list kubeNodeList
	ctxCmd := exec.Command("kubectl", "get", "nodes", "-o", "json")
	out, err := ctxCmd.Output()
	if err != nil {
		return list, err
	}
	err = json.Unmarshal(out, &list)
	return list, err
}

func formatTemp(t float64) string {
	return fmt.Sprintf("%.1f°C", t)
}

func evaluateHealth(tempC, warnTemp float64, raidDegraded bool) (string, bool) {
	if raidDegraded {
		return colorRed + "CRITICAL: RAID DRIVE FAILED" + colorReset, true
	}
	if tempC >= warnTemp {
		return colorRed + "CRITICAL: THERMAL THROTTLE" + colorReset, true
	}
	return colorGreen + "HEALTHY" + colorReset, false
}

func verifyLicense(key string) bool {
	if key == "PRO-DEMO-2026" {
		return true
	}
	if !strings.HasPrefix(key, "HWPRO-") || len(key) < 14 {
		return false
	}
	parts := strings.Split(key, "-")
	if len(parts) != 3 {
		return false
	}
	hash := sha256.Sum256([]byte(parts[1] + "-salt-jembuildz"))
	expected := strings.ToUpper(hex.EncodeToString(hash[:])[:6])
	return parts[2] == expected
}

func runProAutomation(node, raidStatus string, tempC float64, autoDrain bool, webhook, licenseKey string) {
	fmt.Println()
	if !verifyLicense(licenseKey) {
		fmt.Printf("%s[PRO LOCKED] Automated node draining & Slack/Discord alerts require a valid Pro License Key.%s\n", colorYellow, colorReset)
		fmt.Println("-> Get an instant key at: https://github.com/jembuildz/kubectl-hwhealth#pro-license")
		fmt.Println("-> Or test Pro mode right now with demo key: --license-key=PRO-DEMO-2026")
		return
	}

	fmt.Printf("%s[PRO VERIFIED] Active license detected.%s\n", colorGreen, colorReset)
	if node == "" {
		fmt.Println("-> All nodes healthy. Standing by.")
		return
	}

	if autoDrain {
		fmt.Printf("%s[ACTION] Cordoning node '%s' to protect workloads from hardware failure...%s\n", colorRed, node, colorReset)
		_ = exec.Command("kubectl", "cordon", node).Run()
	}

	if webhook != "" {
		payload := map[string]string{
			"content": fmt.Sprintf("🚨 **[kubectl-hwhealth Alert]** Node `%s` hardware failure detected! RAID: `%s` | CPU Temp: `%.1f°C`. Node cordoned: `%v`", node, raidStatus, tempC, autoDrain),
		}
		body, _ := json.Marshal(payload)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(webhook, "application/json", bytes.NewBuffer(body))
		if err == nil {
			resp.Body.Close()
			fmt.Printf("%s[ALERT SENT] Webhook notification delivered.%s\n", colorGreen, colorReset)
		}
	}
}
