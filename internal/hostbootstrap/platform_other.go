//go:build !linux

package hostbootstrap

import (
	"context"
	"errors"
	"io"
)

func unsupported() error {
	return errors.New("host mutation requires Ubuntu 24.04 Linux root; host plan is portable")
}
func applyHost(context.Context, Config, io.Writer) error       { return unsupported() }
func confirmHost(context.Context, Config, io.Writer) error     { return unsupported() }
func rollbackHost(context.Context, io.Writer) error            { return unsupported() }
func installPackages(context.Context, Config, io.Writer) error { return unsupported() }

func LoadFirewall(context.Context) error { return unsupported() }
