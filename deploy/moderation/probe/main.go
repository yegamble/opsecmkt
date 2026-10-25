// The isolation probe uses only synthetic fixtures. It is a test executable,
// never the inference server and never an application dependency.
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

func fail(message string) {
	fmt.Fprintln(os.Stderr, "moderation isolation: "+message)
	os.Exit(1)
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "cleanup-test-socket" {
		if os.Getuid() != 0 || os.Chown("/run/moderation", 0, 0) != nil {
			fail("could not reclaim disposable socket directory")
		}
		if err := os.Remove("/run/moderation/inference.sock"); err != nil && !os.IsNotExist(err) {
			fail("could not remove disposable socket")
		}
		if os.Chmod("/run/moderation", 0777) != nil {
			fail("could not release disposable socket directory")
		}
		return
	}
	// Prepare only the disposable test socket mount. No application artifacts
	// or host directories other than that explicit mount are present.
	if len(os.Args) == 2 && os.Args[1] == "prepare-test-socket" {
		if os.Getuid() != 0 || os.Chown("/run/moderation", 0, 0) != nil ||
			os.Chmod("/run/moderation", 0770) != nil || os.Chown("/run/moderation", 65532, 65532) != nil {
			fail("could not prepare disposable socket directory")
		}
		return
	}
	if os.Getuid() != 65532 || os.Getgid() != 65532 {
		fail("unexpected process identity")
	}
	for _, name := range []string{"DATABASE_URL", "APP_SECRET_KEY", "SETUP_TOKEN", "MONERO_WALLET_SECRET", "MODERATION_TEST_SECRET"} {
		if os.Getenv(name) != "" {
			fail("secret-bearing environment was inherited")
		}
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if os.Getenv(name) != "" {
			fail("proxy environment was inherited")
		}
	}
	if len(os.Args) != 2 {
		fail("missing synthetic host-secret path")
	}
	for _, path := range []string{os.Args[1], "/run/secrets", "/workspace/.env", "/var/run/docker.sock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			fail("host secret or control path is accessible")
		}
	}
	if _, err := os.ReadFile("/models/model.gguf"); err != nil {
		fail("public weights fixture is unavailable")
	}
	if err := os.WriteFile("/models/model.gguf", []byte("mutation"), 0600); err == nil {
		fail("weights mount is writable")
	}
	if err := os.WriteFile("/root-write-probe", []byte("mutation"), 0600); err == nil {
		fail("root filesystem is writable")
	}
	if err := os.WriteFile("/tmp/write-probe", []byte("temporary"), 0600); err != nil {
		fail("bounded scratch space is unavailable")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil || !strings.Contains(string(status), "NoNewPrivs:\t1") || !strings.Contains(string(status), "CapEff:\t0000000000000000") {
		fail("privilege restrictions are missing")
	}
	var core syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_CORE, &core) != nil || core.Cur != 0 || core.Max != 0 {
		fail("core dumps are not disabled")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		fail("cannot inspect network interfaces")
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 {
			fail("non-loopback network interface is present")
		}
	}
	// TEST-NET-3 is documentation address space. A network-none namespace must
	// reject this before a packet can leave the container.
	connection, err := net.DialTimeout("tcp", "203.0.113.1:443", time.Second)
	if err == nil {
		connection.Close()
		fail("external TCP connection succeeded")
	}
	listener, err := net.Listen("unix", "/run/moderation/inference.sock")
	if err != nil {
		fail("private Unix socket could not be created")
	}
	listener.Close()
	fmt.Println("moderation isolation: secret paths denied, read-only artifacts, no external interface, egress denied, Unix socket available")
}
