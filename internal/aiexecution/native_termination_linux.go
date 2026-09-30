package aiexecution

import (
	"bytes"
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

type nativeTerminationProfile struct {
	UID       uint32    `json:"uid"`
	ClaimDir  string    `json:"claim_dir"`
	Systemctl FileProof `json:"systemctl"`
}

func readNativeEvidence(path string, uid uint32, max int64) ([]byte, error) {
	return readNativeOwnedFile(path, uid, max, 0077)
}

func readNativeOwnedFile(path string, uid uint32, max int64, mask os.FileMode) ([]byte, error) {
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
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
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
	if !ok || !st.Mode().IsRegular() || st.Mode().Perm()&mask != 0 || s.Uid != uid || st.Size() > max {
		return nil, Held("containment_evidence_unsafe")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, Held("containment_evidence_unsafe")
	}
	return b, nil
}

// VerifyNativeProcessTermination re-observes the original pinned execution
// profile and exact cgroup. A missing/collected unit is insufficient without a
// durable pre-exec launch proof. It does not use current routing configuration.
func VerifyNativeProcessTermination(profile FileProof, nativeID, unit string) (*NativeProcessTermination, error) {
	if id, err := uuid.Parse(nativeID); err != nil || id.String() != nativeID || unit == "" || filepath.Base(unit) != unit {
		return nil, Held("containment_termination_binding_invalid")
	}
	b, err := readNativeOwnedFile(profile.Path, 0, 128<<10, 0022)
	var p nativeTerminationProfile
	// The full schema is checked by the launch consumer. This narrow read
	// deliberately depends only on the pinned original recovery fields.
	if err != nil || digest(b) != profile.SHA256 || json.Unmarshal(b, &p) != nil || p.UID == 0 || !filepath.IsAbs(p.ClaimDir) {
		return nil, Held("containment_termination_profile_invalid")
	}
	bin, err := readNativeOwnedFile(p.Systemctl.Path, 0, 64<<20, 0022)
	if err != nil || digest(bin) != p.Systemctl.SHA256 {
		return nil, Held("containment_systemctl_drift")
	}
	lockPath := filepath.Join(p.ClaimDir, nativeID+".recovery.lock")
	lockFD, err := unix.Openat2(unix.AT_FDCWD, lockPath, &unix.OpenHow{Flags: unix.O_RDWR | unix.O_CREAT | unix.O_CLOEXEC, Mode: 0600, Resolve: unix.RESOLVE_NO_SYMLINKS})
	if err != nil {
		return nil, Held("containment_recovery_lock_unavailable")
	}
	defer unix.Close(lockFD)
	if _, err := readNativeEvidence(lockPath, p.UID, 16); err != nil || unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return nil, Held("containment_recovery_in_progress")
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
	if launch.LocalStatus != "launch_intent" || !launch.EndedAt.IsZero() || launch.ExitCode != -1 {
		return nil, Held("containment_launch_proof_invalid")
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
	switch proof.LocalStatus {
	case "succeeded", "failed", "local_output_unknown":
	default:
		return nil, Held("containment_termination_proof_invalid")
	}
	if proof.LocalStatus == "succeeded" && proof.ExitCode != 0 {
		return nil, Held("containment_termination_proof_invalid")
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
	var output nativeRecoveryOutput
	cmd.Stdout = &output
	err = cmd.Run()
	if err != nil || output.overflow {
		return nil, Held("containment_process_unobservable")
	}
	out := output.buf.Bytes()
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

type nativeRecoveryOutput struct {
	buf      bytes.Buffer
	overflow bool
}

func (o *nativeRecoveryOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := (16 << 10) - o.buf.Len()
	if len(p) > left {
		p = p[:left]
		o.overflow = true
	}
	o.buf.Write(p)
	return n, nil
}
