package store

import (
	"time"

	"github.com/google/uuid"
)

// Keyset pagination: every list reads Limit+1 rows of a stable, unique order (the id always ends
// it as the tie-break) strictly after the previous page's last row, so a row inserted or deleted
// between two pages never shifts another one onto the wrong page (no duplicate, no skip), unlike
// OFFSET. The extra row only tells whether another page follows.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// pageLimit is limit within 1–MaxPageLimit, else DefaultPageLimit.
func pageLimit(limit int) int {
	if limit <= 0 || limit > MaxPageLimit {
		return DefaultPageLimit
	}
	return limit
}

// cutPage trims rows (read with LIMIT limit+1) to the page and returns the cursor of its last row
// when another page follows (nil on the last page).
func cutPage[T, C any](rows []T, limit int, key func(T) C) ([]T, *C) {
	if len(rows) <= limit {
		return rows, nil
	}
	rows = rows[:limit]
	next := key(rows[limit-1])
	return rows, &next
}

// NameCursor continues a listing ordered by (name, id): dealerships, customers, technicians.
type NameCursor struct {
	Name string
	ID   uuid.UUID
}

// ServiceTypeCursor continues the catalog's (duration_minutes, name, id) order.
type ServiceTypeCursor struct {
	DurationMinutes int
	Name            string
	ID              uuid.UUID
}

// VehicleCursor continues the vehicles' (owner name, make, model, id) order.
type VehicleCursor struct {
	Owner, Make, Model string
	ID                 uuid.UUID
}

// HistoryCursor continues an appointment's audit trail (appointment_history.id, insertion order).
type HistoryCursor struct {
	ID int64
}

// OutboxCursor continues the failed outbox rows' (created_at, id) order.
type OutboxCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}
