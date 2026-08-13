package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/storage/domain"
	"github.com/steveyegge/beads/internal/types"
)

// bd-6dnrw.44 item 11: the descendants CTE walked only parent-child edges,
// so children that exist purely by dotted-ID convention (classic ParentID
// fallback, issueops/filters.go) were dropped from --tree --parent under the
// proxied stack.
func (s *testSuite) TestGetDescendantsDottedOrphans() {
	r := s.issueRepo()
	deps := s.depRepo()

	for _, id := range []string{
		"bd-tree-r",     // root
		"bd-tree-c",     // edge child of root
		"bd-tree-c.7",   // dotted orphan under the edge child (no dep rows)
		"bd-tree-r.1",   // dotted orphan under the root (no dep rows)
		"bd-tree-r.1.2", // nested dotted orphan (no dep rows)
		"bd-tree-m",     // edge child of the dotted orphan bd-tree-r.1
		"bd-tree-z",     // unrelated root
		"bd-tree-r.9",   // dotted ID but re-parented by edge to bd-tree-z
	} {
		s.Require().NoError(r.Insert(s.Ctx(), newTestIssue(id, "tree "+id), "tester", domain.InsertIssueOpts{}))
	}

	for _, e := range []struct{ child, parent string }{
		{"bd-tree-c", "bd-tree-r"},
		{"bd-tree-m", "bd-tree-r.1"},
		{"bd-tree-r.9", "bd-tree-z"},
	} {
		s.Require().NoError(deps.Insert(s.Ctx(),
			&types.Dependency{IssueID: e.child, DependsOnID: e.parent, Type: types.DepParentChild}, "tester", domain.DepInsertOpts{}))
	}

	// Wisps participate in the same walk: an edge wisp child plus a dotted
	// wisp orphan, with their edges in wisp_dependencies. A non-empty wisps
	// table also flips walkWisps on, exercising the wisp CTE branches.
	for _, id := range []string{"bd-tree-wc", "bd-tree-r.5"} {
		s.Require().NoError(r.Insert(s.Ctx(), newTestIssue(id, "wisp "+id), "tester",
			domain.InsertIssueOpts{UseWispsTable: true}))
	}
	s.Require().NoError(deps.Insert(s.Ctx(),
		&types.Dependency{IssueID: "bd-tree-wc", DependsOnID: "bd-tree-r", Type: types.DepParentChild},
		"tester", domain.DepInsertOpts{UseWispsTable: true}))

	got, err := r.GetDescendants(s.Ctx(), "bd-tree-r", types.IssueFilter{})
	s.Require().NoError(err)

	ids := make([]string, len(got))
	for i, issue := range got {
		ids[i] = issue.ID
	}
	s.ElementsMatch([]string{
		"bd-tree-c",     // edge child
		"bd-tree-c.7",   // dotted orphan under edge child
		"bd-tree-r.1",   // dotted orphan under root
		"bd-tree-r.1.2", // nested dotted orphan
		"bd-tree-m",     // edge child hanging off a dotted orphan
		"bd-tree-wc",    // edge wisp child
		"bd-tree-r.5",   // dotted wisp orphan
	}, ids, "dotted-ID orphans must be walked like classic's ParentID fallback; "+
		"bd-tree-r.9 has a parent-child edge elsewhere and must stay out")

	skip := types.IssueFilter{SkipWisps: true}
	got, err = r.GetDescendants(s.Ctx(), "bd-tree-r", skip)
	s.Require().NoError(err)
	ids = ids[:0]
	for _, issue := range got {
		ids = append(ids, issue.ID)
	}
	s.ElementsMatch([]string{"bd-tree-c", "bd-tree-c.7", "bd-tree-r.1", "bd-tree-r.1.2", "bd-tree-m"},
		ids, "SkipWisps must drop the wisp rows but keep the dotted-ID issue walk")
}

