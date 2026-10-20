package market

import (
	"time"
)

const regtestOperatorTimeout = 120 * time.Second

func errorCauseOrNone(err error) string {
	if err == nil {
		return "no error"
	}
	return watcherCause(err)
}
