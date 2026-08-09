package dolt

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/depid"
	"github.com/steveyegge/beads/internal/storage/issueops"
)

// These tests pin the be-dlw fix: the parent-child descendant walk used a
// recursive CTE whose dedup was a per-path string match (LOCATE over a
// CONCAT'd id list), so diamond-dense graphs made it enumerate paths —
// combinatorial work that starved the shared single-writer Dolt server. The
// walk is now a breadth-first traversal in Go with a visited set, O(V+E).
//
// The dense-graph case is the regression trip-wire: its layered graph has
// ~28^8 root-to-leaf paths, so any return to path-enumeration semantics jumps
// from milliseconds back to hours and fails the wall-clock bound (and the
// test context deadline) rather than flaking marginally.

// seedIssuesRaw bulk-inserts bare open task rows so graph fixtures don't pay
// the full CreateIssue write path per node.
func seedIssuesRaw(ctx context.Context, t *testing.T, db *sql.DB, ids []string) {
	t.Helper()
	const chunk = 1000
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		var b strings.Builder
		b.WriteString("INSERT INTO issues (id, title, description, design, acceptance_criteria, notes, status, priority, issue_type) VALUES ")
		args := make([]any, 0, (end-start)*2)
		for i, id := range ids[start:end] {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(?, ?, '', '', '', '', 'open', 2, 'task')")
			args = append(args, id, id)
		}
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			t.Fatalf("seed issues [%d:%d]: %v", start, end, err)
		}
	}
}

// seedParentChildEdgesRaw bulk-inserts parent-child dependency rows directly.
// Raw SQL is deliberate twice over: it keeps 5k-edge fixtures fast, and it can
// materialize parent-child cycles the AddDependency gate would refuse — the
// shape imports and bulk writers leave behind, which the walk must survive.
func seedParentChildEdgesRaw(ctx context.Context, t *testing.T, db *sql.DB, edges [][2]string) {
	t.Helper()
	const chunk = 1000
	for start := 0; start < len(edges); start += chunk {
		end := min(start+chunk, len(edges))
		var b strings.Builder
		b.WriteString("INSERT INTO dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by, metadata) VALUES ")
		args := make([]any, 0, (end-start)*3)
		for i, e := range edges[start:end] {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(?, ?, ?, 'parent-child', NOW(), 'tester', JSON_OBJECT())")
			args = append(args, depid.New(e[0], e[1]), e[0], e[1])
		}
		if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
			t.Fatalf("seed parent-child edges [%d:%d]: %v", start, end, err)
		}
	}
}

