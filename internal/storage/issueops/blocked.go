package issueops

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads/internal/types"
)

type blockingDepRecord struct {
	issueID, dependsOnID, depType string
	metadata                      sql.NullString
}

func optionalBlockedTable(table string) bool {
	return table == "wisps" || table == "wisp_dependencies"
}

func loadBlockingDepsForIssueIDsInTx(ctx context.Context, tx DBTX, depTables []string, issueIDs []string) ([]blockingDepRecord, error) {
	var deps []blockingDepRecord
	for _, depTable := range depTables {
		//nolint:gosec // G201: depTable is a hardcoded constant.
		query := fmt.Sprintf(`
			SELECT issue_id, %s AS depends_on_id, type, metadata FROM %s
			WHERE issue_id = ?
			  AND (type = 'blocks' OR type = 'waits-for' OR type = 'conditional-blocks')
		`, DepTargetExpr, depTable)
		for _, id := range issueIDs {
			rows, err := tx.QueryContext(ctx, query, id)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					break
				}
				return nil, fmt.Errorf("compute blocked IDs: deps from %s: %w", depTable, err)
			}
			for rows.Next() {
				var rec blockingDepRecord
				if err := rows.Scan(&rec.issueID, &rec.dependsOnID, &rec.depType, &rec.metadata); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("compute blocked IDs: scan dep: %w", err)
				}
				deps = append(deps, rec)
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("compute blocked IDs: dep rows from %s: %w", depTable, err)
			}
		}
	}
	return deps, nil
}

func loadParentIDsForChildrenInTx(ctx context.Context, tx DBTX, depTables []string, childIDs []string) (map[string]string, error) {
	childParents := make(map[string]string)
	for _, depTable := range depTables {
		//nolint:gosec // G201: depTable is a hardcoded constant.
		query := fmt.Sprintf(`
			SELECT issue_id, %s AS depends_on_id FROM %s
			WHERE issue_id = ?
			  AND type = 'parent-child'
		`, DepTargetExpr, depTable)
		for _, id := range childIDs {
			rows, err := tx.QueryContext(ctx, query, id)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					break
				}
				return nil, fmt.Errorf("candidate parents from %s: %w", depTable, err)
			}
			for rows.Next() {
				var childID, parentID string
				if err := rows.Scan(&childID, &parentID); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan candidate parent: %w", err)
				}
				childParents[childID] = parentID
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("candidate parent rows from %s: %w", depTable, err)
			}
		}
	}
	return childParents, nil
}

//nolint:gosec // G201: tables are hardcoded
func GetChildrenWithParentsInTx(ctx context.Context, tx DBTX, parentIDs []string) (map[string]string, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	result := make(map[string]string)
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		query := fmt.Sprintf(`
			SELECT issue_id, %s AS depends_on_id FROM %s
			WHERE type = 'parent-child' AND %s = ?
		`, DepTargetExpr, depTable, DepTargetExpr)
		for _, parentID := range parentIDs {
			rows, err := tx.QueryContext(ctx, query, parentID)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					break
				}
				return nil, fmt.Errorf("get children with parents from %s: %w", depTable, err)
			}
			for rows.Next() {
				var childID, parentID string
				if err := rows.Scan(&childID, &parentID); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan children with parents: %w", err)
				}
				result[childID] = parentID
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("children with parents rows from %s: %w", depTable, err)
			}
		}
	}
	return result, nil
}

//nolint:gosec // G201: tables are hardcoded
func GetChildrenOfIssuesInTx(ctx context.Context, tx DBTX, parentIDs []string) ([]string, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	var children []string
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		query := fmt.Sprintf(`
			SELECT issue_id FROM %s
			WHERE type = 'parent-child' AND %s = ?
		`, depTable, DepTargetExpr)
		for _, parentID := range parentIDs {
			rows, err := tx.QueryContext(ctx, query, parentID)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					break
				}
				return nil, fmt.Errorf("get children of issues from %s: %w", depTable, err)
			}
			for rows.Next() {
				var childID string
				if err := rows.Scan(&childID); err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("scan child: %w", err)
				}
				children = append(children, childID)
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("children rows from %s: %w", depTable, err)
			}
		}
	}
	return children, nil
}

