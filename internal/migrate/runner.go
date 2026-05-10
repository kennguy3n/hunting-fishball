// Package migrate owns the SQL migration runner shared by cmd/api and
// cmd/ingest (Phase 8 / Task 18).
//
// The runner reads `*.sql` files from a directory (defaults to
// `migrations/` in the repo root), parses the leading numeric prefix
// to derive an application order, and applies every file that hasn't
// yet been recorded in the `schema_migrations` tracking table. Each
// migration runs inside its own transaction so a failure half-way
// through a multi-statement migration leaves the database consistent.
//
// Dry-run mode skips the apply step and just returns the list of
// pending migrations so an operator can see what would run before
// flipping the AUTO_MIGRATE flag.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SchemaMigration is the GORM row backing the `schema_migrations`
// tracking table. Version is the numeric prefix from the filename
// (e.g. "001_audit_log.sql" → 1).
type SchemaMigration struct {
	Version   int       `gorm:"column:version;primaryKey"`
	Name      string    `gorm:"column:name;not null"`
	AppliedAt time.Time `gorm:"column:applied_at;not null"`
	Checksum  string    `gorm:"column:checksum;not null"`
}

// TableName overrides GORM's default plural form.
func (SchemaMigration) TableName() string { return "schema_migrations" }

// Migration is the in-memory representation of a `.sql` file.
type Migration struct {
	Version int
	Name    string
	Path    string
	SQL     string
}

// Runner is the top-level migration helper.
type Runner struct {
	db  *gorm.DB
	dir string
}

// Config configures Runner. Dir defaults to "migrations".
type Config struct {
	DB  *gorm.DB
	Dir string
}

// New returns a Runner. Dir defaults to "migrations".
func New(cfg Config) (*Runner, error) {
	if cfg.DB == nil {
		return nil, errors.New("migrate: nil DB")
	}
	dir := cfg.Dir
	if dir == "" {
		dir = "migrations"
	}
	return &Runner{db: cfg.DB, dir: dir}, nil
}

// Pending returns the list of migrations that have not yet been
// applied. The list is ordered by Version ascending.
func (r *Runner) Pending(ctx context.Context) ([]Migration, error) {
	all, err := r.discover()
	if err != nil {
		return nil, err
	}
	if err := r.db.WithContext(ctx).AutoMigrate(&SchemaMigration{}); err != nil {
		return nil, fmt.Errorf("migrate: bootstrap tracking table: %w", err)
	}
	var applied []SchemaMigration
	if err := r.db.WithContext(ctx).Find(&applied).Error; err != nil {
		return nil, fmt.Errorf("migrate: load applied: %w", err)
	}
	seen := make(map[int]bool, len(applied))
	for _, m := range applied {
		seen[m.Version] = true
	}
	var pending []Migration
	for _, m := range all {
		if !seen[m.Version] {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// Apply runs every pending migration inside its own transaction.
func (r *Runner) Apply(ctx context.Context) ([]Migration, error) {
	pending, err := r.Pending(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range pending {
		if err := r.applyOne(ctx, m); err != nil {
			return nil, fmt.Errorf("migrate %03d_%s: %w", m.Version, m.Name, err)
		}
	}
	return pending, nil
}

// DryRun returns the pending migrations without applying them.
func (r *Runner) DryRun(ctx context.Context) ([]Migration, error) {
	return r.Pending(ctx)
}

func (r *Runner) applyOne(ctx context.Context, m Migration) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(m.SQL).Error; err != nil {
			return err
		}
		return tx.Create(&SchemaMigration{
			Version:   m.Version,
			Name:      m.Name,
			AppliedAt: time.Now().UTC(),
			Checksum:  checksum(m.SQL),
		}).Error
	})
}

// discover reads every `*.sql` file in r.dir and returns them in
// numeric-prefix order. Files whose name doesn't match
// `NNN_name.sql` are rejected — the convention is enforced.
func (r *Runner) discover() ([]Migration, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir %q: %w", r.dir, err)
	}
	var out []Migration
	seen := make(map[int]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if err := validateName(e); err != nil {
			return nil, err
		}
		v, name, err := parseName(e.Name())
		if err != nil {
			return nil, err
		}
		if existing, ok := seen[v]; ok {
			return nil, fmt.Errorf("migrate: duplicate version %d (%s vs %s)", v, existing, e.Name())
		}
		seen[v] = e.Name()
		path := filepath.Join(r.dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", path, err)
		}
		out = append(out, Migration{Version: v, Name: name, Path: path, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func validateName(e fs.DirEntry) error {
	name := e.Name()
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return fmt.Errorf("migrate: %q must be NNN_name.sql", name)
	}
	if _, err := strconv.Atoi(name[:idx]); err != nil {
		return fmt.Errorf("migrate: %q prefix must be numeric", name)
	}
	return nil
}

func parseName(filename string) (int, string, error) {
	idx := strings.IndexByte(filename, '_')
	if idx <= 0 {
		return 0, "", fmt.Errorf("migrate: %q must be NNN_name.sql", filename)
	}
	version, err := strconv.Atoi(filename[:idx])
	if err != nil {
		return 0, "", fmt.Errorf("migrate: %q prefix not numeric: %w", filename, err)
	}
	name := strings.TrimSuffix(filename[idx+1:], ".sql")
	return version, name, nil
}

// checksum is a fast non-cryptographic fingerprint for noticing
// edits to an already-applied migration. Callers compare against
// `applied.Checksum` outside this package; we do not enforce a
// match here because deployment patterns differ.
func checksum(sql string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(sql); i++ {
		h ^= uint64(sql[i])
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 16)
}
