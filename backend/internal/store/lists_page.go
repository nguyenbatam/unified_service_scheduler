package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Keyset pages of the catalog and admin lists (see page.go). Each order is backed by an index
// (001_schema.sql), and each "after" condition repeats the leading key as a plain bound so
// the index can seek to the cursor.

var (
	dealershipsFirstSQL = dealershipSelect + ` ORDER BY name, id LIMIT $1`
	dealershipsAfterSQL = dealershipSelect + ` WHERE name >= $1 AND (name, id) > ($1, $2) ORDER BY name, id LIMIT $3`

	// appointment_history_appointment_idx (appointment_id, id).
	historyPageSQL = `
		SELECT id, version, event_type, actor_id, at, status, start_at, end_at, service_bay_id, technician_id
		FROM appointment_history
		WHERE appointment_id = $1 AND id > $2
		ORDER BY id
		LIMIT $3`

	serviceTypesFirstSQL = serviceTypeSelect + ` ORDER BY duration_minutes, name, id LIMIT $1`
	serviceTypesAfterSQL = serviceTypeSelect + ` WHERE duration_minutes >= $1 AND (duration_minutes, name, id) > ($1, $2, $3)
		ORDER BY duration_minutes, name, id LIMIT $4`
)

// ListDealershipsPage returns one page of dealerships in (name, id) order.
func (s *Store) ListDealershipsPage(ctx context.Context, limit int, after *NameCursor) ([]domain.Dealership, *NameCursor, error) {
	limit = pageLimit(limit)
	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		rows, err = s.pool.Query(ctx, dealershipsFirstSQL, limit+1)
	} else {
		rows, err = s.pool.Query(ctx, dealershipsAfterSQL, after.Name, after.ID, limit+1)
	}
	if err != nil {
		return nil, nil, err
	}
	items, err := collect(rows, scanDealership)
	if err != nil {
		return nil, nil, err
	}
	items, next := cutPage(items, limit, func(d domain.Dealership) NameCursor { return NameCursor{Name: d.Name, ID: d.ID} })
	return items, next, nil
}

// ListServiceTypesPage returns one page of the service catalog in (duration, name, id) order.
func (s *Store) ListServiceTypesPage(ctx context.Context, limit int, after *ServiceTypeCursor) ([]domain.ServiceType, *ServiceTypeCursor, error) {
	limit = pageLimit(limit)
	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		rows, err = s.pool.Query(ctx, serviceTypesFirstSQL, limit+1)
	} else {
		rows, err = s.pool.Query(ctx, serviceTypesAfterSQL, after.DurationMinutes, after.Name, after.ID, limit+1)
	}
	if err != nil {
		return nil, nil, err
	}
	items, err := collect(rows, scanServiceType)
	if err != nil {
		return nil, nil, err
	}
	items, next := cutPage(items, limit, func(t domain.ServiceType) ServiceTypeCursor {
		return ServiceTypeCursor{DurationMinutes: t.DurationMinutes, Name: t.Name, ID: t.ID}
	})
	return items, next, nil
}

// technicianAdminPageSQL is one page of technicians, every one or one dealership's, in (name, id)
// order. The dealership filter is spelled out (not `$n IS NULL OR …`) so a generic plan can use
// technicians_dealership_name_idx.
func technicianAdminPageSQL(dealershipID uuid.UUID, limit int, after *NameCursor) (string, []any) {
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if dealershipID != uuid.Nil {
		conds = append(conds, `t.dealership_id = `+arg(dealershipID))
	}
	if after != nil {
		name, id := arg(after.Name), arg(after.ID)
		conds = append(conds, fmt.Sprintf(`t.name >= %s AND (t.name, t.id) > (%s, %s)`, name, name, id))
	}
	sql := technicianAdminSelect
	if len(conds) > 0 {
		sql += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	return sql + ` GROUP BY t.id ORDER BY t.name, t.id LIMIT ` + arg(pageLimit(limit)+1), args
}

// ListTechniciansAdmin returns one page of technicians, every one or one dealership's (uuid.Nil =
// all), in (name, id) order.
func (s *Store) ListTechniciansAdmin(ctx context.Context, dealershipID uuid.UUID, limit int, after *NameCursor) ([]domain.TechnicianAdmin, *NameCursor, error) {
	sql, args := technicianAdminPageSQL(dealershipID, limit, after)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	items, err := collect(rows, scanTechnicianAdmin)
	if err != nil {
		return nil, nil, err
	}
	items, next := cutPage(items, pageLimit(limit), func(t domain.TechnicianAdmin) NameCursor { return NameCursor{Name: t.Name, ID: t.ID} })
	return items, next, nil
}

// AppointmentHistory returns one page of an appointment's audit trail, oldest first (id order:
// rows are appended as the appointment changes).
func (s *Store) AppointmentHistory(ctx context.Context, id uuid.UUID, limit int, after *HistoryCursor) ([]domain.AppointmentChange, *HistoryCursor, error) {
	limit = pageLimit(limit)
	var afterID int64
	if after != nil {
		afterID = after.ID
	}
	rows, err := s.pool.Query(ctx, historyPageSQL, id, afterID, limit+1)
	if err != nil {
		return nil, nil, err
	}
	type change struct {
		id int64
		domain.AppointmentChange
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (change, error) {
		var c change
		var status, event int16
		if err := row.Scan(&c.id, &c.Version, &event, &c.ActorID, &c.At, &status, &c.StartAt, &c.EndAt, &c.ServiceBayID, &c.TechnicianID); err != nil {
			return c, err
		}
		c.Status, c.EventType = domain.AppointmentStatus(status), domain.EventType(event)
		return c, nil
	})
	if err != nil {
		return nil, nil, err
	}
	page, next := cutPage(items, limit, func(c change) HistoryCursor { return HistoryCursor{ID: c.id} })
	out := make([]domain.AppointmentChange, len(page))
	for i, c := range page {
		out[i] = c.AppointmentChange
	}
	return out, next, nil
}
