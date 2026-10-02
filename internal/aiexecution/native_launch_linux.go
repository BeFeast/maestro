package aiexecution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ObserveNativeProcessLaunch binds the durable pre-exec monitor claim to the
// exact live systemd incarnation. The tmux host runner is outside this cgroup.
func ObserveNativeProcessLaunch(profile FileProof, projectID, nativeID, unit string) (*NativeProcessTermination, int, error) {
	p, err := readContainmentProfile(profile)
	if err != nil {
		return nil, 0, err
	}
	if p.ProjectID != projectID || uuid.Validate(nativeID) != nil || filepath.Base(unit) != unit || !strings.HasSuffix(unit, ".service") {
		return nil, 0, Held("containment_launch_binding_invalid")
	}
	b, err := readNativeEvidence(filepath.Join(p.ClaimDir, nativeID+".launch.json"), p.UID, 16<<10)
	var proof NativeProcessTermination
	if err != nil || DecodeStrict(b, &proof) != nil || ValidateNativeProcessLaunch(proof, profile, nativeID, unit) != nil {
		return nil, 0, Held("containment_launch_proof_unavailable")
	}
	if _, err := os.Lstat(filepath.Join(p.ClaimDir, nativeID+".termination.json")); !os.IsNotExist(err) {
		return nil, 0, Held("containment_native_process_terminal")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil || strings.TrimSpace(string(boot)) != proof.BootID {
		return nil, 0, Held("containment_process_incarnation_changed")
	}
	bin, err := readNativeOwnedFile(p.Systemctl.Path, 0, 64<<20, 0022)
	if err != nil || digest(bin) != p.Systemctl.SHA256 {
		return nil, 0, Held("containment_systemctl_drift")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Systemctl.Path, "show", "--no-pager", "--property=ActiveState", "--property=ControlGroup", "--property=InvocationID", "--property=MainPID", unit)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	var output nativeRecoveryOutput
	cmd.Stdout = &output
	if cmd.Run() != nil || output.overflow {
		return nil, 0, Held("containment_process_unobservable")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(output.buf.String(), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			if _, exists := fields[k]; exists {
				return nil, 0, Held("containment_process_unobservable")
			}
			fields[k] = v
		}
	}
	pid, err := strconv.Atoi(fields["MainPID"])
	if err != nil || pid <= 0 || fields["ActiveState"] != "active" || fields["ControlGroup"] != proof.Cgroup || fields["InvocationID"] != proof.InvocationID {
		return nil, 0, Held("containment_native_process_not_running")
	}
	root := "/proc/" + strconv.Itoa(pid)
	cgroup, err := os.ReadFile(root + "/cgroup")
	if err != nil || !strings.Contains("\n"+string(cgroup), "\n0::"+proof.Cgroup+"\n") || !namespaceMatches(root+"/ns/net", p.NamespaceDev, p.NamespaceIno) || VerifyFile(FileProof{Path: root + "/exe", SHA256: p.Maestro.SHA256}) != nil {
		return nil, 0, Held("containment_monitor_identity_invalid")
	}
	argv, err := os.ReadFile(root + "/cmdline")
	if err != nil || !strings.Contains(string(argv), "\x00_native-monitor\x00") {
		return nil, 0, Held("containment_monitor_identity_invalid")
	}
	return &proof, pid, nil
}
