package configstore

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAuxiliaryReceiptIndexSurvivesStoreReopenWithoutProject(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "store.db")
	dir := filepath.Join(t.TempDir(), "removed-project")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RememberAuxiliaryStateDir(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dirs, err := s.AuxiliaryStateDirs(ctx)
	if err != nil || len(dirs) != 1 || dirs[0] != dir {
		t.Fatalf("dirs=%v error=%v", dirs, err)
	}
	if err = s.RememberAuxiliaryStateDir(ctx, "relative"); err == nil {
		t.Fatal("relative receipt root accepted")
	}
}
