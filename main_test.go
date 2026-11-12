package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeTestNode(name, hostname string) kubeNode {
	var n kubeNode
	n.Metadata.Name = name
	if hostname != "" {
		n.Metadata.Labels = map[string]string{"kubernetes.io/hostname": hostname}
	}
	n.Status.NodeInfo.Architecture = "amd64"
	return n
}

func TestParseMdstatAggregatesWorstArrayState(t *testing.T) {
	sample := `Personalities : [raid1]
md0 : active raid1 sdb1[1] sda1[0]
      1047552 blocks super 1.2 [2/2] [UU]
      [==>..................]  resync = 12.4% (130432/1047552) finish=0.8min speed=18633K/sec

md1 : active raid1 sdd1[1] sdc1[0]
      20971520 blocks super 1.2 [2/1] [U_]

unused devices: <none>
`
	summary, state := parseMdstat(sample)
	if state != RaidDegraded {
		t.Fatalf("expected degraded worst state, got %v (%s)", state, summary)
	}
	if !strings.Contains(summary, "md1 [U_] DEGRADED") {
		t.Fatalf("degraded array missing from summary: %q", summary)
	}
}

func TestParseMdstatHealthyEmptyAndUnknown(t *testing.T) {
	healthy := "Personalities : [raid1]\nmd0 : active raid1 sda1[0] sdb1[1]\n 1047552 blocks super 1.2 [2/2] [UU]\n"
	_, state := parseMdstat(healthy)
	if state != RaidHealthy {
		t.Fatalf("expected healthy state, got %v", state)
	}

	_, state = parseMdstat("Personalities : \nunused devices: <none>\n")
	if state != RaidNone {
		t.Fatalf("expected no arrays, got %v", state)
	}

	_, state = parseMdstat("md0 : active raid1 sda1[0] sdb1[1]\n 1000 blocks super 1.2\n")
	if state != RaidUnknown {
		t.Fatalf("array without a member-state field must be unknown, got %v", state)
	}
}

