package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

type RaidState int

const (
	RaidUnknown RaidState = iota
	RaidNone
	RaidHealthy
	RaidResyncing
	RaidDegraded
)

// Injected at release build time via -ldflags "-X main.ed25519PubKeyHex=..."
// Default value is a 32-byte Ed25519 public key; private signing key is never stored in repo.
var ed25519PubKeyHex = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"

// Optional cluster-wide Ed25519 public key for verifying remote node annotations
var ed25519NodePubKeyHex = ""

type kubeNode struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
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
}

var mdDeviceHeader = regexp.MustCompile(`^(md\d+)\s*:\s*(\S+)\s+(\S+)`)
var mdMemberState = regexp.MustCompile(`\[([U_]+)\]`)

func main() {
	warnTemp := flag.Float64("warn-temp", 75.0, "CPU/package temperature threshold in Celsius")
	nodeNameFlag := flag.String("node-name", "", "Explicit Kubernetes node name corresponding to this host (defaults to hostname)")
	publishStatus := flag.Bool("publish-node-status", false, "Sign and publish local hardware telemetry annotations to the local Kubernetes node")
	nodeSigningKeyFlag := flag.String("node-signing-key", "", "Ed25519 private key (64-byte hex or 32-byte seed hex) for --publish-node-status (or HWHEALTH_NODE_SIGNING_KEY env)")
	nodeVerifyPubFlag := flag.String("node-verify-pubkey", "", "Ed25519 public key (32-byte hex) to verify remote node annotations (or HWHEALTH_NODE_VERIFY_PUBKEY env)")
	annotationMaxAge := flag.Duration("annotation-max-age", 5*time.Minute, "Maximum allowed age for remote node telemetry annotations")
	simulateFail := flag.Bool("simulate-raid-fail", false, "Simulate a degraded RAID array (strictly dry-run; never mutates live cluster)")
	autoCordon := flag.Bool("auto-cordon", false, "[PRO] Automatically cordon verified failing nodes (kubectl cordon)")
	autoDrain := flag.Bool("auto-drain", false, "[PRO] Automatically cordon AND evict pods on verified failing nodes (kubectl drain)")
	webhookURL := flag.String("webhook-url", "", "[PRO] HTTPS Slack or Discord webhook URL for hardware failure alerts")
	licenseKeyFlag := flag.String("license-key", "", "Pro Ed25519 license token HWPRO1.<id>.<sig> (or HWHEALTH_LICENSE_KEY env)")
	flag.Parse()

	if err := validateFlagCombinations(*simulateFail, *publishStatus); err != nil {
		fmt.Fprintf(os.Stderr, "%s[ERROR] %v%s\n", colorRed, err, colorReset)
		os.Exit(1)
	}

	licenseKey := firstNonEmpty(*licenseKeyFlag, os.Getenv("HWHEALTH_LICENSE_KEY"))
	nodeSigningKey := firstNonEmpty(*nodeSigningKeyFlag, os.Getenv("HWHEALTH_NODE_SIGNING_KEY"))
	nodeVerifyPub := firstNonEmpty(*nodeVerifyPubFlag, os.Getenv("HWHEALTH_NODE_VERIFY_PUBKEY"), ed25519NodePubKeyHex)

	fmt.Printf("%s=== kubectl-hwhealth v0.2.0: Bare-Metal RAID & Thermal Scanner ===%s\n\n", colorCyan, colorReset)

	localHost := resolveLocalNodeName(*nodeNameFlag)
	cpuTemp, sensorType, tempOK := readMaxThermalC("/sys/class/thermal")
	raidSummary, raidState := checkLinuxRAID("/proc/mdstat", *simulateFail, runtime.GOOS)

	if *publishStatus {
		if err := annotateLocalNode(localHost, cpuTemp, tempOK, raidSummary, raidState, nodeSigningKey, time.Now().UTC()); err != nil {
			fmt.Fprintf(os.Stderr, "%s[ERROR] Failed to publish node annotations for '%s': %v%s\n", colorRed, localHost, err, colorReset)
			os.Exit(1)
		}
		fmt.Printf("%s[OK] Published signed hardware telemetry annotations to node '%s'.%s\n\n", colorGreen, localHost, colorReset)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NODE\tARCH\tTHERMAL\tRAID ARRAY\tHARDWARE STATUS")

	nodes, kubeErr := fetchKubeNodes()
	var reports []nodeHealthReport
	now := time.Now().UTC()

	if kubeErr != nil {
		fmt.Fprintf(os.Stderr, "%s[!] kubectl get nodes failed (%v) — reporting local host '%s' only%s\n\n", colorYellow, kubeErr, localHost, colorReset)
		rep := evaluateNodeTelemetry(localHost, runtime.GOARCH, cpuTemp, sensorType, tempOK, raidSummary, raidState, *warnTemp, false)
		reports = append(reports, rep)
	} else if len(nodes.Items) == 0 {
		rep := evaluateNodeTelemetry(localHost, runtime.GOARCH, cpuTemp, sensorType, tempOK, raidSummary, raidState, *warnTemp, false)
		reports = append(reports, rep)
	} else {
		reports = evaluateClusterNodes(nodes.Items, localHost, cpuTemp, sensorType, tempOK, raidSummary, raidState, *warnTemp, now, *annotationMaxAge, nodeVerifyPub)
	}

	var failingNodes []nodeHealthReport
	for _, r := range reports {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Arch, r.TempDisplay, r.RaidSummary, r.StatusDisplay)
		if r.Critical && r.Inspected {
			failingNodes = append(failingNodes, r)
		}
	}
	w.Flush()

	wantProAction := *autoCordon || *autoDrain || *webhookURL != ""
	if len(failingNodes) > 0 && !wantProAction {
		fmt.Printf("\n%s[!] VERIFIED HARDWARE FAILURE on %d node(s).%s\n", colorRed, len(failingNodes), colorReset)
		fmt.Println("-> Pass --auto-cordon, --auto-drain, or --webhook-url with a Pro license key to remediate:")
		fmt.Println("   kubectl hwhealth --auto-cordon --webhook-url=https://... --license-key=HWPRO1.<ID>.<SIG>")
		os.Exit(2)
	}

	if wantProAction {
		if err := runProAutomation(failingNodes, *autoCordon, *autoDrain, *webhookURL, licenseKey, *simulateFail); err != nil {
			fmt.Fprintf(os.Stderr, "%s[ERROR] %v%s\n", colorRed, err, colorReset)
			os.Exit(1)
		}
	}

	if len(failingNodes) > 0 && !*simulateFail {
		os.Exit(2)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// validateFlagCombinations prevents simulated failures from ever writing fake telemetry to live nodes.
func validateFlagCombinations(simulateFail, publishStatus bool) error {
	if simulateFail && publishStatus {
		return errors.New("--simulate-raid-fail cannot be combined with --publish-node-status (refusing to publish simulated failure state to live cluster annotations)")
	}
	return nil
}

func resolveLocalNodeName(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local-host"
	}
	return h
}

// evaluateClusterNodes binds local hardware readings strictly to the matching local node.
// Remote nodes require BOTH a fresh RFC3339 timestamp (<= maxAge) AND a valid Ed25519 signature.
func evaluateClusterNodes(
	items []kubeNode,
	localHost string,
	localTemp float64,
	sensorType string,
	tempOK bool,
	localRaid string,
	localRaidState RaidState,
	warnTemp float64,
	now time.Time,
	maxAge time.Duration,
	nodeVerifyPubHex string,
) []nodeHealthReport {
	var out []nodeHealthReport
	for _, n := range items {
		name := n.Metadata.Name
		arch := n.Status.NodeInfo.Architecture
		hostLabel := n.Metadata.Labels["kubernetes.io/hostname"]

		if strings.EqualFold(name, localHost) || (hostLabel != "" && strings.EqualFold(hostLabel, localHost)) {
			rep := evaluateNodeTelemetry(name, arch, localTemp, sensorType, tempOK, localRaid, localRaidState, warnTemp, n.Spec.Unschedulable)
			out = append(out, rep)
			continue
		}

		ann := n.Metadata.Annotations
		if ann == nil || ann["hwhealth.io/raid-state"] == "" {
			out = append(out, uninspectedReport(name, arch, "UNKNOWN (uninspected remote node)", n.Spec.Unschedulable))
			continue
		}

		// 1. Verify annotation freshness (TTL)
		updatedStr := strings.TrimSpace(ann["hwhealth.io/updated-at"])
		updatedAt, err := time.Parse(time.RFC3339, updatedStr)
		if err != nil || updatedAt.After(now.Add(30*time.Second)) || now.Sub(updatedAt) > maxAge {
			out = append(out, uninspectedReport(name, arch, "UNKNOWN (stale/expired node annotation)", n.Spec.Unschedulable))
			continue
		}

		// 2. Verify Ed25519 node annotation signature
		if !verifyNodeAnnotationSignature(name, ann, nodeVerifyPubHex) {
			out = append(out, uninspectedReport(name, arch, "UNKNOWN (unsigned/unverified annotation)", n.Spec.Unschedulable))
			continue
		}

		rState := parseRaidStateString(ann["hwhealth.io/raid-state"])
		rSummary := ann["hwhealth.io/raid-summary"]
		if rSummary == "" {
			rSummary = ann["hwhealth.io/raid-state"]
		}
		tVal, tErr := strconv.ParseFloat(ann["hwhealth.io/cpu-temp-c"], 64)
		rep := evaluateNodeTelemetry(name, arch, tVal, "verified-agent", tErr == nil && tVal > 0, rSummary, rState, warnTemp, n.Spec.Unschedulable)
		out = append(out, rep)
	}
	return out
}

func uninspectedReport(name, arch, reason string, cordoned bool) nodeHealthReport {
	status := colorYellow + reason + colorReset
	if cordoned {
		status += " (CORDONED)"
	}
	return nodeHealthReport{
		Name:          name,
		Arch:          arch,
		TempDisplay:   "UNKNOWN",
		RaidSummary:   "UNKNOWN",
		StatusDisplay: status,
		Critical:      false,
		Inspected:     false,
	}
}

func canonicalNodeAnnotationPayload(nodeName, raidState, raidSummary, cpuTemp, updatedAt string) []byte {
	return []byte(fmt.Sprintf("hwhealth-v2|%s|%s|%s|%s|%s", nodeName, raidState, raidSummary, cpuTemp, updatedAt))
}

func verifyNodeAnnotationSignature(nodeName string, ann map[string]string, pubHex string) bool {
	pubBytes, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return false
	}
	sigBytes, err := hex.DecodeString(strings.TrimSpace(ann["hwhealth.io/signature"]))
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return false
	}
	payload := canonicalNodeAnnotationPayload(
		nodeName,
		ann["hwhealth.io/raid-state"],
		ann["hwhealth.io/raid-summary"],
		ann["hwhealth.io/cpu-temp-c"],
		ann["hwhealth.io/updated-at"],
	)
	return ed25519.Verify(ed25519.PublicKey(pubBytes), payload, sigBytes)
}

