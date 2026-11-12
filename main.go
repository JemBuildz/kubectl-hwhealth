package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/jembuildz/kubectl-hwhealth/pkg/health"
)

const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
	colorCyan   = "\033[36m"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("kubectl-hwhealth", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	warnTemp := fs.Float64("warn-temp", health.DefaultWarnTempC, "CPU/package temperature threshold in Celsius")
	nodeName := fs.String("node-name", "", "Kubernetes node name for this host; set explicitly if hostname differs")
	kubeContext := fs.String("context", "", "Kubernetes context to inspect; defaults to the current kubectl context")
	simulateFail := fs.Bool("simulate-raid-fail", false, "Display a simulated degraded RAID result; no action is performed")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		return 2
	}

	fmt.Printf("%s=== kubectl-hwhealth: Bare-Metal RAID & Thermal Scanner ===%s\n\n", colorCyan, colorReset)
	result, err := health.Scan(context.Background(), health.Options{
		KubernetesContext: *kubeContext,
		NodeName:          *nodeName,
		WarnTempC:         *warnTemp,
		SimulateRAIDFail:  *simulateFail,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		return 2
	}
	if result.KubernetesError != nil {
		fmt.Fprintf(os.Stderr, "[!] %v; reporting this host only\n", result.KubernetesError)
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(writer, "NODE\tARCH\tTHERMAL\tRAID ARRAY\tHARDWARE STATUS")
	anyCritical := false
	for _, report := range result.Reports {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			report.Name,
			report.Arch,
			report.TempDisplay,
			report.RaidSummary,
			colorStatus(report.StatusDisplay),
		)
		if report.Critical {
			anyCritical = true
		}
	}
	_ = writer.Flush()

	if anyCritical {
		return 2
	}
	return 0
}

func colorStatus(status string) string {
	switch {
	case strings.HasPrefix(status, "CRITICAL:"):
		return colorRed + status + colorReset
	case strings.HasPrefix(status, "WARNING:") || strings.HasPrefix(status, "UNKNOWN"):
		return colorYellow + status + colorReset
	case strings.HasPrefix(status, "HEALTHY"):
		return colorGreen + status + colorReset
	default:
		return status
	}
}
