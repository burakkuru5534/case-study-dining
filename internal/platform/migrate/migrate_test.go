package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- Load: unit tests, no database ----

func TestLoad(t *testing.T) {
	t.Parallel()

	file := func(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }

	tests := []struct {
		name         string
		fsys         fstest.MapFS
		wantVersions []int64
		wantErr      string
	}{
		{
			name:         "orders numerically, not lexically",
			fsys:         fstest.MapFS{"2_b.sql": file("B"), "10_j.sql": file("J"), "1_a.sql": file("A"), "3_c.sql": file("C"), "4_d.sql": file("D"), "5_e.sql": file("E"), "6_f.sql": file("F"), "7_g.sql": file("G"), "8_h.sql": file("H"), "9_i.sql": file("I")},
			wantVersions: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
		},
		{
			name:         "ignores non-sql files and directories",
			fsys:         fstest.MapFS{"0001_a.sql": file("A"), "embed.go": file("package x"), "README.md": file("x"), "sub/0002_b.sql": file("B")},
			wantVersions: []int64{1},
		},
		{
			name:         "no migrations is valid",
			fsys:         fstest.MapFS{},
			wantVersions: nil,
		},
		{name: "gap in versions", fsys: fstest.MapFS{"0001_a.sql": file("A"), "0003_c.sql": file("C")}, wantErr: "gap: expected 2, found 3"},
		{name: "must start at 1", fsys: fstest.MapFS{"0002_b.sql": file("B")}, wantErr: "gap: expected 1, found 2"},
		{name: "duplicate version", fsys: fstest.MapFS{"0001_a.sql": file("A"), "1_b.sql": file("B")}, wantErr: "duplicate migration version 1"},
		{name: "missing name", fsys: fstest.MapFS{"0001.sql": file("A")}, wantErr: "invalid migration file name"},
		{name: "dash separator", fsys: fstest.MapFS{"0001-create.sql": file("A")}, wantErr: "invalid migration file name"},
		{name: "uppercase name", fsys: fstest.MapFS{"0001_Create.sql": file("A")}, wantErr: "invalid migration file name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Load(tt.fsys)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error: got %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var versions []int64
			for _, m := range got {
				versions = append(versions, m.Version)
			}
			if !slices.Equal(versions, tt.wantVersions) {
				t.Fatalf("versions: got %v, want %v", versions, tt.wantVersions)
			}
		})
	}
}

