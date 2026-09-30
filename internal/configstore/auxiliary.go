package configstore

import (
	"context"
	"fmt"
	"path/filepath"
)

// RememberAuxiliaryStateDir persists the receipt location before a controller
// may reserve its first auxiliary run. Removing a project must not forget an
// unresolved launch after the daemon restarts.
func (s *Store) RememberAuxiliaryStateDir(ctx context.Context, dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("invalid auxiliary receipt directory")
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO auxiliary_receipt_roots(state_dir) VALUES (?)`, dir)
	return err
}

func (s *Store) AuxiliaryStateDirs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state_dir FROM auxiliary_receipt_roots ORDER BY state_dir`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var dirs []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
			return nil, fmt.Errorf("invalid persisted auxiliary receipt directory")
		}
		dirs = append(dirs, dir)
	}
	return dirs, rows.Err()
}
