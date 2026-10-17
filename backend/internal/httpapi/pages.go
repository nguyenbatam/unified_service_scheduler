package httpapi

import (
	"strconv"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The keyset of each listing as page-token values (cursor.go), both ways.

func idFilter(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func nameCursorKeys(c store.NameCursor) []string { return []string{c.Name, c.ID.String()} }

func parseNameCursor(k cursorKeys) (store.NameCursor, error) {
	id, err := k.uuid(1)
	return store.NameCursor{Name: k[0], ID: id}, err
}

func appointmentCursorKeys(c store.AppointmentCursor) []string {
	return []string{cursorTime(c.StartAt), c.ID.String()}
}

func parseAppointmentCursor(k cursorKeys) (store.AppointmentCursor, error) {
	start, err := k.time(0)
	if err != nil {
		return store.AppointmentCursor{}, err
	}
	id, err := k.uuid(1)
	return store.AppointmentCursor{StartAt: start, ID: id}, err
}

func serviceTypeCursorKeys(c store.ServiceTypeCursor) []string {
	return []string{strconv.Itoa(c.DurationMinutes), c.Name, c.ID.String()}
}

func parseServiceTypeCursor(k cursorKeys) (store.ServiceTypeCursor, error) {
	minutes, err := k.int64(0)
	if err != nil {
		return store.ServiceTypeCursor{}, err
	}
	id, err := k.uuid(2)
	return store.ServiceTypeCursor{DurationMinutes: int(minutes), Name: k[1], ID: id}, err
}

func vehicleCursorKeys(c store.VehicleCursor) []string {
	return []string{c.Owner, c.Make, c.Model, c.ID.String()}
}

func parseVehicleCursor(k cursorKeys) (store.VehicleCursor, error) {
	id, err := k.uuid(3)
	return store.VehicleCursor{Owner: k[0], Make: k[1], Model: k[2], ID: id}, err
}

func historyCursorKeys(c store.HistoryCursor) []string { return []string{strconv.FormatInt(c.ID, 10)} }

func parseHistoryCursor(k cursorKeys) (store.HistoryCursor, error) {
	id, err := k.int64(0)
	return store.HistoryCursor{ID: id}, err
}

func outboxCursorKeys(c store.OutboxCursor) []string {
	return []string{cursorTime(c.CreatedAt), c.ID.String()}
}

func parseOutboxCursor(k cursorKeys) (store.OutboxCursor, error) {
	at, err := k.time(0)
	if err != nil {
		return store.OutboxCursor{}, err
	}
	id, err := k.uuid(1)
	return store.OutboxCursor{CreatedAt: at, ID: id}, err
}
