package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/internal/storage/dberrors"
	"github.com/steveyegge/beads/internal/types"
)

func (r *issueSQLRepositoryImpl) GetDescendants(ctx context.Context, rootID string, filter types.IssueFilter) ([]*types.Issue, error) {
	levelFilter := filter
	levelFilter.ParentID = nil
	levelFilter.Limit = 0
	levelFilter.Offset = 0

	issueWhereClauses, issueArgs, err := buildIssueFilterClauses("", levelFilter, issuesFilterTables)
	if err != nil {
		return nil, fmt.Errorf("descendants: issues filter: %w", err)
	}

	wispDepsExist, err := r.optionalTableExists(ctx, "wisp_dependencies")
	if err != nil {
		return nil, fmt.Errorf("descendants: wisp_dependencies probe: %w", err)
	}
	walkWisps := wispDepsExist && !filter.SkipWisps
	if walkWisps {
		empty, probeErr := r.wispsTableEmptyOrMissing(ctx)
		if probeErr != nil {
			return nil, fmt.Errorf("descendants: wisps table probe: %w", probeErr)
		}
		walkWisps = !empty
	}

	var wispWhereClauses []string
	var wispArgs []any
	if walkWisps {
		wispWhereClauses, wispArgs, err = buildIssueFilterClauses("", levelFilter, wispsFilterTables)
		if err != nil {
			return nil, fmt.Errorf("descendants: wisps filter: %w", err)
		}
	}

	// The old implementation used a recursive UNION ALL CTE. It emitted one
	// row for every route to a descendant, so diamonds multiplied work and an
	// imported parent-child cycle recursed until Dolt's engine limit. Keep the
	// traversal in the application, where a visited set makes the cost linear
	// in the reachable graph. This is deliberately the same safety property as
	// issueops.GetDescendantIDsInTx, while retaining this repository's dotted-ID
	// fallback and per-level filter semantics.
	issueMatches, err := r.matchingDescendantIDs(ctx, "issues", issueWhereClauses, issueArgs)
	if err != nil {
		return nil, fmt.Errorf("descendants: match issues: %w", err)
	}
	issueDotted, err := r.dottedDescendantIDs(ctx, "dependencies", issueMatches)
	if err != nil {
		return nil, fmt.Errorf("descendants: dotted issues: %w", err)
	}

	var wispMatches map[string]struct{}
	var wispDotted map[string][]string
	if walkWisps {
		wispMatches, err = r.matchingDescendantIDs(ctx, "wisps", wispWhereClauses, wispArgs)
		if err != nil {
			return nil, fmt.Errorf("descendants: match wisps: %w", err)
		}
		wispDotted, err = r.dottedDescendantIDs(ctx, "wisp_dependencies", wispMatches)
		if err != nil {
			return nil, fmt.Errorf("descendants: dotted wisps: %w", err)
		}
	}

	page, err := r.walkDescendants(ctx, rootID, issueMatches, issueDotted, wispMatches, wispDotted, walkWisps)
	if err != nil {
		return nil, fmt.Errorf("descendants: walk: %w", err)
	}

	issuesByID, err := r.fetchIssuesByIDs(ctx, page.issueIDs, issuesFilterTables, filter)
	if err != nil {
		return nil, fmt.Errorf("descendants: hydrate issues: %w", err)
	}

	var wispsByID map[string]*types.Issue
	if len(page.wispIDs) > 0 {
		wispsByID, err = r.fetchIssuesByIDs(ctx, page.wispIDs, wispsFilterTables, filter)
		if err != nil && !dberrors.IsTableNotExist(err) {
			return nil, fmt.Errorf("descendants: hydrate wisps: %w", err)
		}
	}

	return reassembleBySrc(page.ordered, issuesByID, wispsByID), nil
}

type descendantRef struct {
	id  string
	src string
	via byte // 'e' = parent-child edge, 'd' = dotted-ID fallback
}