// oldPathCTEDescendants is the replaced implementation, kept verbatim as the
// parity reference: recursive CTE over parent-child edges, per-path string
// dedup, per-path depth. It must only be pointed at small fixtures — on dense
// graphs it enumerates paths, which is the defect the production walk no
// longer has.
func oldPathCTEDescendants(ctx context.Context, db *sql.DB, rootID string, maxDepth int) ([]string, error) {
	const depTarget = "COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external)"
	query := fmt.Sprintf(`
		WITH RECURSIVE
		parent_edges(issue_id, depends_on_id) AS (
			SELECT issue_id, %s FROM dependencies WHERE type = 'parent-child'
			UNION ALL
			SELECT issue_id, %s FROM wisp_dependencies WHERE type = 'parent-child'
		),
		descendants(id, depth, path) AS (
			SELECT issue_id, 1, CONCAT(',', ?, ',', issue_id, ',')
			FROM parent_edges
			WHERE depends_on_id = ?
			UNION ALL
			SELECT e.issue_id, d.depth + 1, CONCAT(d.path, e.issue_id, ',')
			FROM parent_edges e
			JOIN descendants d ON e.depends_on_id = d.id
			WHERE (? <= 0 OR d.depth < ?)
			  AND LOCATE(CONCAT(',', e.issue_id, ','), d.path) = 0
		)
		SELECT id, depth FROM descendants WHERE id <> ?
	`, depTarget, depTarget)
	rows, err := db.QueryContext(ctx, query, rootID, rootID, maxDepth, maxDepth, rootID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []string
	reachedMaxDepth := false
	for rows.Next() {
		var id string
		var depth int
		if err := rows.Scan(&id, &depth); err != nil {
			return nil, err
		}
		result = append(result, id)
		if maxDepth > 0 && depth >= maxDepth {
			reachedMaxDepth = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if reachedMaxDepth {
		return nil, fmt.Errorf("parent descendant traversal for %s reached max depth %d", rootID, maxDepth)
	}
	return result, nil
}

func sortedUniq(ids []string) []string {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// TestGetDescendantIDs_DenseLayeredGraphIsLinear is the be-dlw regression
// trip-wire. The fixture is a root over 8 fully-connected layers of 28 nodes
// (~5.5k parent-child edges, ~28^8 distinct root-to-leaf paths) plus three
// cycle back-edges. Path-enumeration semantics cannot finish this walk inside
// any test deadline; the visited-set walk touches each of the 225 nodes once.
// The same graph then feeds the add-time cycle gate, which the acceptance
// criteria require to stay correct and fast on dense graphs.
func TestGetDescendantIDs_DenseLayeredGraphIsLinear(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	const layers = 8
	const width = 28
	root := "dw-dense-root"
	outside := "dw-dense-outside"

	nodeID := func(layer, i int) string { return fmt.Sprintf("dw-dense-l%d-%d", layer, i) }

	ids := []string{root, outside}
	var want []string
	for l := 0; l < layers; l++ {
		for i := 0; i < width; i++ {
			ids = append(ids, nodeID(l, i))
			want = append(want, nodeID(l, i))
		}
	}
	var edges [][2]string
	for i := 0; i < width; i++ {
		edges = append(edges, [2]string{nodeID(0, i), root})
	}
	for l := 1; l < layers; l++ {
		for child := 0; child < width; child++ {
			for parent := 0; parent < width; parent++ {
				edges = append(edges, [2]string{nodeID(l, child), nodeID(l-1, parent)})
			}
		}
	}
	// Cycle back-edges: three bottom-layer nodes become parents of top-layer
	// nodes, closing root-reachable cycles the walk must terminate through.
	for i := 0; i < 3; i++ {
		edges = append(edges, [2]string{nodeID(0, i), nodeID(layers-1, i)})
	}

	seedIssuesRaw(ctx, t, store.db, ids)
	seedParentChildEdgesRaw(ctx, t, store.db, edges)

	start := time.Now()
	got, err := issueops.GetDescendantIDsInTx(ctx, store.db, root, 0)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GetDescendantIDsInTx on dense graph: %v", err)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("dense descendant walk took %v; the visited-set walk finishes in milliseconds, so this is the path-enumeration regression", elapsed)
	}
	if len(got) != len(sortedUniq(got)) {
		t.Fatalf("descendant walk returned duplicate ids: %d rows, %d unique", len(got), len(sortedUniq(got)))
	}
	wantSorted := sortedUniq(want)
	gotSorted := sortedUniq(got)
	if len(gotSorted) != len(wantSorted) {
		t.Fatalf("dense walk found %d descendants, want %d", len(gotSorted), len(wantSorted))
	}
	for i := range wantSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("dense walk set mismatch at %d: got %s, want %s", i, gotSorted[i], wantSorted[i])
		}
	}

	// Add-time cycle gate on the same dense graph: "root depends on a bottom
	// node" closes a cycle through every layer and must be refused; a
	// dependency on an unconnected node must not; and both probes must stay
	// far from the path-enumeration wall.
	start = time.Now()
	wouldCycle, err := issueops.WouldCreateSchedulingCycleInTx(ctx, store.db, root, nodeID(layers-1, 5), nil)
	if err != nil {
		t.Fatalf("WouldCreateSchedulingCycleInTx(cycle probe): %v", err)
	}
	if !wouldCycle {
		t.Fatal("cycle-add gate missed a cycle through the dense layered graph")
	}
	noCycle, err := issueops.WouldCreateSchedulingCycleInTx(ctx, store.db, nodeID(layers-1, 5), outside, nil)
	if err != nil {
		t.Fatalf("WouldCreateSchedulingCycleInTx(acyclic probe): %v", err)
	}
	if noCycle {
		t.Fatal("cycle-add gate refused an edge to an unconnected node")
	}
	if elapsed = time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("cycle-add probes took %v on the dense graph; the gate must stay visited-set-linear", elapsed)
	}
}

