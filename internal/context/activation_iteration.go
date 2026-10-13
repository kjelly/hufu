package context

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// IterateAgentActivations reads canonical items and their typed activation
// projection in one cursor. It narrows ancestor visibility to the requested
// agent: shared records are excluded. Like Iterate, the visitor must not
// re-enter the repository. Missing projections fail closed.
func (r *SQLiteRepository) IterateAgentActivations(ctx context.Context, query RepositoryQuery, visit func(ContextItem, ActivationRecord) error) (returnErr error) {
	if query.Limit != 0 || query.Scope.AgentID == "" || visit == nil {
		return errors.New("agent activation iteration requires an agent, zero limit and visitor")
	}
	where, args, err := compileRepositoryPredicates(query, EvaluationTime(ctx).UnixMilli())
	if err != nil {
		return err
	}
	where = append(where, "agent_id=?")
	args = append(args, query.Scope.AgentID)
	columns := "c." + strings.ReplaceAll(itemColumns, ",", ",c.")
	rows, err := r.db.QueryContext(ctx, "SELECT "+columns+`,a.context_item_id,a.phases,a.triggers,a.roles,a.capabilities,a.tools,a.error_classes,a.environment
		FROM (SELECT * FROM context_items WHERE `+strings.Join(where, " AND ")+`) c
		LEFT JOIN context_activation a ON a.context_item_id=c.id`, args...)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, rows.Close()) }()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var activation ActivationRecord
		item, err := scanItem(&scanWithActivation{rows: rows, record: &activation})
		if err != nil {
			return err
		}
		if err := visit(item, activation); err != nil {
			return err
		}
	}
	return rows.Err()
}

type scanWithActivation struct {
	rows   *sql.Rows
	record *ActivationRecord
}

func (s *scanWithActivation) Scan(destinations ...any) error {
	var id, phases, triggers, roles, capabilities, tools, errorClasses, environment sql.NullString
	destinations = append(destinations, &id, &phases, &triggers, &roles, &capabilities, &tools, &errorClasses, &environment)
	if err := s.rows.Scan(destinations...); err != nil {
		return err
	}
	if !id.Valid {
		return sql.ErrNoRows
	}
	*s.record = ActivationRecord{ContextItemID: id.String, Phases: phases.String, Triggers: triggers.String, Roles: roles.String, Capabilities: capabilities.String, Tools: tools.String, ErrorClasses: errorClasses.String, Environment: environment.String}
	return nil
}
