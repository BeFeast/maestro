package supervisor

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/befeast/maestro/internal/aiexecution"
	"github.com/google/uuid"
)

// AuxiliaryOccupant describes one durable reason a receipt root still consumes
// auxiliary capacity. It carries no prompt, output or authority data.
type AuxiliaryOccupant struct {
	Root     string
	Identity string
	Intent   string
	Source   string
	Since    time.Time
}

// Key is the capacity-union key shared with in-memory reservations.
func (o AuxiliaryOccupant) Key() string { return filepath.Clean(o.Root) + "\x00" + o.Identity }

// PendingAuxiliaryRuns reads existing native consultation receipts only. No
// process is launched, receipt reconciled or financial outcome inferred.
func PendingAuxiliaryRuns(stateDir string) ([]string, error) {
	occupants, err := PendingAuxiliaryOccupancy(stateDir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var result []string
	for _, o := range occupants {
		key := o.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, key)
	}
	return result, nil
}

// PendingAuxiliaryOccupancy is PendingAuxiliaryRuns with the durable evidence
// behind each occupied identity (launch marker, launch_intent candidate or an
// unsettled native outcome) so an exhausted ceiling can be journaled and
// diagnosed (#1232). It is read-only.
func PendingAuxiliaryOccupancy(stateDir string) ([]AuxiliaryOccupant, error) {
	dirs := []string{stateDir, filepath.Join(stateDir, "native-reviews")}
	var result []AuxiliaryOccupant
	for _, dir := range dirs {
		ids := map[string]bool{}
		marker, err := readAuxiliaryReceipt(dir, "launch.json")
		if err == nil {
			var launch struct {
				Identity ConsultationIdentity `json:"identity"`
				IntentID string               `json:"intent_id"`
			}
			if json.Unmarshal(marker, &launch) != nil || uuid.Validate(launch.Identity.ID) != nil || uuid.Validate(launch.IntentID) != nil {
				return nil, aiexecution.Held("auxiliary_receipt_invalid")
			}
			ids[launch.Identity.ID] = true
			result = append(result, AuxiliaryOccupant{Root: filepath.Clean(dir), Identity: launch.Identity.ID, Intent: launch.IntentID, Source: "launch_marker", Since: auxiliaryReceiptModTime(dir, "launch.json")})
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		data, err := readAuxiliaryReceipt(dir, "current.json")
		if err == nil {
			var receipt ConsultationReceipt
			if json.Unmarshal(data, &receipt) != nil || receipt.SchemaVersion != 1 || uuid.Validate(receipt.Identity.ID) != nil {
				return nil, aiexecution.Held("auxiliary_receipt_invalid")
			}
			for _, candidate := range receipt.Candidates {
				if candidate.Status == "launch_intent" {
					ids[receipt.Identity.ID] = true
					result = append(result, AuxiliaryOccupant{Root: filepath.Clean(dir), Identity: receipt.Identity.ID, Intent: candidate.IntentID, Source: "launch_intent_candidate", Since: receipt.StartedAt})
				}
			}
			if hasNativeSession(&receipt) {
				if !receipt.NativeOutcomeComplete || !nativeInvocationsAllowed(&receipt) {
					ids[receipt.Identity.ID] = true
					result = append(result, AuxiliaryOccupant{Root: filepath.Clean(dir), Identity: receipt.Identity.ID, Source: "native_outcome_unsettled", Since: receipt.StartedAt})
				} else if !ids[receipt.Identity.ID] {
					if err := NativeAuxiliaryOutcomeComplete(dir, receipt.Identity.ID); err != nil {
						return nil, err
					}
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return result, nil
}

func auxiliaryReceiptModTime(dir, name string) time.Time {
	info, err := os.Lstat(filepath.Join(dir, "supervisor-consultations", name))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

func readAuxiliaryReceipt(dir, name string) ([]byte, error) {
	// Refuse symlinked receipt roots/files and group/world-writable records.
	// An inaccessible or corrupt root cannot be interpreted as empty capacity.
	for _, p := range []string{dir, filepath.Join(dir, "supervisor-consultations")} {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode()&0022 != 0 || !ok || st.Uid != uint32(os.Geteuid()) {
			return nil, aiexecution.Held("auxiliary_receipt_root_invalid")
		}
	}
	path := filepath.Join(dir, "supervisor-consultations", name)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	const maxReceipt = 2 << 20
	if !before.Mode().IsRegular() || before.Mode()&0022 != 0 || !ok || st.Uid != uint32(os.Geteuid()) || before.Size() > maxReceipt {
		return nil, aiexecution.Held("auxiliary_receipt_invalid")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, aiexecution.Held("auxiliary_receipt_changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReceipt+1))
	if err != nil || len(b) > maxReceipt {
		return nil, aiexecution.Held("auxiliary_receipt_unavailable")
	}
	return b, nil
}
