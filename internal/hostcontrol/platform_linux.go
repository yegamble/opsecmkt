//go:build linux

package hostcontrol

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"opsecmkt/internal/storageguard"

	"golang.org/x/sys/unix"
)

func unixPeerUID(conn net.Conn) (uint32, error) {
	local, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("local peer credentials unavailable")
	}
	raw, err := local.SyscallConn()
	if err != nil {
		return 0, errors.New("local peer credentials unavailable")
	}
	var credentials *unix.Ucred
	var socketErr error
	if raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}) != nil || socketErr != nil || credentials == nil {
		return 0, errors.New("local peer credentials unavailable")
	}
	return credentials.Uid, nil
}

func verifyRootPeer(conn net.Conn) error {
	uid, err := unixPeerUID(conn)
	if err != nil || uid != 0 {
		return errors.New("host control root peer unavailable")
	}
	return nil
}

func runPlatformAgent(ctx context.Context, cfg agentConfig, out, _ io.Writer, verifyStorage func(context.Context, string) error) error {
	if os.Geteuid() != 0 {
		return errors.New("host control agent requires Linux root")
	}
	initName, err := readBounded("/proc/1/comm", 64)
	if err != nil || strings.TrimSpace(string(initName)) != "systemd" || rootAncestors("/run/systemd/system/check") != nil || rebootExecutableReady() != nil {
		return errors.New("host control agent requires a supported systemd Linux host")
	}
	security := func(ctx context.Context) error {
		if verifyStorage(ctx, cfg.deploymentDir) != nil || storageguard.VerifyPersistent(cfg.stateDir) != nil || storageguard.VerifyPersistent(cfg.policyFile) != nil {
			return errors.New("host control encrypted storage verification failed")
		}
		return nil
	}
	if security(ctx) != nil {
		return errors.New("host control agent requires verified encrypted storage")
	}
	bootBytes, err := readBounded("/proc/sys/kernel/random/boot_id", 64)
	bootID := strings.TrimSpace(string(bootBytes))
	if err != nil || !validBootID(bootID) {
		return errors.New("host control agent cannot verify current boot identity")
	}
	policy, err := readUnlockPolicy(cfg, bootID)
	if err != nil && !errors.Is(err, errUnlockBootChanged) {
		return err
	}
	if err := privateStateDir(cfg.stateDir); err != nil {
		return err
	}
	lock, err := lockState(cfg.stateDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	store := &ledgerStore{dir: cfg.stateDir, checkFile: rootPrivateFile}
	initial, err := store.load(cfg.deploymentDir, !policy.UnlockRehearsed)
	if err != nil {
		return err
	}
	deps := agentDependencies{
		context: ctx,
		now:     time.Now,
		ready: func(ctx context.Context) string {
			if rebootExecutableReady() != nil {
				return "host-unavailable"
			}
			if security(ctx) != nil {
				return "storage-unverified"
			}
			policy, err := readUnlockPolicy(cfg, bootID)
			if errors.Is(err, errUnlockBootChanged) {
				return "unlock-not-rehearsed"
			}
			if err != nil {
				return "policy-unavailable"
			}
			if !policy.UnlockRehearsed {
				return "unlock-not-rehearsed"
			}
			return "ready"
		},
		execute: systemReboot,
		schedule: func(delay time.Duration, action func()) func() {
			timer := time.AfterFunc(delay, action)
			return func() { timer.Stop() }
		},
	}
	control, err := newController(store, initial, bootID, uint32(cfg.appUID), deps)
	if err != nil {
		return err
	}
	defer control.close()
	listener, err := listenRootSocket(cfg)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: control, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 4096,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			uid, err := unixPeerUID(conn)
			if err != nil || uid != uint32(cfg.appUID) {
				conn.Close()
				return ctx
			}
			return context.WithValue(ctx, peerUIDKey{}, uid)
		},
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	// Type=notify must not release dependants until the protected directory and
	// socket exist. Manual runs do not need a systemd notification socket.
	if err := notifySystemdReady(ctx, os.Getenv("NOTIFY_SOCKET")); err != nil {
		return err
	}
	_, _ = io.WriteString(out, "host control agent ready for configured local application peer; readiness policy still applies\n")
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("host control agent stopped unexpectedly")
	}
	return nil
}

func notifySystemdReady(ctx context.Context, address string) error {
	if address == "" {
		return nil
	}
	unavailable := errors.New("host control service readiness notification failed")
	// Linux sockaddr_un reserves one byte for a terminator on filesystem
	// sockets. Use the same conservative bound for abstract socket names.
	if len(address) > 107 || strings.IndexFunc(address, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return unavailable
	}
	if address[0] == '@' {
		if len(address) == 1 {
			return unavailable
		}
	} else if !safeAbsolute(address) || address == "/" {
		return unavailable
	}
	notifyCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(notifyCtx, "unixgram", address)
	if err != nil {
		return unavailable
	}
	defer conn.Close()
	deadline, _ := notifyCtx.Deadline()
	if conn.SetWriteDeadline(deadline) != nil {
		return unavailable
	}
	stop := context.AfterFunc(notifyCtx, func() { _ = conn.Close() })
	defer stop()
	const message = "READY=1\n"
	if n, err := io.WriteString(conn, message); err != nil || n != len(message) {
		return unavailable
	}
	return nil
}