func TestReadMaxThermalCUsesCPUZoneAndRejectsAmbientOnly(t *testing.T) {
	root := t.TempDir()
	writeZone := func(name, kind, temp string) {
		t.Helper()
		zone := filepath.Join(root, name)
		if err := os.MkdirAll(zone, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zone, "type"), []byte(kind+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zone, "temp"), []byte(temp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeZone("thermal_zone0", "acpitz", "65000")
	writeZone("thermal_zone1", "x86_pkg_temp", "58000")

	temp, sensor, ok := readMaxThermalC(root)
	if !ok || temp != 58 || sensor != "x86_pkg_temp" {
		t.Fatalf("expected CPU package reading 58C, got %.1f %q, ok=%v", temp, sensor, ok)
	}

	if err := os.RemoveAll(filepath.Join(root, "thermal_zone1")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readMaxThermalC(root); ok {
		t.Fatal("ambient-only reading must not be treated as a CPU temperature")
	}
}

func TestUnknownTelemetryNeverDisplaysHealthy(t *testing.T) {
	cases := []struct {
		name   string
		tempOK bool
		raid   RaidState
		temp   float64
	}{
		{name: "no temperature with no RAID", tempOK: false, raid: RaidNone},
		{name: "temperature with unknown RAID", tempOK: true, raid: RaidUnknown, temp: 40},
		{name: "nothing inspected", tempOK: false, raid: RaidUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := evaluateNodeTelemetry("node-a", "amd64", tc.temp, "", tc.tempOK, "n/a", tc.raid, 75, false, true)
			if strings.Contains(r.StatusDisplay, "HEALTHY") || !strings.Contains(r.StatusDisplay, "UNKNOWN") {
				t.Fatalf("partial/unknown telemetry must not report healthy: %q", r.StatusDisplay)
			}
			if r.Critical {
				t.Fatalf("unknown telemetry must not trigger a critical action: %+v", r)
			}
		})
	}
}

func TestClusterEvaluationOnlyTrustsUniqueLocalMatch(t *testing.T) {
	items := []kubeNode{makeTestNode("node-local", "node-local"), makeTestNode("node-remote", "node-remote")}
	reports, count := evaluateClusterNodes(items, "node-local", false, 90, "x86_pkg_temp", true, "md0 [U_] DEGRADED", RaidDegraded, 75)
	if count != 1 || !reports[0].Critical || !reports[0].Actionable {
		t.Fatalf("expected only the matched local node to be actionable: count=%d reports=%+v", count, reports)
	}
	if reports[1].Critical || reports[1].Actionable || !strings.Contains(reports[1].StatusDisplay, "UNKNOWN") {
		t.Fatalf("remote node must remain unknown and non-actionable: %+v", reports[1])
	}

	ambiguous := []kubeNode{makeTestNode("node-a", "host-x"), makeTestNode("node-b", "host-x")}
	reports, count = evaluateClusterNodes(ambiguous, "host-x", false, 90, "cpu", true, "md0 [U_]", RaidDegraded, 75)
	if count != 2 {
		t.Fatalf("expected ambiguous host identity, got %d matches", count)
	}
	for _, r := range reports {
		if r.Actionable || r.Critical || !strings.Contains(r.StatusDisplay, "ambiguous") {
			t.Fatalf("ambiguous identity must fail closed: %+v", r)
		}
	}
}

func TestLicenseAcceptsOnlyEd25519SignedKeys(t *testing.T) {
	old := ed25519PubKeyHex
	defer func() { ed25519PubKeyHex = old }()
	ed25519PubKeyHex = ""
	if verifyLicense("PRO-DEMO-2026") || verifyLicense("HWPRO-ABCD-123456") {
		t.Fatal("demo and legacy forgeable formats must not authorize live actions")
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ed25519PubKeyHex = hex.EncodeToString(pub)
	message := "HWPRO1.CUSTOMER"
	token := message + "." + hex.EncodeToString(ed25519.Sign(priv, []byte(message)))
	if !verifyLicense(token) {
		t.Fatal("expected correctly signed token to verify")
	}
	if verifyLicense("HWPRO1.CUSTOMER." + strings.Repeat("00", ed25519.SignatureSize)) {
		t.Fatal("invalid signature must be rejected")
	}
}

func TestRunRejectsActionWithoutContext(t *testing.T) {
	if code := run([]string{"--auto-cordon"}); code != 2 {
		t.Fatalf("expected CLI usage failure without --context, got exit code %d", code)
	}
}

func TestAutomationRequiresExplicitContext(t *testing.T) {
	err := runProAutomation(nil, true, false, "", "", "", true)
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected missing context to be rejected, got %v", err)
	}
}

func TestSimulationNeverRunsKubectlOrWebhook(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "kubectl-was-called")
	stub := filepath.Join(dir, "kubectl")
	script := "#!/bin/sh\nprintf called > \"" + sentinel + "\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := runProAutomation([]nodeHealthReport{{Name: "node-local", RaidSummary: "simulated", TempDisplay: "90C"}}, true, false, "http://127.0.0.1/not-a-real-webhook", "a-production-looking-key", "test-context", true)
	if err != nil {
		t.Fatalf("simulation should only print a dry-run plan: %v", err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("simulation invoked kubectl or created sentinel: stat err=%v", err)
	}
}

func TestKubectlArgsPinsContext(t *testing.T) {
	got := kubectlArgs("staging", "get", "nodes")
	want := []string{"--context", "staging", "get", "nodes"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("expected explicit context args %v, got %v", want, got)
	}
	if got := kubectlArgs("", "get", "nodes"); strings.Join(got, " ") != "get nodes" {
		t.Fatalf("unexpected args without a context: %v", got)
	}
}

func TestWebhookURLAllowlist(t *testing.T) {
	valid := []string{
		"https://hooks.slack.com/services/T00/B00/X00",
		"https://discord.com/api/webhooks/123/token",
		"https://discordapp.com/api/webhooks/123/token",
	}
	for _, value := range valid {
		if _, err := validateWebhookURL(value); err != nil {
			t.Errorf("expected allowed webhook URL %q: %v", value, err)
		}
	}
	invalid := []string{
		"http://hooks.slack.com/services/T00/B00/X00",
		"https://127.0.0.1/services/x",
		"https://attacker.example/services/x",
		"https://user@hooks.slack.com/services/x",
		"https://hooks.slack.com:8443/services/x",
		"https://hooks.slack.com/other/path",
		"https://hooks.slack.com/services/x?next=127.0.0.1",
	}
	for _, value := range invalid {
		if _, err := validateWebhookURL(value); err == nil {
			t.Errorf("expected webhook URL %q to be rejected", value)
		}
	}
}

func TestUnsafeWebhookIPRejectsSpecialUseAddresses(t *testing.T) {
	unsafe := []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "0.0.0.0", "224.0.0.1"}
	for _, value := range unsafe {
		if !unsafeWebhookIP(net.ParseIP(value)) {
			t.Errorf("expected %s to be rejected", value)
		}
	}
	if unsafeWebhookIP(net.ParseIP("8.8.8.8")) {
		t.Error("expected a public unicast IP to pass the address check")
	}
}