// GetDescendantIDsInTx returns the IDs of every transitive parent-child
// descendant of rootID, walking the edges breadth-first in Go with a visited
// set.
//
// It replaces a recursive CTE that deduplicated by matching each discovered
// path as a delimiter-joined id string (LOCATE over a CONCAT accumulator).
// That guard is per-path, so a node reachable through k distinct routes was
// expanded k times and the walk went combinatorial on diamond-dense graphs —
// single invocations ran 14s+ on a few-hundred-edge graph, and one such query
// starves every other connection on a shared single-writer Dolt server
// (be-dlw). The BFS admits each node to the frontier once: O(V+E) total work,
// one IN-batched query per level per dependency table.
//
// maxDepth <= 0 is unbounded; termination on cyclic data (imports can land
// parent-child cycles the add-time gate would refuse) comes from the visited
// set, not a depth bound. With maxDepth > 0 the walk fails once any node's
// shortest path from rootID reaches maxDepth levels — the same "reached max
// depth" contract the CTE had, minus its false positives: the CTE measured
// per-path depth, so a node also reachable through a redundant longer route
// could trip the bound spuriously.
func GetDescendantIDsInTx(ctx context.Context, tx DBTX, rootID string, maxDepth int) ([]string, error) {
	if rootID == "" {
		return nil, nil
	}

	includeWisps := true
	visited := map[string]struct{}{rootID: {}}
	frontier := []string{rootID}
	var result []string
	for depth := 1; len(frontier) > 0; depth++ {
		var level []string
		for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
			if depTable == "wisp_dependencies" && !includeWisps {
				continue
			}
			children, err := parentChildSourcesInTx(ctx, tx, depTable, frontier)
			if err != nil {
				if optionalBlockedTable(depTable) && isTableNotExistError(err) {
					includeWisps = false
					continue
				}
				return nil, err
			}
			level = append(level, children...)
		}
		frontier = frontier[:0]
		for _, id := range level {
			if _, seen := visited[id]; seen {
				continue
			}
			visited[id] = struct{}{}
			result = append(result, id)
			frontier = append(frontier, id)
		}
		if maxDepth > 0 && depth >= maxDepth && len(frontier) > 0 {
			return nil, fmt.Errorf("parent descendant traversal for %s reached max depth %d", rootID, maxDepth)
		}
	}
	return result, nil
}

// parentChildSourcesInTx returns the issue_id of every parent-child row in
// depTable whose target is one of parentIDs, batching the IN clause so a wide
// BFS level cannot build an unbounded statement.
func parentChildSourcesInTx(ctx context.Context, tx DBTX, depTable string, parentIDs []string) ([]string, error) {
	var out []string
	for start := 0; start < len(parentIDs); start += queryBatchSize {
		batch := parentIDs[start:min(start+queryBatchSize, len(parentIDs))]
		inClause, args := buildSQLInClause(batch)
		//nolint:gosec // G201: depTable is a hardcoded constant, inClause is placeholders.
		query := fmt.Sprintf(`
			SELECT issue_id FROM %s
			WHERE type = 'parent-child' AND %s IN (%s)
		`, depTable, DepTargetExpr, inClause)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan descendant: %w", err)
			}
			out = append(out, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("descendant rows: %w", err)
		}
	}
	return out, nil
}

