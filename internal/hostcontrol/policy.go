package hostcontrol

import "errors"

var errUnlockBootChanged = errors.New("unlock readiness must be acknowledged for the current OS boot")

func decodeUnlockPolicy(data []byte, deployment, currentBoot string) (UnlockPolicy, error) {
	var policy UnlockPolicy
	if decodeObject(data, &policy) != nil || policy.Version != ProtocolVersion || policy.DeploymentDir != deployment || policy.BootID != "" && !validBootID(policy.BootID) {
		return UnlockPolicy{}, errors.New("host control unlock policy invalid")
	}
	if policy.UnlockRehearsed && (!validBootID(currentBoot) || policy.BootID != currentBoot) {
		// Return the actual true policy so startup never mistakes a stale
		// acknowledgement for permission to recreate missing receipt history.
		return policy, errUnlockBootChanged
	}
	return policy, nil
}
