// Package hostcontrol implements a narrowly scoped, local application-host
// reboot request. The root agent trusts the configured application UID to
// perform administrator authorization, fresh MFA and durable audit first.
// It never accepts a command, host target, unlock key or passphrase.
package hostcontrol

import "time"

const ProtocolVersion = 1
const RebootAction = "reboot-app-host"

type Request struct {
	Version     int       `json:"version"`
	Action      string    `json:"action"`
	RequestID   string    `json:"request_id"`
	RequestedAt time.Time `json:"requested_at"`
}

type Receipt struct {
	RequestID  string    `json:"request_id"`
	State      string    `json:"state"`
	AcceptedAt time.Time `json:"accepted_at"`
	RebootAt   time.Time `json:"reboot_at"` // Earliest time; flushing always precedes a full grace.
}

type Status struct {
	Ready   bool     `json:"ready"`
	Reason  string   `json:"reason"`
	Pending *Receipt `json:"pending,omitempty"`
	Last    *Receipt `json:"last,omitempty"`
}

// UnlockPolicy is an operator acknowledgement, not evidence of an unlock test.
// Only the manual root operator workflow may change UnlockRehearsed to true.
type UnlockPolicy struct {
	Version         int    `json:"version"`
	DeploymentDir   string `json:"deployment_dir"`
	UnlockRehearsed bool   `json:"unlock_rehearsed"`
	BootID          string `json:"boot_id"`
}