func TestLoadKeepsContentAndName(t *testing.T) {
	t.Parallel()

	got, err := Load(fstest.MapFS{"0001_create_orders.sql": {Data: []byte("CREATE TABLE x ();")}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "create_orders" || got[0].SQL != "CREATE TABLE x ();" {
		t.Fatalf("got %+v", got[0])
	}
}

// ---- Run: integration tests against PostgreSQL ----
//
// Each test gets its own schema (via search_path), so tests never see each
// other's tables. They do share the database-wide advisory lock, which is why
// they don't use t.Parallel.

type testDB struct {
	t      *testing.T
	cfg    *pgx.ConnConfig
	schema string
}

func newTestDB(t *testing.T) *testDB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "migrate_test_" + hex.EncodeToString(b)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.RuntimeParams["search_path"] = schema
	return &testDB{t: t, cfg: cfg, schema: schema}
}

// conn opens a new dedicated connection scoped to the test schema.
func (d *testDB) conn() *pgx.Conn {
	d.t.Helper()
	c, err := pgx.ConnectConfig(context.Background(), d.cfg)
	if err != nil {
		d.t.Fatalf("connect: %v", err)
	}
	d.t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func (d *testDB) appliedVersions() []int64 {
	d.t.Helper()
	rows, err := d.conn().Query(context.Background(), "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		d.t.Fatalf("query schema_migrations: %v", err)
	}
	v, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		d.t.Fatalf("collect: %v", err)
	}
	return v
}

func (d *testDB) tableExists(name string) bool {
	d.t.Helper()
	var exists bool
	err := d.conn().QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", d.schema+"."+name).Scan(&exists)
	if err != nil {
		d.t.Fatalf("to_regclass: %v", err)
	}
	return exists
}

var discard = slog.New(slog.DiscardHandler)

func TestRun_AppliesInOrderAndIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Loaded from an FS so ordering goes through Load. 0002 fails unless 0001 ran first.
	migrations, err := Load(fstest.MapFS{
		"0002_add_column.sql":   {Data: []byte("ALTER TABLE items ADD COLUMN price bigint NOT NULL DEFAULT 0;")},
		"0001_create_items.sql": {Data: []byte("CREATE TABLE items (id bigint PRIMARY KEY);")},
		"0003_two_statements.sql": {Data: []byte(`
			CREATE TABLE a (id int);
			CREATE TABLE b (id int);`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	applied, err := Run(ctx, db.conn(), migrations, discard)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if applied != 3 {
		t.Fatalf("first run applied %d, want 3", applied)
	}
	if got := db.appliedVersions(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("applied versions: got %v", got)
	}
	if !db.tableExists("a") || !db.tableExists("b") {
		t.Fatal("multi-statement migration did not create both tables")
	}

	applied, err = Run(ctx, db.conn(), migrations, discard)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if applied != 0 {
		t.Fatalf("second run applied %d, want 0", applied)
	}
}

func TestRun_FailedMigrationRollsBack(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	good := Migration{Version: 1, Name: "first", SQL: "CREATE TABLE first (id int);"}
	// First statement succeeds, second fails: the whole migration must roll back.
	bad := Migration{Version: 2, Name: "broken", SQL: `
		CREATE TABLE partial (id int);
		SELECT * FROM table_that_does_not_exist;`}

	applied, err := Run(ctx, db.conn(), []Migration{good, bad}, discard)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "0002_broken") {
		t.Errorf("error should name the migration: %v", err)
	}
	if applied != 1 {
		t.Errorf("applied %d, want 1", applied)
	}
	if db.tableExists("partial") {
		t.Error("partial table exists: failed migration was not rolled back")
	}
	if got := db.appliedVersions(); !slices.Equal(got, []int64{1}) {
		t.Errorf("applied versions: got %v, want [1]", got)
	}

	// After fixing the migration, a rerun applies only the fixed one.
	fixed := Migration{Version: 2, Name: "broken", SQL: "CREATE TABLE partial (id int);"}
	applied, err = Run(ctx, db.conn(), []Migration{good, fixed}, discard)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if applied != 1 || !db.tableExists("partial") {
		t.Fatalf("rerun applied %d, partial exists=%v", applied, db.tableExists("partial"))
	}
}

func TestRun_ConcurrentRunnersApplyEachMigrationOnce(t *testing.T) {
	db := newTestDB(t)

	// CREATE TABLE is not idempotent: if two runners applied 0001, one would fail.
	// pg_sleep widens the window in which an unlocked runner would interleave.
	migrations := []Migration{
		{Version: 1, Name: "create", SQL: "CREATE TABLE counter (id int); SELECT pg_sleep(0.3);"},
		{Version: 2, Name: "alter", SQL: "ALTER TABLE counter ADD COLUMN n int;"},
	}

	const runners = 5
	conns := make([]*pgx.Conn, runners)
	for i := range conns {
		conns[i] = db.conn()
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		total   int
		errList []error
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := Run(ctx, c, migrations, discard)
			mu.Lock()
			defer mu.Unlock()
			total += n
			if err != nil {
				errList = append(errList, err)
			}
		}()
	}
	wg.Wait()

	if len(errList) > 0 {
		t.Fatalf("runner errors: %v", errList)
	}
	if total != len(migrations) {
		t.Fatalf("total applied across runners = %d, want %d", total, len(migrations))
	}
	if got := db.appliedVersions(); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("applied versions: got %v", got)
	}
}

func TestRun_ReleasesLockAfterRun(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	runner := db.conn()
	if _, err := Run(ctx, runner, []Migration{{Version: 1, Name: "x", SQL: "SELECT 1;"}}, discard); err != nil {
		t.Fatal(err)
	}

	// The runner connection is still open: if the lock were leaked, this would be false.
	var got bool
	other := db.conn()
	if err := other.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockKey).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("lock still held after Run returned")
	}
	_, _ = other.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
}

func TestRun_WaitingForLockRespectsContext(t *testing.T) {
	db := newTestDB(t)

	holder := db.conn()
	if _, err := holder.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey) }()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx, db.conn(), []Migration{{Version: 1, Name: "x", SQL: "SELECT 1;"}}, discard)
	if err == nil || !strings.Contains(err.Error(), "acquire migration lock") {
		t.Fatalf("expected lock acquisition error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Run waited %s despite 300ms context", elapsed)
	}
	if db.tableExists("schema_migrations") {
		t.Error("schema_migrations created without holding the lock")
	}
}

func TestRun_DatabaseAheadOfBinary(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	all := []Migration{
		{Version: 1, Name: "a", SQL: "SELECT 1;"},
		{Version: 2, Name: "b", SQL: "SELECT 1;"},
	}
	if _, err := Run(ctx, db.conn(), all, discard); err != nil {
		t.Fatal(err)
	}

	_, err := Run(ctx, db.conn(), all[:1], discard)
	if err == nil || !strings.Contains(err.Error(), "binary is older than the database") {
		t.Fatalf("expected database-ahead error, got %v", err)
	}
}

func TestRun_RejectsGapWithoutTouchingDatabase(t *testing.T) {
	db := newTestDB(t)

	_, err := Run(context.Background(), db.conn(), []Migration{
		{Version: 1, Name: "a", SQL: "SELECT 1;"},
		{Version: 3, Name: "c", SQL: "SELECT 1;"},
	}, discard)
	if err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("expected gap error, got %v", err)
	}
	if db.tableExists("schema_migrations") {
		t.Error("database was touched despite invalid migration set")
	}
}
