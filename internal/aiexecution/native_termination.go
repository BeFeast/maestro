package aiexecution

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// NativeProcessTermination proves the exact launched incarnation's OS outcome.
// LocalStatus=local_output_unknown is a terminal OS observation, never proof of
// complete stdout or a financially settled provider invocation.
type NativeProcessTermination struct {
	Version         int       `json:"version"`
	Profile         FileProof `json:"profile"`
	NativeSessionID string    `json:"native_session_id"`
	Unit            string    `json:"unit"`
	Cgroup          string    `json:"cgroup"`
	BootID          string    `json:"boot_id"`
	InvocationID    string    `json:"invocation_id"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	LocalStatus     string    `json:"local_status"`
	ExitCode        int       `json:"exit_code"`
	Digest          string    `json:"digest"`
}

type nativeTerminationProfile struct {
	UID       uint32    `json:"uid"`
	ClaimDir  string    `json:"claim_dir"`
	Systemctl FileProof `json:"systemctl"`
}

func readNativeEvidence(path string, uid uint32, max int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, Held("containment_evidence_unsafe")
	}
	parent := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/") {
		parent = filepath.Join(parent, part)
		st, err := os.Lstat(parent)
		if err != nil {
			return nil, err
		}
		s, ok := st.Sys().(*syscall.Stat_t)
		if !ok || !st.IsDir() || st.Mode().Perm()&0022 != 0 || s.Uid != 0 && s.Uid != uid {
			return nil, Held("containment_evidence_unsafe")
		}
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || s.Uid != uid || st.Size() > max {
		return nil, Held("containment_evidence_unsafe")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, Held("containment_evidence_unsafe")
	}
	return b, nil
}

func nativeTerminationDigest(proof NativeProcessTermination) string {
	proof.Digest = ""
	b, _ := json.Marshal(proof)
	return digest(b)
}

// VerifyNativeProcessTermination re-observes the original pinned execution
// profile and exact cgroup. A missing/collected unit is insufficient without a
// durable pre-exec launch proof. It does not use current routing configuration.
func VerifyNativeProcessTermination(profile FileProof, nativeID, unit string) (*NativeProcessTermination, error) {
	if id, err := uuid.Parse(nativeID); err != nil || id.String() != nativeID || unit == "" || filepath.Base(unit) != unit {
		return nil, Held("containment_termination_binding_invalid")
	}
	if VerifyFile(profile) != nil {
		return nil, Held("containment_termination_profile_drift")
	}
	b, err := os.ReadFile(profile.Path)
	var p nativeTerminationProfile
	// The full schema is checked by the launch consumer. This narrow read
	// deliberately depends only on the pinned original recovery fields.
	if err != nil || len(b) > 128<<10 || json.Unmarshal(b, &p) != nil || !filepath.IsAbs(p.ClaimDir) || VerifyFile(p.Systemctl) != nil {
		return nil, Held("containment_termination_profile_invalid")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, Held("containment_boot_unobservable")
	}
	read := func(suffix string) (NativeProcessTermination, error) {
		var proof NativeProcessTermination
		path := filepath.Join(p.ClaimDir, nativeID+suffix)
		data, err := readNativeEvidence(path, p.UID, 16<<10)
		if err != nil {
			return proof, err
		}
		if err != nil || DecodeStrict(data, &proof) != nil || proof.Version != 1 || proof.Profile != profile || proof.NativeSessionID != nativeID || proof.Unit != unit || proof.StartedAt.IsZero() || proof.Cgroup == "/" || !strings.HasPrefix(proof.Cgroup, "/") || filepath.Clean(proof.Cgroup) != proof.Cgroup || filepath.Base(proof.Cgroup) != unit || proof.Digest != nativeTerminationDigest(proof) {
			return proof, Held("containment_termination_proof_invalid")
		}
		if uuid.Validate(proof.BootID) != nil || len(proof.InvocationID) != 32 {
			return proof, Held("containment_termination_proof_invalid")
		}
		return proof, nil
	}
	launch, err := read(".launch.json")
	if err != nil {
		return nil, Held("containment_launch_proof_unavailable")
	}
	proof, err := read(".termination.json")
	recovered := false
	if os.IsNotExist(err) {
		recovered = true
		proof = launch
		proof.LocalStatus = "local_output_unknown"
		proof.ExitCode = -1
		proof.EndedAt = time.Now().UTC()
	} else if err != nil {
		return nil, err
	}
	if proof.Cgroup != launch.Cgroup || proof.BootID != launch.BootID || proof.InvocationID != launch.InvocationID || !proof.StartedAt.Equal(launch.StartedAt) || proof.EndedAt.Before(proof.StartedAt) {
		return nil, Held("containment_termination_proof_invalid")
	}
	events, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", proof.Cgroup, "cgroup.events"))
	if proof.BootID == strings.TrimSpace(string(boot)) && (err != nil && !os.IsNotExist(err) || err == nil && !strings.Contains("\n"+string(events), "\npopulated 0\n")) {
		return nil, Held("containment_previous_cgroup_unresolved")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Systemctl.Path, "show", "--no-pager", "--property=LoadState", "--property=ActiveState", "--property=ControlGroup", "--property=InvocationID", unit)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	out, err := cmd.Output()
	if err != nil || len(out) > 16<<10 {
		return nil, Held("containment_process_unobservable")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			fields[k] = v
		}
	}
	if fields["LoadState"] != "not-found" && (fields["LoadState"] != "loaded" || fields["ActiveState"] != "inactive" && fields["ActiveState"] != "failed" || fields["ControlGroup"] != "" && fields["ControlGroup"] != proof.Cgroup) {
		return nil, Held("containment_previous_cgroup_unresolved")
	}
	if fields["LoadState"] == "loaded" && fields["InvocationID"] != "" && fields["InvocationID"] != proof.InvocationID {
		return nil, Held("containment_process_incarnation_changed")
	}
	proof.Digest = nativeTerminationDigest(proof)
	if recovered {
		b, err := json.Marshal(proof)
		if err != nil || replaceRevisionFile(filepath.Join(p.ClaimDir, nativeID+".termination.json"), b) != nil {
			return nil, Held("containment_termination_persistence_failed")
		}
	}
	return &proof, nil
}
