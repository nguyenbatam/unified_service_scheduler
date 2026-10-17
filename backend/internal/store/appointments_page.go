package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// AppointmentWhen picks which appointments a page lists and in which order.
type AppointmentWhen int8

const (
	// WhenAll lists every appointment, newest first (start_at DESC, id DESC).
	WhenAll AppointmentWhen = iota
	// WhenUpcoming lists the appointments still to come or on site, soonest first (start_at ASC,
	// id ASC): confirmed (status 1) and not yet ended, or checked in or in progress (2|3) whatever
	// their end_at (the car is on site and the job holds its bay and technician; an overrun may be
	// flagged without being extended).
	WhenUpcoming
	// WhenPast lists every other appointment, its exact complement (a confirmed one that ended,
	// completed, cancelled, no-show), newest first (start_at DESC, id DESC).
	WhenPast
)

// ascending reports whether pages of w run oldest first.
func (w AppointmentWhen) ascending() bool { return w == WhenUpcoming }

// AppointmentCursor is the last row of a page in ListAppointmentsPage's order; the next page
// starts strictly after it. The order depends on the filter's When, so a cursor is only valid
// with the When it was read with (httpapi encodes it into the token).
type AppointmentCursor struct {
	StartAt time.Time
	ID      uuid.UUID
}

// ListAppointmentsPage returns one page of the appointments matching the filter with keyset
// pagination in f.When's order, ties on start_at broken by id, so rows are neither repeated nor
// skipped across pages even when many share a start time. after == nil reads the first page. next
// is the cursor of the following page, or nil on the last one.
func (s *Store) ListAppointmentsPage(ctx context.Context, f AppointmentFilter, after *AppointmentCursor) (items []domain.Appointment, next *AppointmentCursor, err error) {
	limit := pageLimit(f.Limit)
	sql, args := appointmentsPageSQL(f, after)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items, err = collect(rows, scanAppointment)
	if err != nil {
		return nil, nil, err
	}
	items, next = cutPage(items, limit, func(a domain.Appointment) AppointmentCursor {
		return AppointmentCursor{StartAt: a.StartAt, ID: a.ID}
	})
	return items, next, nil
}

// appointmentsPageSQL builds ListAppointmentsPage's query (one row past the page, which tells
// whether another page exists). The WHERE clause names only the filters that are set (still
// parameterized): with `$n IS NULL OR col = $n` a cached generic plan cannot use
// (customer_id|vehicle_id|dealership_id, start_at) and walks the whole history instead.
//
// Upcoming is two disjoint branches, each read in the page's order up to the page and merged:
// confirmed rows end after now and no appointment spans more than 24h, so start_at > now - 24h is
// a redundant bound that lets the (scope, start_at) indexes seek to the start of the range; the
// rows on site (2|3) have no such bound and come from the partial on-site indexes (001_schema.sql),
// which hold only them. Past is the exact complement of upcoming.
func appointmentsPageSQL(f AppointmentFilter, after *AppointmentCursor) (string, []any) {
	var conds []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	for _, filter := range []struct {
		column string
		id     uuid.UUID
	}{{"a.customer_id", f.CustomerID}, {"a.vehicle_id", f.VehicleID}, {"a.dealership_id", f.DealershipID}} {
		if filter.id != uuid.Nil {
			conds = append(conds, filter.column+" = "+arg(filter.id))
		}
	}
	if after != nil {
		start, id := arg(after.StartAt), arg(after.ID)
		// The redundant start_at bound lets the (filter, start_at) indexes seek to the cursor.
		if f.When.ascending() {
			conds = append(conds, fmt.Sprintf("a.start_at >= %s AND (a.start_at, a.id) > (%s, %s)", start, start, id))
		} else {
			conds = append(conds, fmt.Sprintf("a.start_at <= %s AND (a.start_at, a.id) < (%s, %s)", start, start, id))
		}
	}
	limit := arg(pageLimit(f.Limit) + 1)
	where := func(extra ...string) string {
		all := append(append([]string(nil), conds...), extra...)
		if len(all) == 0 {
			return ""
		}
		return ` WHERE ` + strings.Join(all, ` AND `)
	}
	switch f.When {
	case WhenUpcoming:
		now := arg(f.Now)
		confirmed := where("a.status = "+statusConfirmedSQL, fmt.Sprintf("a.end_at > %s::timestamptz", now),
			fmt.Sprintf("a.start_at > %s::timestamptz", arg(f.Now.Add(-MaxAppointmentSpan))))
		onSite := where("a.status IN (" + onSiteSQL + ")")
		branch := func(w string) string {
			return `(` + appointmentSelect + w + ` ORDER BY a.start_at, a.id LIMIT ` + limit + `)`
		}
		// start_at and id name one output column each (appointmentColumns), as a UNION's ORDER BY needs.
		return branch(confirmed) + ` UNION ALL ` + branch(onSite) + ` ORDER BY start_at, id LIMIT ` + limit, args
	case WhenPast:
		return appointmentSelect + where(fmt.Sprintf("NOT (a.status IN (%s) OR (a.status = %s AND a.end_at > %s::timestamptz))", onSiteSQL, statusConfirmedSQL, arg(f.Now))) +
			` ORDER BY a.start_at DESC, a.id DESC LIMIT ` + limit, args
	default:
		return appointmentSelect + where() + ` ORDER BY a.start_at DESC, a.id DESC LIMIT ` + limit, args
	}
}