func parseEd25519PrivateKey(keyHex string) (ed25519.PrivateKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil {
		return nil, fmt.Errorf("invalid hex key: %v", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("expected 32-byte seed or 64-byte Ed25519 private key, got %d bytes", len(raw))
	}
}

func annotateLocalNode(nodeName string, tempC float64, tempOK bool, raidSummary string, raidState RaidState, signingKeyHex string, now time.Time) error {
	if strings.TrimSpace(signingKeyHex) == "" {
		return errors.New("--publish-node-status requires an Ed25519 signing key (--node-signing-key or HWHEALTH_NODE_SIGNING_KEY)")
	}
	privKey, err := parseEd25519PrivateKey(signingKeyHex)
	if err != nil {
		return err
	}

	rStateStr := raidStateToString(raidState)
	tempStr := "UNKNOWN"
	if tempOK {
		tempStr = fmt.Sprintf("%.1f", tempC)
	}
	updatedAt := now.UTC().Format(time.RFC3339)
	payload := canonicalNodeAnnotationPayload(nodeName, rStateStr, raidSummary, tempStr, updatedAt)
	sigHex := hex.EncodeToString(ed25519.Sign(privKey, payload))

	args := []string{
		"annotate", "node", nodeName, "--overwrite",
		fmt.Sprintf("hwhealth.io/raid-state=%s", rStateStr),
		fmt.Sprintf("hwhealth.io/raid-summary=%s", raidSummary),
		fmt.Sprintf("hwhealth.io/cpu-temp-c=%s", tempStr),
		fmt.Sprintf("hwhealth.io/updated-at=%s", updatedAt),
		fmt.Sprintf("hwhealth.io/signature=%s", sigHex),
	}
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func evaluateNodeTelemetry(name, arch string, tempC float64, sensorType string, tempOK bool, raidSummary string, raidState RaidState, warnTemp float64, cordoned bool) nodeHealthReport {
	tempDisp := "UNKNOWN (no sensor)"
	if tempOK {
		if sensorType != "" {
			tempDisp = fmt.Sprintf("%.1f°C (%s)", tempC, sensorType)
		} else {
			tempDisp = fmt.Sprintf("%.1f°C", tempC)
		}
	}

	var status string
	var critical bool

	switch {
	case raidState == RaidDegraded:
		status = colorRed + "CRITICAL: RAID DEGRADED" + colorReset
		critical = true
	case tempOK && tempC >= warnTemp:
		status = colorRed + "CRITICAL: THERMAL THRESHOLD" + colorReset
		critical = true
	case raidState == RaidResyncing:
		status = colorYellow + "WARNING: RAID RESYNCING" + colorReset
	case !tempOK && raidState == RaidUnknown:
		status = colorYellow + "UNKNOWN (sensors unavailable)" + colorReset
	default:
		status = colorGreen + "HEALTHY" + colorReset
	}

	if cordoned {
		status += " (CORDONED)"
	}

	return nodeHealthReport{
		Name:          name,
		Arch:          arch,
		TempDisplay:   tempDisp,
		RaidSummary:   raidSummary,
		StatusDisplay: status,
		Critical:      critical,
		Inspected:     tempOK || raidState != RaidUnknown,
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
		if os.IsNotExist(err) {
			return "NONE (mdraid not present)", RaidNone
		}
		return "UNKNOWN (cannot read /proc/mdstat)", RaidUnknown
	}
	return parseMdstat(string(data))
}

func parseMdstat(content string) (string, RaidState) {
	lines := strings.Split(content, "\n")
	type arrayInfo struct {
		name   string
		detail string
		state  RaidState
	}
	var arrays []arrayInfo
	var current *arrayInfo

	flushCurrent := func() {
		if current != nil {
			arrays = append(arrays, *current)
			current = nil
		}
	}

	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if matches := mdDeviceHeader.FindStringSubmatch(line); matches != nil {
			flushCurrent()
			devName := matches[1]
			devState := matches[2]
			st := RaidHealthy
			if devState != "active" {
				st = RaidDegraded
			}
			current = &arrayInfo{
				name:   devName,
				detail: devName + "[healthy]",
				state:  st,
			}
		}
		if current == nil {
			continue
		}
		if m := mdMemberState.FindStringSubmatch(line); m != nil {
			members := m[1]
			if strings.Contains(members, "_") {
				current.state = RaidDegraded
				current.detail = fmt.Sprintf("%s [%s] DEGRADED", current.name, members)
			} else if current.state != RaidDegraded {
				current.detail = fmt.Sprintf("%s [%s]", current.name, members)
			}
		}
		if (strings.Contains(line, "recovery") || strings.Contains(line, "resync")) && current.state != RaidDegraded {
			current.state = RaidResyncing
			current.detail = fmt.Sprintf("%s (RESYNCING)", current.name)
		}
	}
	flushCurrent()

	if len(arrays) == 0 {
		return "NONE (0 active md arrays)", RaidNone
	}

	worstState := RaidHealthy
	var summaries []string
	for _, a := range arrays {
		summaries = append(summaries, a.detail)
		if a.state > worstState {
			worstState = a.state
		}
	}
	return strings.Join(summaries, ", "), worstState
}

func parseRaidStateString(s string) RaidState {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEGRADED":
		return RaidDegraded
	case "RESYNCING":
		return RaidResyncing
	case "HEALTHY":
		return RaidHealthy
	case "NONE":
		return RaidNone
	default:
		return RaidUnknown
	}
}