func (r *issueSQLRepositoryImpl) matchingDescendantIDs(ctx context.Context, table string, clauses []string, args []any) (map[string]struct{}, error) {
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	//nolint:gosec // G201: table is one of the two hardcoded issue tables.
	rows, err := r.runner.QueryContext(ctx, fmt.Sprintf("SELECT id FROM %s%s", table, where), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan matching id: %w", err)
		}
		ids[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// dottedDescendantIDs builds the dotted-ID fallback index once per call. A
// dotted row without a parent-child edge is a descendant of every dotted
// prefix, matching the old `id LIKE CONCAT(parent, '.%')` behavior without a
// recursive LIKE join.
func (r *issueSQLRepositoryImpl) dottedDescendantIDs(ctx context.Context, depTable string, matches map[string]struct{}) (map[string][]string, error) {
	//nolint:gosec // G201: depTable is one of the two hardcoded dependency tables.
	rows, err := r.runner.QueryContext(ctx, fmt.Sprintf("SELECT issue_id FROM %s WHERE type = 'parent-child'", depTable))
	if err != nil {
		return nil, err
	}
	explicit := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan explicit child: %w", err)
		}
		explicit[id] = struct{}{}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byParent := make(map[string][]string)
	for id := range matches {
		if _, hasExplicitParent := explicit[id]; hasExplicitParent {
			continue
		}
		for parent := dottedParentID(id); parent != ""; parent = dottedParentID(parent) {
			byParent[parent] = append(byParent[parent], id)
		}
	}
	return byParent, nil
}

func dottedParentID(id string) string {
	if at := strings.LastIndexByte(id, '.'); at >= 0 {
		return id[:at]
	}
	return ""
}

func (r *issueSQLRepositoryImpl) walkDescendants(ctx context.Context, rootID string, issueMatches map[string]struct{}, issueDotted map[string][]string, wispMatches map[string]struct{}, wispDotted map[string][]string, walkWisps bool) (idSrcPage, error) {
	seen := map[string]struct{}{rootID: {}}
	frontier := []descendantRef{{id: rootID, via: 'e'}}
	var page idSrcPage

	appendRef := func(ref descendantRef, matches map[string]struct{}, next *[]descendantRef) {
		if _, allowed := matches[ref.id]; !allowed {
			return // A filtered node is not returned and cannot extend the walk.
		}
		if _, duplicate := seen[ref.id]; duplicate {
			return
		}
		seen[ref.id] = struct{}{}
		page.ordered = append(page.ordered, idSrcRef{id: ref.id, src: ref.src})
		switch ref.src {
		case "i":
			page.issueIDs = append(page.issueIDs, ref.id)
		case "w":
			page.wispIDs = append(page.wispIDs, ref.id)
		}
		*next = append(*next, ref)
	}

	for len(frontier) > 0 {
		parentIDs := make([]string, len(frontier))
		for i, parent := range frontier {
			parentIDs[i] = parent.id
		}
		var next []descendantRef

		issueEdges, err := r.parentChildDescendantIDs(ctx, "dependencies", parentIDs)
		if err != nil {
			return idSrcPage{}, fmt.Errorf("issue edge children: %w", err)
		}
		for _, id := range issueEdges {
			appendRef(descendantRef{id: id, src: "i", via: 'e'}, issueMatches, &next)
		}
		for _, parent := range frontier {
			if parent.via != 'e' {
				continue
			}
			for _, id := range issueDotted[parent.id] {
				appendRef(descendantRef{id: id, src: "i", via: 'd'}, issueMatches, &next)
			}
		}

		if walkWisps {
			wispEdges, err := r.parentChildDescendantIDs(ctx, "wisp_dependencies", parentIDs)
			if err != nil {
				return idSrcPage{}, fmt.Errorf("wisp edge children: %w", err)
			}
			for _, id := range wispEdges {
				appendRef(descendantRef{id: id, src: "w", via: 'e'}, wispMatches, &next)
			}
			for _, parent := range frontier {
				if parent.via != 'e' {
					continue
				}
				for _, id := range wispDotted[parent.id] {
					appendRef(descendantRef{id: id, src: "w", via: 'd'}, wispMatches, &next)
				}
			}
		}
		frontier = next
	}
	return page, nil
}

func (r *issueSQLRepositoryImpl) parentChildDescendantIDs(ctx context.Context, depTable string, parentIDs []string) ([]string, error) {
	var out []string
	for start := 0; start < len(parentIDs); start += queryBatchSize {
		end := min(start+queryBatchSize, len(parentIDs))
		placeholders, args := buildInPlaceholders(parentIDs[start:end])
		//nolint:gosec // G201: depTable is one of the two hardcoded dependency tables.
		rows, err := r.runner.QueryContext(ctx, fmt.Sprintf(`
			SELECT issue_id FROM %s
			WHERE type = 'parent-child' AND %s IN (%s)`, depTable, depTargetExpr, placeholders), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan child: %w", err)
			}
			out = append(out, id)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
