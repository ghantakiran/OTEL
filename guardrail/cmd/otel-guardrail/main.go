// Command otel-guardrail runs Guardrails over a service's declared telemetry.
package main

import (
	"os"

	"github.com/ghantakiran/OTEL/guardrail/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