func raidStateToString(s RaidState) string {
	switch s {
	case RaidDegraded:
		return "DEGRADED"
	case RaidResyncing:
		return "RESYNCING"
	case RaidHealthy:
		return "HEALTHY"
	case RaidNone:
		return "NONE"
	default:
		return "UNKNOWN"
	}
}

func readMaxThermalC(thermalRoot string) (float64, string, bool) {
	files, err := filepath.Glob(filepath.Join(thermalRoot, "thermal_zone*", "temp"))
	if err != nil || len(files) == 0 {
		return 0, "", false
	}
	maxCPU := -1.0
	var cpuType string
	maxAny := -1.0
	var anyType string

	for _, tempFile := range files {
		rawBytes, err := os.ReadFile(tempFile)
		if err != nil {
			continue
		}
		val, err := strconv.Atoi(strings.TrimSpace(string(rawBytes)))
		if err != nil {
			continue
		}
		c := float64(val) / 1000.0
		if c <= 0.0 || c >= 135.0 {
			continue
		}

		zoneDir := filepath.Dir(tempFile)
		typeBytes, _ := os.ReadFile(filepath.Join(zoneDir, "type"))
		zType := strings.TrimSpace(string(typeBytes))
		if zType == "" {
			zType = "thermal"
		}

		if isCPUSensorType(zType) && c > maxCPU {
			maxCPU = c
			cpuType = zType
		}
		if c > maxAny {
			maxAny = c
			anyType = zType
		}
	}

	if maxCPU > 0 {
		return maxCPU, cpuType, true
	}
	if maxAny > 0 {
		return maxAny, anyType, true
	}
	return 0, "", false
}

