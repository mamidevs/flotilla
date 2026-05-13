package buildinfo

import (
	"fmt"
)

// These variables will be set at build time via -ldflags.
var (
	AppName    = "flotilla"
	AppVersion = "canary"
	BuildId    string
	CommitHash string
	BuildDate  string
	Production string
)

// BuildDescription set during initialization.
var BuildDescription string

func init() {
	if BuildId != "" && BuildDate != "" && CommitHash != "" {
		BuildDescription = fmt.Sprintf("%s, %s (%s)", BuildId, BuildDate, CommitHash)
	} else {
		BuildDescription = "null"
	}

	if Production != "1" {
		BuildDescription += " (non-production)"
	}
}
