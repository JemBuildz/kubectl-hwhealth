package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// Set at release-build time to the public key corresponding to the offline
// license-signing key. The private signing key must never be shipped here.
var ed25519PubKeyHex = ""

type RaidState int

const (
	RaidUnknown RaidState = iota
	RaidNone
	RaidHealthy
	RaidResyncing
	RaidDegraded
)

type kubeNode struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		NodeInfo struct {
			Architecture string `json:"architecture"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type kubeNodeList struct {
	Items []kubeNode `json:"items"`
}

type nodeHealthReport struct {
	Name          string
	Arch          string
	TempDisplay   string
	RaidSummary   string
	StatusDisplay string
	Critical      bool
	Inspected     bool
	Actionable    bool
}

var (
	mdDeviceHeader = regexp.MustCompile(`^(md\d+)\s*:\s*(\S+)\s+(\S+)`)
	mdMemberState  = regexp.MustCompile(`\[([U_]+)\]`)
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("kubectl-hwhealth", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	warnTemp := fs.Float64("warn-temp", 75.0, "CPU/package temperature threshold in Celsius")
	nodeNameFlag := fs.String("node-name", "", "Kubernetes node name for this host; set explicitly if hostname differs")
	contextFlag := fs.String("context", "", "Kubernetes context to inspect and target; required with remediation/webhooks")
	simulateFail := fs.Bool("simulate-raid-fail", false, "Simulate a degraded array; all actions are always dry-run")
	autoCordon := fs.Bool("auto-cordon", false, "Cordon the verified local node on a confirmed hardware failure")
	autoDrain := fs.Bool("auto-drain", false, "Drain the verified local node on a confirmed hardware failure")
	webhookURL := fs.String("webhook-url", "", "Slack or Discord HTTPS webhook URL")
	licenseKeyFlag := fs.String("license-key", "", "Signed license key, or set HWHEALTH_LICENSE_KEY")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		return 2
	}
	if math.IsNaN(*warnTemp) || math.IsInf(*warnTemp, 0) || *warnTemp <= 0 || *warnTemp >= 135 {
		fmt.Fprintln(os.Stderr, "--warn-temp must be a finite value greater than 0 and less than 135 Celsius")
		return 2
	}
	if *autoCordon && *autoDrain {
		fmt.Fprintln(os.Stderr, "choose either --auto-cordon or --auto-drain, not both")
		return 2
	}
	wantAction := *autoCordon || *autoDrain || *webhookURL != ""
	kubeContext := strings.TrimSpace(*contextFlag)
	if wantAction && kubeContext == "" {
		fmt.Fprintln(os.Stderr, "--context is required with --auto-cordon, --auto-drain, or --webhook-url")
		return 2
	}

	licenseKey := strings.TrimSpace(*licenseKeyFlag)
	if licenseKey == "" {
		licenseKey = strings.TrimSpace(os.Getenv("HWHEALTH_LICENSE_KEY"))
	}

	fmt.Printf("%s=== kubectl-hwhealth: Bare-Metal RAID & Thermal Scanner ===%s\n\n", colorCyan, colorReset)
	localHost := resolveLocalNodeName(*nodeNameFlag)
	tempC, sensorType, tempOK := readMaxThermalC("/sys/class/thermal")
	raidSummary, raidState := checkLinuxRAID("/proc/mdstat", *simulateFail, runtime.GOOS)

	nodes, kubeErr := fetchKubeNodes(kubeContext)
	clusterAvailable := kubeErr == nil && len(nodes.Items) > 0
	var reports []nodeHealthReport
	var localMatchCount int
	if clusterAvailable {
		reports, localMatchCount = evaluateClusterNodes(nodes.Items, localHost, *nodeNameFlag != "", tempC, sensorType, tempOK, raidSummary, raidState, *warnTemp)
	} else {
		if kubeErr != nil {
			fmt.Fprintf(os.Stderr, "[!] kubectl get nodes failed; reporting this host only: %v\n", kubeErr)
		} else {
			fmt.Fprintln(os.Stderr, "[!] Kubernetes returned no nodes; reporting this host only")
		}
		reports = []nodeHealthReport{evaluateNodeTelemetry(localHost, runtime.GOARCH, tempC, sensorType, tempOK, raidSummary, raidState, *warnTemp, false, false)}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NODE\tARCH\tTHERMAL\tRAID ARRAY\tHARDWARE STATUS")
	var failingNodes []nodeHealthReport
	anyCritical := false
	for _, r := range reports {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Arch, r.TempDisplay, r.RaidSummary, r.StatusDisplay)
		if r.Critical {
			anyCritical = true
		}
		if r.Critical && r.Actionable {
			failingNodes = append(failingNodes, r)
		}
	}
	_ = w.Flush()

	if wantAction && (!clusterAvailable || localMatchCount != 1) {
		fmt.Fprintln(os.Stderr, "[ERROR] refusing remediation: this host could not be matched to exactly one Kubernetes node")
		return 1
	}
	if wantAction {
		if err := runProAutomation(failingNodes, *autoCordon, *autoDrain, *webhookURL, licenseKey, kubeContext, *simulateFail); err != nil {
			fmt.Fprintf(os.Stderr, "%s[ERROR] %v%s\n", colorRed, err, colorReset)
			return 1
		}
	}
	if anyCritical {
		return 2
	}
	return 0
}

func resolveLocalNodeName(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "local-host"
	}
	return strings.TrimSpace(host)
}

func evaluateClusterNodes(items []kubeNode, localHost string, explicitName bool, localTemp float64, sensorType string, tempOK bool, localRaid string, localRaidState RaidState, warnTemp float64) ([]nodeHealthReport, int) {
	matches := make([]bool, len(items))
	matchCount := 0
	for i, n := range items {
		match := false
		if explicitName {
			match = n.Metadata.Name == localHost
		} else {
			match = strings.EqualFold(n.Metadata.Name, localHost)
			if n.Metadata.Labels["kubernetes.io/hostname"] != "" {
				match = match || strings.EqualFold(n.Metadata.Labels["kubernetes.io/hostname"], localHost)
			}
		}
		if match {
			matches[i] = true
			matchCount++
		}
	}

	reports := make([]nodeHealthReport, 0, len(items)+1)
	for i, n := range items {
		name := n.Metadata.Name
		if matches[i] && matchCount == 1 {
			reports = append(reports, evaluateNodeTelemetry(name, n.Status.NodeInfo.Architecture, localTemp, sensorType, tempOK, localRaid, localRaidState, warnTemp, n.Spec.Unschedulable, true))
			continue
		}
		unknownReason := "UNKNOWN (uninspected remote node)"
		if matches[i] && matchCount > 1 {
			unknownReason = "UNKNOWN (ambiguous local identity)"
		}
		reports = append(reports, unknownNodeReport(name, n.Status.NodeInfo.Architecture, unknownReason, n.Spec.Unschedulable))
	}
	if matchCount == 0 {
		reports = append(reports, evaluateNodeTelemetry(localHost, runtime.GOARCH, localTemp, sensorType, tempOK, localRaid, localRaidState, warnTemp, false, false))
	}
	return reports, matchCount
}

func unknownNodeReport(name, arch, reason string, cordoned bool) nodeHealthReport {
	status := colorYellow + reason + colorReset
	if cordoned {
		status += " (CORDONED)"
	}
	return nodeHealthReport{
		Name: name, Arch: arch, TempDisplay: "UNKNOWN", RaidSummary: "UNKNOWN (no authenticated telemetry)",
		StatusDisplay: status, Critical: false, Inspected: false, Actionable: false,
	}
}

func evaluateNodeTelemetry(name, arch string, tempC float64, sensorType string, tempOK bool, raidSummary string, raidState RaidState, warnTemp float64, cordoned, actionable bool) nodeHealthReport {
	tempDisplay := "UNKNOWN (CPU sensor unavailable)"
	if tempOK {
		if sensorType != "" {
			tempDisplay = fmt.Sprintf("%.1f°C (%s)", tempC, sensorType)
		} else {
			tempDisplay = fmt.Sprintf("%.1f°C", tempC)
		}
	}

	var status string
	critical := false
	switch {
	case raidState == RaidDegraded:
		status, critical = colorRed+"CRITICAL: RAID DEGRADED"+colorReset, true
	case tempOK && tempC >= warnTemp:
		status, critical = colorRed+"CRITICAL: THERMAL THRESHOLD"+colorReset, true
	case raidState == RaidResyncing:
		status = colorYellow + "WARNING: RAID RESYNCING" + colorReset
	case !tempOK:
		status = colorYellow + "UNKNOWN (CPU sensor unavailable)" + colorReset
	case raidState == RaidUnknown:
		status = colorYellow + "UNKNOWN (RAID state unavailable)" + colorReset
	default:
		status = colorGreen + "HEALTHY" + colorReset
	}
	if cordoned {
		status += " (CORDONED)"
	}
	return nodeHealthReport{
		Name: name, Arch: arch, TempDisplay: tempDisplay, RaidSummary: raidSummary,
		StatusDisplay: status, Critical: critical, Inspected: tempOK || raidState != RaidUnknown,
		Actionable: actionable,
	}
}

func checkLinuxRAID(mdstatPath string, simulateFail bool, goos string) (string, RaidState) {
	if simulateFail {
		return "md0 [U_] (SIMULATED DEGRADED)", RaidDegraded
	}
	if goos != "linux" {
		return fmt.Sprintf("UNKNOWN (%s host)", goos), RaidUnknown
	}
	data, err := os.ReadFile(mdstatPath)
	if err != nil {
		return "UNKNOWN (cannot read /proc/mdstat)", RaidUnknown
	}
	return parseMdstat(string(data))
}

func parseMdstat(content string) (string, RaidState) {
	type arrayInfo struct {
		name       string
		detail     string
		state      RaidState
		active     bool
		hasMembers bool
	}
	var arrays []arrayInfo
	var current *arrayInfo
	flush := func() {
		if current == nil {
			return
		}
		if !current.active && current.state != RaidDegraded {
			current.state = RaidUnknown
			current.detail = current.name + " (inactive/unknown)"
		} else if !current.hasMembers && current.state != RaidResyncing && current.state != RaidDegraded {
			current.state = RaidUnknown
			current.detail = current.name + " (member state unavailable)"
		}
		arrays = append(arrays, *current)
		current = nil
	}

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if m := mdDeviceHeader.FindStringSubmatch(line); m != nil {
			flush()
			current = &arrayInfo{name: m[1], active: m[2] == "active", state: RaidUnknown, detail: m[1] + " (unknown)"}
		}
		if current == nil {
			continue
		}
		if m := mdMemberState.FindStringSubmatch(line); m != nil {
			members := m[1]
			current.hasMembers = true
			if strings.Contains(members, "_") {
				current.state = RaidDegraded
				current.detail = fmt.Sprintf("%s [%s] DEGRADED", current.name, members)
			} else if current.active {
				current.state = RaidHealthy
				current.detail = fmt.Sprintf("%s [%s]", current.name, members)
			}
		}
		if strings.Contains(line, "recovery") || strings.Contains(line, "resync") {
			if current.state != RaidDegraded {
				current.state = RaidResyncing
				current.detail = fmt.Sprintf("%s (RESYNCING)", current.name)
			}
		}
	}
	flush()
	if len(arrays) == 0 {
		return "NONE (0 active md arrays)", RaidNone
	}

	worst := RaidHealthy
	var summaries []string
	for _, array := range arrays {
		summaries = append(summaries, array.detail)
		if raidSeverity(array.state) > raidSeverity(worst) {
			worst = array.state
		}
	}
	return strings.Join(summaries, ", "), worst
}

func raidSeverity(state RaidState) int {
	switch state {
	case RaidDegraded:
		return 4
	case RaidUnknown:
		return 3
	case RaidResyncing:
		return 2
	case RaidHealthy:
		return 1
	default:
		return 0
	}
}

func readMaxThermalC(thermalRoot string) (float64, string, bool) {
	files, err := filepath.Glob(filepath.Join(thermalRoot, "thermal_zone*", "temp"))
	if err != nil || len(files) == 0 {
		return 0, "", false
	}
	maxTemp := -1.0
	var sensorType string
	for _, tempFile := range files {
		zoneDir := filepath.Dir(tempFile)
		typeBytes, err := os.ReadFile(filepath.Join(zoneDir, "type"))
		if err != nil {
			continue
		}
		zoneType := strings.TrimSpace(string(typeBytes))
		if !isCPUSensorType(zoneType) {
			continue
		}
		raw, err := os.ReadFile(tempFile)
		if err != nil {
			continue
		}
		milliC, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			continue
		}
		tempC := float64(milliC) / 1000
		if tempC <= 0 || tempC >= 135 {
			continue
		}
		if tempC > maxTemp {
			maxTemp, sensorType = tempC, zoneType
		}
	}
	if maxTemp < 0 {
		return 0, "", false
	}
	return maxTemp, sensorType, true
}

func isCPUSensorType(zoneType string) bool {
	lower := strings.ToLower(zoneType)
	for _, keyword := range []string{"x86_pkg", "cpu", "coretemp", "k10temp", "soc", "package", "tctl"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func fetchKubeNodes(kubeContext string) (kubeNodeList, error) {
	var list kubeNodeList
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(kubeContext, "get", "nodes", "-o", "json")...)
	out, err := cmd.Output()
	if err != nil {
		return list, err
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return list, fmt.Errorf("invalid kubectl JSON output: %w", err)
	}
	return list, nil
}

func kubectlArgs(kubeContext string, args ...string) []string {
	if strings.TrimSpace(kubeContext) == "" {
		return args
	}
	return append([]string{"--context", strings.TrimSpace(kubeContext)}, args...)
}

func verifyLicense(key string) bool {
	if !strings.HasPrefix(key, "HWPRO1.") || ed25519PubKeyHex == "" {
		return false
	}
	return verifyEd25519License(key, ed25519PubKeyHex)
}

func verifyEd25519License(token, pubHex string) bool {
	pubBytes, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "HWPRO1" || parts[1] == "" {
		return false
	}
	sigBytes, err := hex.DecodeString(parts[2])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false
	}
	message := []byte("HWPRO1." + parts[1])
	return ed25519.Verify(ed25519.PublicKey(pubBytes), message, sigBytes)
}

func runProAutomation(failingNodes []nodeHealthReport, autoCordon, autoDrain bool, webhook, licenseKey, kubeContext string, simulateMode bool) error {
	if autoCordon && autoDrain {
		return errors.New("choose either auto-cordon or auto-drain")
	}
	if strings.TrimSpace(kubeContext) == "" {
		return errors.New("an explicit Kubernetes context is required for actions")
	}
	if simulateMode {
		fmt.Printf("%s[DRY-RUN] Simulation is always non-mutating; no kubectl commands or webhook requests will be made.%s\n", colorYellow, colorReset)
		for _, node := range failingNodes {
			switch {
			case autoDrain:
				fmt.Printf("[DRY-RUN] Would drain verified local node %q (without deleting emptyDir data).\n", node.Name)
			case autoCordon:
				fmt.Printf("[DRY-RUN] Would cordon verified local node %q.\n", node.Name)
			}
			if webhook != "" {
				fmt.Printf("[DRY-RUN] Would send a webhook alert for node %q; request suppressed.\n", node.Name)
			}
		}
		return nil
	}
	if !verifyLicense(licenseKey) {
		return errors.New("live automated actions require a valid Ed25519-signed Pro key and a release build with its public key configured")
	}
	if len(failingNodes) == 0 {
		fmt.Println("No confirmed local hardware failure; no action needed.")
		return nil
	}

	for _, node := range failingNodes {
		if autoDrain {
			fmt.Printf("[ACTION] Draining verified local node %q (60 second timeout).\n", node.Name)
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(kubeContext, "drain", node.Name, "--ignore-daemonsets", "--timeout=60s")...)
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				return fmt.Errorf("kubectl drain failed for %q: %w (%s)", node.Name, err, strings.TrimSpace(string(out)))
			}
		} else if autoCordon {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			cmd := exec.CommandContext(ctx, "kubectl", kubectlArgs(kubeContext, "cordon", node.Name)...)
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				return fmt.Errorf("kubectl cordon failed for %q: %w (%s)", node.Name, err, strings.TrimSpace(string(out)))
			}
		}
		if webhook != "" {
			message := fmt.Sprintf("[kubectl-hwhealth] Node %q hardware alert. RAID: %s; thermal: %s; cordon: %v; drain: %v.", node.Name, node.RaidSummary, node.TempDisplay, autoCordon || autoDrain, autoDrain)
			if err := sendWebhookAlert(webhook, message); err != nil {
				return fmt.Errorf("webhook delivery failed: %w", err)
			}
		}
	}
	return nil
}

func validateWebhookURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("malformed webhook URL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return nil, errors.New("webhook must be an HTTPS URL without user information, query, or fragment")
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, errors.New("webhook URL must use port 443")
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "hooks.slack.com":
		if !strings.HasPrefix(u.EscapedPath(), "/services/") {
			return nil, errors.New("Slack webhook path must begin with /services/")
		}
	case "discord.com", "discordapp.com":
		if !strings.HasPrefix(u.EscapedPath(), "/api/webhooks/") {
			return nil, errors.New("Discord webhook path must begin with /api/webhooks/")
		}
	default:
		return nil, fmt.Errorf("webhook host %q is not an approved Slack or Discord host", host)
	}
	return u, nil
}

func sendWebhookAlert(rawURL, message string) error {
	u, err := validateWebhookURL(rawURL)
	if err != nil {
		return err
	}
	var payload map[string]string
	switch strings.ToLower(u.Hostname()) {
	case "hooks.slack.com":
		payload = map[string]string{"text": message}
	default:
		payload = map[string]string{"content": message}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialApprovedWebhookHost,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kubectl-hwhealth/0.2.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	return nil
}

// Resolve approved webhook domains immediately before dialing, reject any
// private/special-use answer, then connect to the validated IP directly. This
// avoids accepting arbitrary URLs and reduces DNS-rebinding/SSRF exposure.
func dialApprovedWebhookHost(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	host = strings.ToLower(host)
	if host != "hooks.slack.com" && host != "discord.com" && host != "discordapp.com" {
		return nil, fmt.Errorf("unapproved webhook host %q", host)
	}
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve webhook host %q: %w", host, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("webhook host %q resolved to no addresses", host)
	}
	for _, candidate := range resolved {
		if unsafeWebhookIP(candidate.IP) {
			return nil, fmt.Errorf("webhook host %q resolved to a private or special-use address", host)
		}
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, candidate := range resolved {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("connect to approved webhook host %q: %w", host, lastErr)
}

func unsafeWebhookIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}