func isCPUSensorType(zoneType string) bool {
	lower := strings.ToLower(zoneType)
	for _, kw := range []string{"x86_pkg", "cpu", "coretemp", "k10temp", "soc", "package", "tctl"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func fetchKubeNodes() (kubeNodeList, error) {
	var list kubeNodeList
	cmd := exec.Command("kubectl", "get", "nodes", "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return list, err
	}
	err = json.Unmarshal(out, &list)
	return list, err
}

// verifyLicense strictly validates Ed25519 asymmetric tokens (HWPRO1.<CustomerID>.<128-hex-char-Ed25519-sig>).
// No symmetric secrets exist in the binary. Furthermore, whenever simulateMode is true, dryRunOnly is forced true
// even if a valid production license key is supplied.
func verifyLicense(key string, simulateMode bool) (valid bool, dryRunOnly bool) {
	if key == "PRO-DEMO-2026" {
		if simulateMode {
			return true, true
		}
		return false, false
	}

	if strings.HasPrefix(key, "HWPRO1.") && ed25519PubKeyHex != "" {
		if verifyEd25519License(key, ed25519PubKeyHex) {
			return true, simulateMode
		}
	}
	return false, false
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
	return ed25519.Verify(ed25519.PublicKey(pubBytes), []byte("HWPRO1."+parts[1]), sigBytes)
}

func runProAutomation(failingNodes []nodeHealthReport, autoCordon, autoDrain bool, webhook, licenseKey string, simulateMode bool) error {
	fmt.Println()
	valid, dryRunFromLicense := verifyLicense(licenseKey, simulateMode)
	if !valid {
		if licenseKey == "PRO-DEMO-2026" && !simulateMode {
			return errors.New("demo key PRO-DEMO-2026 is restricted to --simulate-raid-fail dry-run mode; use an Ed25519 production key (HWPRO1.<ID>.<SIG>) for live cluster remediation")
		}
		return errors.New("automated node cordoning/draining and webhook alerts require a valid Ed25519 Pro license key (HWPRO1.<ID>.<SIG>)")
	}

	// Hard safety invariant: simulation mode NEVER executes live kubectl or webhook calls, even with a production key.
	isDryRun := simulateMode || dryRunFromLicense

	if isDryRun {
		fmt.Printf("%s[PRO DRY-RUN] Simulation mode active — no live kubectl commands or webhooks will be executed.%s\n", colorYellow, colorReset)
	} else {
		fmt.Printf("%s[PRO VERIFIED] Ed25519 signature verified.%s\n", colorGreen, colorReset)
	}

	if len(failingNodes) == 0 {
		fmt.Println("-> All inspected nodes are within healthy thresholds. No remediation needed.")
		return nil
	}

	for _, n := range failingNodes {
		if autoDrain {
			if isDryRun {
				fmt.Printf("%s[DRY-RUN] Would execute: kubectl drain %s --ignore-daemonsets --delete-emptydir-data --timeout=60s%s\n", colorYellow, n.Name, colorReset)
			} else {
				fmt.Printf("%s[ACTION] Draining node '%s' (respecting PodDisruptionBudgets)...%s\n", colorRed, n.Name, colorReset)
				cmd := exec.Command("kubectl", "drain", n.Name, "--ignore-daemonsets", "--delete-emptydir-data", "--timeout=60s")
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("kubectl drain failed on %s: %v (%s)", n.Name, err, strings.TrimSpace(string(out)))
				}
			}
		} else if autoCordon {
			if isDryRun {
				fmt.Printf("%s[DRY-RUN] Would execute: kubectl cordon %s%s\n", colorYellow, n.Name, colorReset)
			} else {
				fmt.Printf("%s[ACTION] Cordoning node '%s' to prevent new pod scheduling...%s\n", colorRed, n.Name, colorReset)
				cmd := exec.Command("kubectl", "cordon", n.Name)
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("kubectl cordon failed on %s: %v (%s)", n.Name, err, strings.TrimSpace(string(out)))
				}
			}
		}

		if webhook != "" {
			if _, err := validateWebhookURL(webhook); err != nil {
				return err
			}
			msg := fmt.Sprintf("🚨 [kubectl-hwhealth] Node `%s` hardware alert! RAID: `%s` | Thermal: `%s` | Cordon: `%v` | Drain: `%v`",
				n.Name, n.RaidSummary, n.TempDisplay, autoCordon || autoDrain, autoDrain)
			if isDryRun {
				fmt.Printf("%s[DRY-RUN] Would POST HTTPS alert to webhook: %s%s\n", colorYellow, msg, colorReset)
			} else {
				if err := sendWebhookAlert(webhook, msg); err != nil {
					return fmt.Errorf("webhook delivery failed: %v", err)
				}
				fmt.Printf("%s[ALERT SENT] Verified 2xx webhook response for node '%s'.%s\n", colorGreen, n.Name, colorReset)
			}
		}
	}
	return nil
}

func validateWebhookURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("malformed webhook URL: %v", err)
	}
	if u.Scheme != "https" {
		return nil, errors.New("webhook URL must use https://")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return nil, fmt.Errorf("disallowed webhook host %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return nil, fmt.Errorf("webhook IP %s must not be loopback or private", ip.String())
		}
	}
	return u, nil
}

func sendWebhookAlert(rawURL, message string) error {
	u, err := validateWebhookURL(rawURL)
	if err != nil {
		return err
	}
	payload := map[string]string{
		"text":    message,
		"content": message,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewBuffer(body))
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
