package aiexecution

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This fixture creates only anonymous, process-owned user/network namespaces.
// No named netns, production interface, host nft table, or service is touched.
func TestNativeNFTPrivateNamespaces(t *testing.T) {
	mode := os.Getenv("MAESTRO_NFT_FIXTURE")
	if mode == "" {
		if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
			t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for private namespace firewall fixture")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", os.Args[0], "-test.run=^TestNativeNFTPrivateNamespaces$", "-test.v")
		cmd.Env = append(os.Environ(), "MAESTRO_NFT_FIXTURE=gateway")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("private namespace: %s %v", out, err)
		}
		t.Log(string(out))
		return
	}
	command := func(name string, args ...string) {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %s %v", name, args, out, err)
		}
	}
	serve := func(address string) net.Listener {
		t.Helper()
		l, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		t.Cleanup(func() { l.Close() })
		return l
	}
	if mode == "gateway" {
		command("ip", "link", "set", "lo", "up")
		command("ip", "link", "add", "nativegw", "type", "veth", "peer", "name", "nativeclient")
		command("ip", "addr", "add", "192.0.2.1/24", "dev", "nativegw")
		command("ip", "addr", "add", "192.0.2.3/24", "dev", "nativegw")
		command("ip", "link", "set", "nativegw", "up")
		for _, address := range []string{"192.0.2.1:8317", "192.0.2.1:443", "192.0.2.1:9999", "192.0.2.3:443"} {
			serve(address)
		}
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer read.Close()
		defer write.Close()
		cmd := exec.Command("unshare", "--net", os.Args[0], "-test.run=^TestNativeNFTPrivateNamespaces$", "-test.v")
		cmd.Env = append(os.Environ(), "MAESTRO_NFT_FIXTURE=client")
		cmd.ExtraFiles = []*os.File{read}
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		// Wait for unshare to finish before moving the endpoint into its namespace.
		parentNS, _ := os.Readlink("/proc/self/ns/net")
		deadline := time.Now().Add(2 * time.Second)
		for {
			ns, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", cmd.Process.Pid))
			if ns != "" && ns != parentNS {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("child namespace unavailable")
			}
			time.Sleep(time.Millisecond)
		}
		pid := fmt.Sprint(cmd.Process.Pid)
		command("ip", "link", "set", "nativeclient", "netns", pid)
		command("nsenter", "-t", pid, "-n", "--", "ip", "addr", "add", "192.0.2.2/24", "dev", "nativeclient")
		command("nsenter", "-t", pid, "-n", "--", "ip", "link", "set", "nativeclient", "up")
		command("nsenter", "-t", pid, "-n", "--", "ip", "link", "set", "lo", "up")
		write.Write([]byte("ready"))
		write.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child firewall: %s %v", out.String(), err)
		}
		t.Log(out.String())
		return
	}
	gate := os.NewFile(3, "ready")
	io.ReadAll(gate)
	gate.Close()
	rules, err := NativeNftRules("http://192.0.2.1:8317", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	install := exec.Command("nft", "-f", "-")
	install.Stdin = strings.NewReader(rules)
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("nft: %s %v", out, err)
	}
	data, err := exec.Command("nft", "--json", "--stateless", "list", "ruleset").Output()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NativeRulesDigest(data); err != nil {
		t.Fatal(err)
	}
	loop := serve("127.0.0.1:0")
	for _, address := range []string{"192.0.2.1:8317", "192.0.2.1:443", loop.Addr().String()} {
		c, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("allowed %s: %v", address, err)
		}
		c.Close()
	}
	for _, address := range []string{"192.0.2.1:9999", "192.0.2.3:443"} {
		c, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
		if err == nil {
			c.Close()
			t.Fatalf("forbidden destination %s reachable", address)
		}
	}
	t.Log("exact gateway/Forgejo and private loopback permitted; live alternate port/provider listener denied")
}

func TestNativeNamespaceClaimRejectsConcurrentAndSurvivingCgroup(t *testing.T) {
	if os.Getenv("MAESTRO_NATIVE_KERNEL_TESTS") != "1" {
		t.Skip("set MAESTRO_NATIVE_KERNEL_TESTS=1 for namespace/cgroup fixture")
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	var cg string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "0::/") {
			cg = strings.TrimPrefix(line, "0::")
		}
	}
	if cg == "" || cg == "/" {
		t.Skip("test process lacks named cgroup")
	}
	p := NativeContainmentProfile{UID: uint32(os.Getuid()), ClaimDir: t.TempDir(), NamespaceDev: 42, NamespaceIno: 43}
	e := nativeLaunchEnvelope{Unit: filepath.Base(cg), NativeSessionID: "private-fixture"}
	lock, err := claimNativeNamespace(p, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimNativeNamespace(p, e); err == nil {
		t.Fatal("concurrent namespace reuse")
	}
	lock.Close()
	if _, err := claimNativeNamespace(p, e); err == nil {
		t.Fatal("released monitor lock hid populated old cgroup")
	}
}
