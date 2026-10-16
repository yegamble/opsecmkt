// Package emergency provides explicitly targeted, manual incident operations.
// It never monitors events or automatically erases anything.
package emergency

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"opsecmkt/internal/operator"
	"opsecmkt/internal/ramsecrets"
)

const usage = `emergency plan --dir DIR [--device /dev/DEVICE] [--ram-dir /run/opsecmkt-secrets/INSTANCE] [--reboot]
emergency stop --dir DIR --confirm-project PROJECT [--ram-dir /run/opsecmkt-secrets/INSTANCE]
emergency erase-luks --dir DIR --confirm-project PROJECT --device /dev/DEVICE --confirm-device /dev/DEVICE [--ram-dir /run/opsecmkt-secrets/INSTANCE] [--reboot]
Manual operations only. stop kills/removes the selected Compose containers, retaining volumes.
Docker uses the saved context; RAM cleanup, LUKS erasure and reboot target the host running this CLI.
erase-luks also destroys every LUKS keyslot on the named block device. Existing header backups or live mappings can still expose data. --reboot requests immediate unsynced reboot after erasure.
No canary, provider inquiry or other external signal invokes these commands automatically.
`

type options struct {
	command, dir, confirmProject, device, confirmDevice, ramDir string
	reboot                                                      bool
}
type host interface {
	checkDevice(context.Context, string) error
	erase(context.Context, string) error
	reboot() error
}

// Run requires exact saved project and block-device confirmations for mutations.
// It does not take secrets or keys in argv.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := io.WriteString(out, usage)
		return err
	}
	o := options{command: args[0]}
	if o.command != "plan" && o.command != "stop" && o.command != "erase-luks" {
		return errors.New("emergency: unknown command; use emergency --help")
	}
	f := flag.NewFlagSet("emergency "+o.command, flag.ContinueOnError)
	f.SetOutput(errOut)
	f.StringVar(&o.dir, "dir", "./opsecmkt-deployment", "saved installation directory")
	f.StringVar(&o.confirmProject, "confirm-project", "", "exact saved Compose project to stop")
	f.StringVar(&o.device, "device", "", "exact clean /dev/ block-device path; no default")
	f.StringVar(&o.confirmDevice, "confirm-device", "", "repeat the exact target device to authorize destruction")
	f.StringVar(&o.ramDir, "ram-dir", "", "optional unlocked instance directory to remove after all selected containers stop")
	f.BoolVar(&o.reboot, "reboot", false, "immediate unsynced host reboot after successful LUKS erasure")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("emergency: unexpected arguments")
	}
	h := &systemHost{}
	defer h.Close()
	return execute(ctx, o, out, errOut, h, operator.EmergencyStop, ramsecrets.Verify)
}

func execute(ctx context.Context, o options, out, errOut io.Writer, h host, stop func(context.Context, string, operator.EmergencyTarget, io.Writer, io.Writer) error, verifyRAM func(string) error) error {
	dir, err := filepath.Abs(o.dir)
	if err != nil {
		return errors.New("emergency: invalid installation path")
	}
	target, err := operator.InstallationTarget(dir)
	if err != nil {
		return err
	}
	if o.command != "plan" && o.confirmProject != target.Project {
		return errors.New("emergency: --confirm-project must exactly match the saved installation project")
	}
	if o.command == "stop" && (o.device != "" || o.confirmDevice != "" || o.reboot) {
		return errors.New("emergency stop cannot erase a device or reboot")
	}
	if o.device != "" {
		if !filepath.IsAbs(o.device) || filepath.Clean(o.device) != o.device || !strings.HasPrefix(o.device, "/dev/") || strings.ContainsAny(o.device, "\x00\r\n") || strings.Contains(o.device, "<") || strings.Contains(o.device, ">") {
			return errors.New("emergency: name an exact /dev/ block device; placeholders are refused")
		}
		if err := h.checkDevice(ctx, o.device); err != nil {
			return err
		}
	}
	if o.command == "erase-luks" && (o.device == "" || o.confirmDevice != o.device) {
		return errors.New("emergency: erase-luks requires --device and matching --confirm-device; LUKS data may become permanently unrecoverable")
	}
	if o.reboot && o.device == "" {
		return errors.New("emergency: reboot requires an explicit LUKS device plan")
	}
	if o.ramDir != "" {
		ram, err := filepath.Abs(o.ramDir)
		if err != nil || ram != dir || !strings.HasPrefix(ram, "/run/opsecmkt-secrets/") || ram == "/run/opsecmkt-secrets/" {
			return errors.New("emergency: RAM cleanup must name this installation under /run/opsecmkt-secrets/INSTANCE")
		}
		if err := verifyRAM(ram); err != nil {
			return err
		}
	}
	if o.command == "plan" {
		_, err := fmt.Fprintf(out, "Selected project: %s\nDocker context: %s\nStop/remove its containers; retain persistent volumes.\nRAM cleanup, device operations and reboot target this CLI host.\n", target.Project, target.DockerContext)
		if err != nil {
			return err
		}
		if o.ramDir != "" {
			fmt.Fprintln(out, "Then unlink the selected tmpfs installation files; this does not guarantee memory zeroization.")
		}
		if o.device != "" {
			fmt.Fprintf(out, "Then destroy all LUKS keyslots on %s; existing mappings and header backups can retain access.\n", o.device)
		}
		if o.reboot {
			fmt.Fprintln(out, "Then immediately reboot this host without syncing filesystems.")
		}
		return nil
	}
	if err := stop(ctx, dir, target, out, errOut); err != nil {
		return errors.New("emergency: deployment stop failed; no secret cleanup, device erasure or reboot performed")
	}
	if o.ramDir != "" {
		// Verify again after the stop, before removing anything. Never force-unmount.
		if err := verifyRAM(dir); err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return errors.New("emergency: cannot remove selected tmpfs files; device was not erased")
		}
	}
	if o.command == "erase-luks" {
		if err := h.checkDevice(ctx, o.device); err != nil {
			return err
		}
		if err := h.erase(ctx, o.device); err != nil {
			return err
		}
		fmt.Fprintln(out, "Selected LUKS keyslots erased. Live mappings and header backups require separate assessment.")
		if o.reboot {
			return h.reboot()
		}
	}
	_, err = fmt.Fprintln(out, "Selected deployment stopped; persistent volumes retained.")
	return err
}