// TestGetDescendantsFilteredByStatus guards the dolt 2.1.6 analyzer
// workaround (commit 341c7a5a4): when GetDescendants carries a level filter,
// each branch of the recursive descendants CTE references the same
// `id IN (SELECT id FROM <table> WHERE ...)` predicate. Inlining that
// subquery into 3+ branches trips the analyzer ("unable to find field with
// index N in row of M columns"); hoisting it into a named non-recursive CTE
// (issue_matches / wisp_matches) dodges it. The existing dotted-orphans test
// uses an empty filter and so never builds the predicate — this test does.
func (s *testSuite) TestGetDescendantsFilteredByStatus() {
	r := s.issueRepo()
	deps := s.depRepo()

	mk := func(id string, st types.Status) {
		iss := newTestIssue(id, "f "+id)
		iss.Status = st
		s.Require().NoError(r.Insert(s.Ctx(), iss, "tester", domain.InsertIssueOpts{}))
	}
	mk("bd-f-r", types.StatusOpen)     // root
	mk("bd-f-a", types.StatusOpen)     // open edge child
	mk("bd-f-b", types.StatusClosed)   // closed edge child (must be filtered out)
	mk("bd-f-r.1", types.StatusOpen)   // open dotted orphan
	mk("bd-f-r.2", types.StatusClosed) // closed dotted orphan (must be filtered out)

	for _, e := range []struct{ child, parent string }{
		{"bd-f-a", "bd-f-r"},
		{"bd-f-b", "bd-f-r"},
	} {
		s.Require().NoError(deps.Insert(s.Ctx(),
			&types.Dependency{IssueID: e.child, DependsOnID: e.parent, Type: types.DepParentChild},
			"tester", domain.DepInsertOpts{}))
	}

	st := types.StatusOpen
	got, err := r.GetDescendants(s.Ctx(), "bd-f-r", types.IssueFilter{Status: &st})
	s.Require().NoError(err) // dolt analyzer bug surfaces here without the named-CTE hoist

	ids := make([]string, len(got))
	for i, g := range got {
		ids[i] = g.ID
	}
	s.ElementsMatch([]string{"bd-f-a", "bd-f-r.1"}, ids,
		"only open descendants must be returned; the per-level filter must apply across edge and dotted branches")
}

// TestGetDescendantsDenseCycleIsLinear is the real-Dolt regression trip-wire
// for be-v8i. The prior recursive UNION ALL CTE emitted every path through a
// layered diamond and never terminated when an imported cycle was present. A
// visited-set walk returns each reachable node once, even though this graph has
// 10^5 root-to-leaf paths and three deliberately bypassed cycle-gate edges.
func (s *testSuite) TestGetDescendantsDenseCycleIsLinear() {
	const (
		layers = 6
		width  = 10
	)
	root := "bd-desc-dense-root"
	nodeID := func(layer, offset int) string {
		return fmt.Sprintf("bd-desc-dense-%d-%d", layer, offset)
	}

	ids := []string{root}
	for layer := 0; layer < layers; layer++ {
		for offset := 0; offset < width; offset++ {
			ids = append(ids, nodeID(layer, offset))
		}
	}
	s.seedDescendantIssues(ids)

	var edges [][2]string
	for offset := 0; offset < width; offset++ {
		edges = append(edges, [2]string{nodeID(0, offset), root})
	}
	for layer := 1; layer < layers; layer++ {
		for child := 0; child < width; child++ {
			for parent := 0; parent < width; parent++ {
				edges = append(edges, [2]string{nodeID(layer, child), nodeID(layer-1, parent)})
			}
		}
	}
	// Raw inserts model imported/bulk-written bad data: the mutation-time
	// cycle gate correctly rejects these edges, but readers must still finish.
	for offset := 0; offset < 3; offset++ {
		edges = append(edges, [2]string{nodeID(0, offset), nodeID(layers-1, offset)})
	}
	s.seedDescendantEdges(edges)

	started := time.Now()
	got, err := s.issueRepo().GetDescendants(s.Ctx(), root, types.IssueFilter{})
	s.Require().NoError(err)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		s.T().Fatalf("dense descendant walk took %v; visited-set traversal should remain near-instant", elapsed)
	}

	seen := make(map[string]struct{}, len(got))
	for _, issue := range got {
		if _, duplicate := seen[issue.ID]; duplicate {
			s.T().Fatalf("duplicate descendant %q returned from dense graph", issue.ID)
		}
		seen[issue.ID] = struct{}{}
	}
	if len(seen) != layers*width {
		s.T().Fatalf("dense walk found %d descendants, want %d", len(seen), layers*width)
	}
}

func (s *testSuite) seedDescendantIssues(ids []string) {
	const batchSize = 200
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		var query strings.Builder
		query.WriteString("INSERT INTO issues (id, title, description, design, acceptance_criteria, notes) VALUES ")
		args := make([]any, 0, (end-start)*2)
		for index, id := range ids[start:end] {
			if index > 0 {
				query.WriteString(", ")
			}
			query.WriteString("(?, ?, '', '', '', '')")
			args = append(args, id, id)
		}
		_, err := s.Runner().ExecContext(s.Ctx(), query.String(), args...)
		s.Require().NoError(err)
	}
}

func (s *testSuite) seedDescendantEdges(edges [][2]string) {
	const batchSize = 200
	for start := 0; start < len(edges); start += batchSize {
		end := min(start+batchSize, len(edges))
		var query strings.Builder
		query.WriteString("INSERT INTO dependencies (id, issue_id, depends_on_issue_id, type, created_at, created_by, metadata) VALUES ")
		args := make([]any, 0, (end-start)*2)
		for index, edge := range edges[start:end] {
			if index > 0 {
				query.WriteString(", ")
			}
			query.WriteString("(UUID(), ?, ?, 'parent-child', NOW(), 'tester', JSON_OBJECT())")
			args = append(args, edge[0], edge[1])
		}
		_, err := s.Runner().ExecContext(s.Ctx(), query.String(), args...)
		s.Require().NoError(err)
	}
}
