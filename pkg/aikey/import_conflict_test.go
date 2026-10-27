package aikey

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	cmn "github.com/loxilb-io/loxilb/common"
)

type importConflictDB struct {
	err   error
	calls int
}

func (d *importConflictDB) ExecContext(_ context.Context, _ string, _ ...any) (sql.Result, error) {
	d.calls++
	return nil, d.err
}
func (*importConflictDB) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("unexpected Query")
}
func (*importConflictDB) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRow")
}
func (*importConflictDB) PingContext(context.Context) error { return nil }

// Drive CreateAPIKey's actual insert failure, including wrapped PostgreSQL errors.
func TestImportedCredentialUniqueViolationIsSecretFreeConflict(t *testing.T) {
	const secret = "fixture-imported-key-only"
	driver := &pgconn.PgError{Code: "23505", ConstraintName: "uq_api_keys_key_hash", Message: "duplicate key", Detail: "private-hash-detail"}
	for _, err := range []error{driver, fmt.Errorf("wrapped: %w", driver)} {
		db := &importConflictDB{err: err}
		svc := &Service{db: db}
		raw, id, got := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{TenantID: "fixture-tenant", ApiKey: secret})
		var conflict *cmn.ConflictError
		if !errors.As(got, &conflict) {
			t.Fatalf("got %T, want typed conflict", got)
		}
		if raw != "" || id != "" || db.calls != 1 {
			t.Fatal("failure returned key material or did not execute insert")
		}
		if got.Error() != "imported API key is already registered" || strings.Contains(got.Error(), secret) || strings.Contains(got.Error(), driver.Detail) {
			t.Fatal("conflict is not sanitized")
		}
		var exposed *pgconn.PgError
		if errors.As(got, &exposed) {
			t.Fatal("public conflict unwraps private driver detail")
		}
	}
}
func TestImportConflictDoesNotReclassifyOtherFailures(t *testing.T) {
	cases := []struct {
		name, key, code, constraint string
		plain                       bool
	}{
		{"generated collision", "", "23505", "uq_api_keys_key_hash", false},
		{"identifier collision", "fixture-imported-key-only", "23505", "api_keys_pkey", false},
		{"different SQLSTATE", "fixture-imported-key-only", "23503", "uq_api_keys_key_hash", false},
		{"message impersonation", "fixture-imported-key-only", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var original error = &pgconn.PgError{Code: tc.code, ConstraintName: tc.constraint, Message: "duplicate key"}
			if tc.plain {
				original = errors.New("23505 uq_api_keys_key_hash duplicate key")
			}
			db := &importConflictDB{err: original}
			svc := &Service{db: db}
			raw, id, got := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{TenantID: "fixture-tenant", ApiKey: tc.key})
			// The write-outcome classifier may wrap the cause (a plain error
			// is "outcome unknown" to it), but it must stay in the chain,
			// and nothing here may turn it into the public conflict.
			var conflict *cmn.ConflictError
			if !errors.Is(got, original) || errors.As(got, &conflict) || raw != "" || id != "" || db.calls != 1 {
				t.Fatal("unrelated failure reclassified")
			}
			if tc.plain && !errors.Is(got, cmn.ErrStoreOutcomeUnknown) {
				t.Fatalf("a driver error of unknown shape must be reported as outcome unknown, got %v", got)
			}
			if !tc.plain && errors.Is(got, cmn.ErrStoreOutcomeUnknown) {
				t.Fatalf("a server verdict must not be reported as outcome unknown, got %v", got)
			}
		})
	}
}

// Optional store leg exercises the real unique index; required PG gates must
// provide AIKEY_TEST_DSN and AIKEY_TEST_PG=required, as other store tests do.
func TestImportedCredentialDuplicatePostgresConflict(t *testing.T) {
	svc := storeFixture(t)
	entry := cmn.ApiKeyEntry{TenantID: "fixture-tenant", ApiKey: "fixture-imported-key-only", Enabled: true}
	raw, first, err := svc.CreateAPIKey(context.Background(), entry)
	if err != nil || raw != "" || first == "" {
		t.Fatalf("first import failed: %v", err)
	}
	entry.TenantID = "fixture-other-tenant"
	raw, id, err := svc.CreateAPIKey(context.Background(), entry)
	var conflict *cmn.ConflictError
	if !errors.As(err, &conflict) || raw != "" || id != "" || err.Error() != "imported API key is already registered" {
		t.Fatal("duplicate import was not sanitized typed conflict")
	}
	original, err := svc.GetAPIKeyByID(first)
	if err != nil || original.TenantID != "fixture-tenant" {
		t.Fatal("duplicate attempt changed the original credential")
	}
}
