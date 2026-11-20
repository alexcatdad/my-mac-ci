// Package netrules embeds the trial-verified firewall rules in the signed daemon.
package netrules

import _ "embed"

//go:embed mmc.pf.conf
var Rules string