//nolint:gosec // G201: tables are hardcoded
func GetBlockedIssuesInTx(ctx context.Context, tx DBTX, filter types.WorkFilter) ([]*types.BlockedIssue, error) {
	var blockedIDList []string
	blockedSet := make(map[string]bool)
	for _, table := range []string{"issues", "wisps"} {
		//nolint:gosec // G201: table is one of two hardcoded values.
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
			SELECT id FROM %s
			WHERE is_blocked = 1 AND status <> 'closed' AND status <> 'pinned'
		`, table))
		if err != nil {
			if optionalBlockedTable(table) && isTableNotExistError(err) {
				continue
			}
			return nil, fmt.Errorf("read blocked ids from %s: %w", table, err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan blocked id from %s: %w", table, err)
			}
			if !blockedSet[id] {
				blockedSet[id] = true
				blockedIDList = append(blockedIDList, id)
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("blocked id rows from %s: %w", table, err)
		}
	}
	if len(blockedIDList) == 0 {
		return nil, nil
	}

	blockerMap := make(map[string][]string)
	blockingDeps, err := loadBlockingDepsForIssueIDsInTx(ctx, tx, []string{"dependencies", "wisp_dependencies"}, blockedIDList)
	if err != nil {
		return nil, fmt.Errorf("get blocking deps: %w", err)
	}
	if len(blockingDeps) > 0 {
		targetIDs := make([]string, 0, len(blockingDeps))
		seenTargets := make(map[string]bool, len(blockingDeps))
		for _, rec := range blockingDeps {
			if !seenTargets[rec.dependsOnID] {
				seenTargets[rec.dependsOnID] = true
				targetIDs = append(targetIDs, rec.dependsOnID)
			}
		}
		activeTargets, err := loadStatusByIDInTx(ctx, tx, targetIDs)
		if err != nil {
			return nil, fmt.Errorf("blocker target status: %w", err)
		}
		for _, rec := range blockingDeps {
			status, ok := activeTargets[rec.dependsOnID]
			if !ok || status == types.StatusClosed || status == types.StatusPinned {
				continue
			}
			blockerMap[rec.issueID] = append(blockerMap[rec.issueID], rec.dependsOnID)
		}
	}

	var inheritedIDs []string
	for _, id := range blockedIDList {
		if _, ok := blockerMap[id]; !ok {
			inheritedIDs = append(inheritedIDs, id)
		}
	}
	if len(inheritedIDs) > 0 {
		parentMap, err := loadParentIDsForChildrenInTx(ctx, tx, []string{"dependencies", "wisp_dependencies"}, inheritedIDs)
		if err == nil {
			for childID, parentID := range parentMap {
				if _, alreadyHas := blockerMap[childID]; !alreadyHas {
					blockerMap[childID] = []string{parentID}
				}
			}
		}
	}

	displayIDs := make([]string, 0, len(blockerMap))
	for id := range blockerMap {
		displayIDs = append(displayIDs, id)
	}
	issues, err := GetIssuesByIDsInTx(ctx, tx, displayIDs, nil)
	if err != nil {
		return nil, fmt.Errorf("batch-fetch blocked issues: %w", err)
	}
	issueMap := make(map[string]*types.Issue, len(issues))
	for _, issue := range issues {
		issueMap[issue.ID] = issue
	}

	var parentChildSet map[string]bool
	if filter.ParentID != nil {
		parentChildSet = make(map[string]bool)
		parentID := *filter.ParentID
		children, childErr := GetChildrenOfIssuesInTx(ctx, tx, []string{parentID})
		if childErr == nil {
			for _, childID := range children {
				parentChildSet[childID] = true
			}
		}
		for id := range blockerMap {
			if strings.HasPrefix(id, parentID+".") {
				parentChildSet[id] = true
			}
		}
	}

	var results []*types.BlockedIssue
	for id, blockerIDs := range blockerMap {
		if parentChildSet != nil && !parentChildSet[id] {
			continue
		}
		issue, ok := issueMap[id]
		if !ok || issue == nil {
			continue
		}
		results = append(results, &types.BlockedIssue{
			Issue:          *issue,
			BlockedByCount: len(blockerIDs),
			BlockedBy:      blockerIDs,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Issue.Priority != results[j].Issue.Priority {
			return results[i].Issue.Priority < results[j].Issue.Priority
		}
		return results[i].Issue.CreatedAt.After(results[j].Issue.CreatedAt)
	})

	return results, nil
}