// TestGetDescendantIDs_ParityWithReplacedPathCTE proves the visited-set walk
// reports exactly the descendant SET the replaced recursive CTE reported, on
// every fixture shape the CTE could actually finish: a plain tree, a diamond
// (two routes to one node), a cycle below the root, and a mixed-type fan
// where only the parent-child edge may be walked. The CTE emits one row per
// path, so parity is on sets — and the new walk must also be duplicate-free,
// which the CTE never was on diamonds.
func TestGetDescendantIDs_ParityWithReplacedPathCTE(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssuesRaw(ctx, t, store.db, []string{
		"dwp-tree", "dwp-tree-a", "dwp-tree-b", "dwp-tree-c",
		"dwp-dia", "dwp-dia-x", "dwp-dia-y", "dwp-dia-d", "dwp-dia-leaf",
		"dwp-cyc", "dwp-cyc-p", "dwp-cyc-q",
		"dwp-mix", "dwp-mix-child", "dwp-mix-blocked",
	})
	seedParentChildEdgesRaw(ctx, t, store.db, [][2]string{
		{"dwp-tree-a", "dwp-tree"}, {"dwp-tree-b", "dwp-tree"}, {"dwp-tree-c", "dwp-tree-a"},
		{"dwp-dia-x", "dwp-dia"}, {"dwp-dia-y", "dwp-dia"},
		{"dwp-dia-d", "dwp-dia-x"}, {"dwp-dia-d", "dwp-dia-y"},
		{"dwp-dia-leaf", "dwp-dia-d"},
		{"dwp-cyc-p", "dwp-cyc"}, {"dwp-cyc-q", "dwp-cyc-p"}, {"dwp-cyc-p", "dwp-cyc-q"},
		{"dwp-mix-child", "dwp-mix"},
	})
	// The mixed fixture's second edge is a blocking dependency; descendant
	// walks are parent-child-only and must not follow it.
	if _, err := store.db.ExecContext(ctx,
		"INSERT INTO dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by, metadata) VALUES (?, ?, ?, 'blocks', NOW(), 'tester', JSON_OBJECT())",
		depid.New("dwp-mix-blocked", "dwp-mix"), "dwp-mix-blocked", "dwp-mix"); err != nil {
		t.Fatalf("seed blocks edge: %v", err)
	}

	for _, tc := range []struct {
		root string
		want []string
	}{
		{"dwp-tree", []string{"dwp-tree-a", "dwp-tree-b", "dwp-tree-c"}},
		{"dwp-dia", []string{"dwp-dia-x", "dwp-dia-y", "dwp-dia-d", "dwp-dia-leaf"}},
		{"dwp-cyc", []string{"dwp-cyc-p", "dwp-cyc-q"}},
		{"dwp-mix", []string{"dwp-mix-child"}},
	} {
		oldIDs, err := oldPathCTEDescendants(ctx, store.db, tc.root, 0)
		if err != nil {
			t.Fatalf("reference CTE on %s: %v", tc.root, err)
		}
		newIDs, err := issueops.GetDescendantIDsInTx(ctx, store.db, tc.root, 0)
		if err != nil {
			t.Fatalf("GetDescendantIDsInTx on %s: %v", tc.root, err)
		}
		oldSet := sortedUniq(oldIDs)
		newSet := sortedUniq(newIDs)
		wantSet := sortedUniq(tc.want)
		if strings.Join(oldSet, ",") != strings.Join(wantSet, ",") {
			t.Errorf("reference CTE on %s: got %v, want %v", tc.root, oldSet, wantSet)
		}
		if strings.Join(newSet, ",") != strings.Join(oldSet, ",") {
			t.Errorf("parity break on %s: new walk %v, replaced CTE %v", tc.root, newSet, oldSet)
		}
		if len(newIDs) != len(newSet) {
			t.Errorf("new walk on %s returned duplicates: %v", tc.root, newIDs)
		}
	}
}

// TestGetDescendantIDs_MaxDepthSemanticsPreserved pins the bounded-walk
// contract across the rewrite on a 3-deep chain: a bound the graph reaches
// fails with the same error text the CTE produced (a node AT maxDepth means
// the walk cannot prove completeness), one level of headroom succeeds with
// the full set, and 0 means unbounded.
func TestGetDescendantIDs_MaxDepthSemanticsPreserved(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx, cancel := testContext(t)
	defer cancel()

	seedIssuesRaw(ctx, t, store.db, []string{"dwmd-root", "dwmd-c1", "dwmd-c2", "dwmd-c3"})
	seedParentChildEdgesRaw(ctx, t, store.db, [][2]string{
		{"dwmd-c1", "dwmd-root"}, {"dwmd-c2", "dwmd-c1"}, {"dwmd-c3", "dwmd-c2"},
	})

	if _, err := oldPathCTEDescendants(ctx, store.db, "dwmd-root", 3); err == nil {
		t.Fatal("reference CTE with maxDepth=3 on a 3-deep chain must error; the parity claim below would be vacuous")
	}
	_, err := issueops.GetDescendantIDsInTx(ctx, store.db, "dwmd-root", 3)
	if err == nil {
		t.Fatal("maxDepth=3 on a 3-deep chain must error: a node at the bound means the walk below it was never expanded")
	}
	const wantMsg = "parent descendant traversal for dwmd-root reached max depth 3"
	if err.Error() != wantMsg {
		t.Errorf("bounded-walk error = %q, want byte-identical %q", err.Error(), wantMsg)
	}

	for _, maxDepth := range []int{4, 0} {
		got, err := issueops.GetDescendantIDsInTx(ctx, store.db, "dwmd-root", maxDepth)
		if err != nil {
			t.Fatalf("maxDepth=%d: %v", maxDepth, err)
		}
		if want := "dwmd-c1,dwmd-c2,dwmd-c3"; strings.Join(sortedUniq(got), ",") != want {
			t.Errorf("maxDepth=%d: got %v, want [%s]", maxDepth, got, want)
		}
	}
}
