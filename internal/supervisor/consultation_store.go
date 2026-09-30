package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
)

type consultationStore struct{ dir string }

func openConsultationStore(stateDir string) (*consultationStore, func(), error) {
	s, unlock, err := lockConsultationStore(stateDir)
	if err != nil {
		return nil, nil, err
	}
	dir := s.dir
	if _, err := os.Stat(filepath.Join(dir, "registration.json")); err == nil {
		unlock()
		return nil, nil, &ConsultationHold{Code: "unresolved_registration_intent"}
	} else if !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "current.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, unlock, nil
	}
	if err != nil {
		unlock()
		return nil, nil, err
	}
	var previous ConsultationReceipt
	if err := json.Unmarshal(b, &previous); err != nil || previous.SchemaVersion != 1 || uuid.Validate(previous.Identity.ID) != nil {
		unlock()
		return nil, nil, &ConsultationHold{Code: "receipt_invalid"}
	}
	if hasNativeSession(&previous) && (!previous.NativeOutcomeComplete || !nativeInvocationsAllowed(&previous)) {
		unlock()
		return nil, nil, &ConsultationHold{Code: "unresolved_native_outcome"}
	}
	for _, c := range previous.Candidates {
		if c.Status == "launch_intent" || c.Status == "registration_intent" {
			unlock()
			code := "unresolved_launch_intent"
			if c.Status == "registration_intent" {
				code = "unresolved_registration_intent"
			}
			return nil, nil, &ConsultationHold{Code: code}
		}
	}
	// Archive the previous receipt before replacing current. A failure here
	// prevents another launch; completed observations are never discarded.
	if err := atomicReceiptWrite(dir, previous.Identity.ID+".json", b); err != nil {
		unlock()
		return nil, nil, err
	}
	return s, unlock, nil
}

// Reconciliation takes the same writer lock and may inspect pending registration,
// but a launch marker always blocks it: registration cannot settle inference.
func lockConsultationStore(stateDir string) (*consultationStore, func(), error) {
	return lockConsultationStoreMode(stateDir, false)
}

func lockConsultationStoreMode(stateDir string, allowLaunch bool) (*consultationStore, func(), error) {
	dir := filepath.Join(stateDir, "supervisor-consultations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	// Persist the newly created directory before relying on files within it.
	if err := syncDirectory(stateDir); err != nil {
		return nil, nil, err
	}
	lock := flock.New(filepath.Join(dir, ".lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, &ConsultationHold{Code: "consultation_in_progress"}
	}
	unlock := func() { _ = lock.Unlock() }
	s := &consultationStore{dir: dir}
	// Keep a separate durable launch marker until the outcome is fsynced.
	// Even a rename-success/fsync-failure while saving current must not permit
	// the next process to interpret that uncertain write as a cleared intent.
	if _, err := os.Stat(filepath.Join(dir, "launch.json")); err == nil && !allowLaunch {
		unlock()
		return nil, nil, &ConsultationHold{Code: "unresolved_launch_intent"}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, nil, err
	}
	return s, unlock, nil
}

func (s *consultationStore) beginRegistration(identity ConsultationIdentity, registration *NativeSessionRegistrationReceipt) error {
	b, err := json.Marshal(struct {
		ConsultationID string                            `json:"consultation_id"`
		Registration   *NativeSessionRegistrationReceipt `json:"registration"`
	}{identity.ID, registration})
	if err != nil {
		return err
	}
	return atomicReceiptWrite(s.dir, "registration.json", b)
}

func (s *consultationStore) finishRegistration() error {
	if err := os.Remove(filepath.Join(s.dir, "registration.json")); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *consultationStore) beginLaunch(identity ConsultationIdentity, intent string) error {
	b, err := json.Marshal(struct {
		Identity ConsultationIdentity `json:"identity"`
		IntentID string               `json:"intent_id"`
	}{identity, intent})
	if err != nil {
		return err
	}
	return atomicReceiptWrite(s.dir, "launch.json", b)
}

func (s *consultationStore) finishLaunch() error {
	if err := os.Remove(filepath.Join(s.dir, "launch.json")); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

func (s *consultationStore) save(r *ConsultationReceipt) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicReceiptWrite(s.dir, "current.json", append(b, '\n'))
}

func atomicReceiptWrite(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, ".receipt-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync receipt directory: %w", err)
	}
	return nil
}