func rootAncestors(path string) error {
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		var st unix.Stat_t
		if unix.Lstat(current, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0022 != 0 {
			return errors.New("host control requires protected root-owned paths")
		}
		if current == "/" {
			return nil
		}
	}
}

func rootPrivateFile(path string) error {
	var st unix.Stat_t
	if rootAncestors(path) != nil || unix.Lstat(path, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Mode&0777 != 0600 {
		return errors.New("host control requires private root-owned configuration and state")
	}
	return nil
}

func readUnlockPolicy(cfg agentConfig, bootID string) (UnlockPolicy, error) {
	var policy UnlockPolicy
	if rootPrivateFile(cfg.policyFile) != nil {
		return policy, errors.New("host control unlock policy unavailable")
	}
	data, err := readBounded(cfg.policyFile, 4096)
	if err != nil {
		return policy, errors.New("host control unlock policy unavailable")
	}
	return decodeUnlockPolicy(data, cfg.deploymentDir, bootID)
}

func privateStateDir(path string) error {
	if rootAncestors(path) != nil {
		return errors.New("host control state directory unavailable")
	}
	if err := os.Mkdir(path, 0700); err == nil {
		if os.Chmod(path, 0700) != nil {
			return errors.New("host control state directory unavailable")
		}
	} else if !os.IsExist(err) {
		return errors.New("host control state directory unavailable")
	}
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Mode&0777 != 0700 {
		return errors.New("host control state directory must be private and root-owned")
	}
	return nil
}

func lockState(dir string) (*os.File, error) {
	path := filepath.Join(dir, "agent.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("host control receipt lock unavailable")
	}
	file := os.NewFile(uintptr(fd), "host-control-lock")
	if rootPrivateFile(path) != nil || unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		file.Close()
		return nil, errors.New("host control receipt lock unavailable or another agent is running")
	}
	return file, nil
}

func listenRootSocket(cfg agentConfig) (*net.UnixListener, error) {
	dir := filepath.Dir(cfg.socket)
	if rootAncestors(dir) != nil {
		return nil, errors.New("host control socket directory unavailable")
	}
	if err := os.Mkdir(dir, 0750); err == nil {
		if os.Chmod(dir, 0750) != nil || os.Chown(dir, 0, cfg.appGID) != nil {
			return nil, errors.New("host control socket directory unavailable")
		}
	} else if !os.IsExist(err) {
		return nil, errors.New("host control socket directory unavailable")
	}
	var st unix.Stat_t
	if unix.Lstat(dir, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != 0 || st.Gid != uint32(cfg.appGID) || st.Mode&0777 != 0750 {
		return nil, errors.New("host control socket directory must match the protected application group")
	}
	if before, err := os.Lstat(cfg.socket); err == nil {
		var socket unix.Stat_t
		if unix.Lstat(cfg.socket, &socket) != nil || socket.Mode&unix.S_IFMT != unix.S_IFSOCK || socket.Uid != 0 || socket.Gid != uint32(cfg.appGID) || socket.Mode&0777 != 0660 {
			return nil, errors.New("host control socket path is already in use")
		}
		conn, err := net.DialTimeout("unix", cfg.socket, time.Second)
		if err == nil {
			conn.Close()
			return nil, errors.New("host control socket already has a listener")
		}
		if !errors.Is(err, unix.ECONNREFUSED) {
			return nil, errors.New("host control socket could not be proved stale")
		}
		after, err := os.Lstat(cfg.socket)
		if err != nil || !os.SameFile(before, after) || os.Remove(cfg.socket) != nil {
			return nil, errors.New("host control stale socket could not be safely removed")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("host control socket path unavailable")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.socket, Net: "unix"})
	if err != nil {
		return nil, errors.New("host control socket could not be opened")
	}
	if os.Chmod(cfg.socket, 0660) != nil || os.Chown(cfg.socket, 0, cfg.appGID) != nil {
		listener.Close()
		return nil, errors.New("host control socket permissions could not be secured")
	}
	return listener, nil
}

func systemReboot(ctx context.Context) error {
	const binary = "/usr/bin/systemctl"
	if rebootExecutableReady() != nil {
		return errors.New("host reboot executable unavailable")
	}
	cmd := exec.CommandContext(ctx, binary, "reboot")
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		return errors.New("host reboot command did not complete")
	}
	return nil
}

func rebootExecutableReady() error {
	const binary = "/usr/bin/systemctl"
	var st unix.Stat_t
	if rootAncestors(binary) != nil || unix.Lstat(binary, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != 0 || st.Mode&0022 != 0 || st.Mode&0111 == 0 {
		return errors.New("host reboot executable unavailable")
	}
	return nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("host control configuration unavailable")
	}
	return data, nil
}
