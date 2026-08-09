package issueops

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin the wire protocol of the be-dlw descendant-walk rewrite:
// one IN-batched query per level per dependency table, a visited set that
// keeps re-discovered nodes out of later frontiers, the optional-wisp-table
// fallback, and the bounded-walk error. Graph-shape behavior (diamonds,
// cycles, parity with the replaced recursive CTE) is exercised against a real
// engine in internal/storage/dolt/descendant_walk_test.go.

var (
	descendantDepsQuery = regexp.QuoteMeta("SELECT issue_id FROM dependencies WHERE type = 'parent-child'")
	descendantWispQuery = regexp.QuoteMeta("SELECT issue_id FROM wisp_dependencies WHERE type = 'parent-child'")
)

func descendantRows(ids ...string) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{"issue_id"})
	for _, id := range ids {
		rows.AddRow(id)
	}
	return rows
}

func TestGetDescendantIDsInTxEmptyRootMakesNoQueries(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	got, err := GetDescendantIDsInTx(context.Background(), tx, "", 5)
	if err != nil || got != nil {
		t.Fatalf("GetDescendantIDsInTx(empty root) = (%v, %v), want (nil, nil)", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("empty root must not touch the database: %v", err)
	}
}

// TestGetDescendantIDsInTxWalksLevelsWithVisitedSet drives a two-level walk
// where level two re-discovers a level-one node. The re-discovered node must
// not rejoin the frontier — the per-path dedup this walk replaced would have
// re-expanded it once per route, which is exactly the combinatorial defect.
func TestGetDescendantIDsInTxWalksLevelsWithVisitedSet(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(descendantDepsQuery).WithArgs("vs-root").
		WillReturnRows(descendantRows("vs-a", "vs-b"))
	mock.ExpectQuery(descendantWispQuery).WithArgs("vs-root").
		WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantDepsQuery).WithArgs("vs-a", "vs-b").
		WillReturnRows(descendantRows("vs-c", "vs-a"))
	mock.ExpectQuery(descendantWispQuery).WithArgs("vs-a", "vs-b").
		WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantDepsQuery).WithArgs("vs-c").
		WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantWispQuery).WithArgs("vs-c").
		WillReturnRows(descendantRows())

	got, err := GetDescendantIDsInTx(context.Background(), tx, "vs-root", 0)
	if err != nil {
		t.Fatalf("GetDescendantIDsInTx: %v", err)
	}
	if want := []string{"vs-a", "vs-b", "vs-c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("descendants = %v, want %v (each node once, in discovery order)", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet query expectations: %v", err)
	}
}

// TestGetDescendantIDsInTxWispTableFallback pins the optional-table contract:
// a missing wisp_dependencies table (error 1146) downgrades the walk to the
// permanent table only — for the current level AND every later one — instead
// of failing it.
func TestGetDescendantIDsInTxWispTableFallback(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(descendantDepsQuery).WithArgs("fb-root").
		WillReturnRows(descendantRows("fb-c1"))
	mock.ExpectQuery(descendantWispQuery).WithArgs("fb-root").
		WillReturnError(errors.New("Error 1146 (42S02): table 'test.wisp_dependencies' doesn't exist"))
	mock.ExpectQuery(descendantDepsQuery).WithArgs("fb-c1").
		WillReturnRows(descendantRows())

	got, err := GetDescendantIDsInTx(context.Background(), tx, "fb-root", 0)
	if err != nil {
		t.Fatalf("GetDescendantIDsInTx with missing wisp table: %v", err)
	}
	if want := []string{"fb-c1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("descendants = %v, want %v", got, want)
	}
	// ExpectationsWereMet also proves the second level queried the permanent
	// table only: no wisp expectation exists for it.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("wisp fallback must stop querying the missing table: %v", err)
	}
}

// TestGetDescendantIDsInTxBatchesWideFrontiers proves a frontier wider than
// queryBatchSize splits into bounded IN batches rather than one unbounded
// statement.
func TestGetDescendantIDsInTxBatchesWideFrontiers(t *testing.T) {
	t.Parallel()

	wide := make([]string, queryBatchSize+1)
	rows := descendantRows()
	firstBatch := make([]driver.Value, 0, queryBatchSize)
	for i := range wide {
		wide[i] = fmt.Sprintf("bt-c%03d", i)
		rows.AddRow(wide[i])
		if i < queryBatchSize {
			firstBatch = append(firstBatch, wide[i])
		}
	}

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(descendantDepsQuery).WithArgs("bt-root").WillReturnRows(rows)
	mock.ExpectQuery(descendantWispQuery).WithArgs("bt-root").WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantDepsQuery).WithArgs(firstBatch...).WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantDepsQuery).WithArgs(wide[queryBatchSize]).WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantWispQuery).WithArgs(firstBatch...).WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantWispQuery).WithArgs(wide[queryBatchSize]).WillReturnRows(descendantRows())

	got, err := GetDescendantIDsInTx(context.Background(), tx, "bt-root", 0)
	if err != nil {
		t.Fatalf("GetDescendantIDsInTx: %v", err)
	}
	if !reflect.DeepEqual(got, wide) {
		t.Errorf("descendants = %d ids, want the %d seeded children", len(got), len(wide))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("frontier batching expectations: %v", err)
	}
}

// TestGetDescendantIDsInTxMaxDepthStopsWithContractError pins the bounded
// walk: nodes discovered at the bound mean the walk below them never ran, so
// the caller gets the same "reached max depth" error the replaced CTE raised,
// byte-identical, and no further level is queried.
func TestGetDescendantIDsInTxMaxDepthStopsWithContractError(t *testing.T) {
	t.Parallel()

	_, mock, tx := beginMockTx(t)
	mock.ExpectQuery(descendantDepsQuery).WithArgs("md-root").
		WillReturnRows(descendantRows("md-c1"))
	mock.ExpectQuery(descendantWispQuery).WithArgs("md-root").
		WillReturnRows(descendantRows())
	mock.ExpectQuery(descendantDepsQuery).WithArgs("md-c1").
		WillReturnRows(descendantRows("md-c2"))
	mock.ExpectQuery(descendantWispQuery).WithArgs("md-c1").
		WillReturnRows(descendantRows())

	_, err := GetDescendantIDsInTx(context.Background(), tx, "md-root", 2)
	if err == nil {
		t.Fatal("GetDescendantIDsInTx(maxDepth=2) on a deeper chain = nil error, want the bounded-walk refusal")
	}
	const want = "parent descendant traversal for md-root reached max depth 2"
	if err.Error() != want {
		t.Errorf("error = %q, want byte-identical %q", err.Error(), want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the walk must stop at the bound without querying deeper levels: %v", err)
	}
}
