package booking

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// GetAppointment hides other customers' appointments behind 404 (no existence leak).
func (s *Service) GetAppointment(ctx context.Context, actor domain.Actor, id uuid.UUID) (domain.Appointment, error) {
	item, err := s.store.GetAppointment(ctx, id)
	if err != nil {
		return domain.Appointment{}, err
	}
	if !actor.CanAccessAppointment(item) {
		return domain.Appointment{}, notFoundAppointment()
	}
	return item, nil
}

type AppointmentQuery struct {
	DealershipID uuid.UUID
	CustomerID   uuid.UUID
	VehicleID    uuid.UUID
	Limit        int
	// When lists upcoming (confirmed and not yet ended, or on site — checked in or in progress —
	// whatever end_at: soonest first), past (the rest: newest first), or every appointment (the
	// zero value: newest first).
	When store.AppointmentWhen
	// After continues ListAppointmentsPage from a previous page's cursor; nil = first page.
	After *store.AppointmentCursor
}

// ListAppointmentsPage lists appointments with keyset pagination in q.When's order (ties broken by
// id; "now" for upcoming vs past is the service clock): next is the cursor of the following page,
// nil on the last one. Customers always get only their own;
// staff may filter by dealership, customer, or vehicle, and dealership-scoped staff only ever see
// their dealership.
func (s *Service) ListAppointmentsPage(ctx context.Context, actor domain.Actor, q AppointmentQuery) ([]domain.Appointment, *store.AppointmentCursor, error) {
	filter, none, err := appointmentFilter(actor, q)
	if err != nil || none {
		return []domain.Appointment{}, nil, err
	}
	filter.When, filter.Now = q.When, s.now()
	return s.store.ListAppointmentsPage(ctx, filter, q.After)
}

// appointmentFilter applies the caller's scope to q; none reports a caller who can see nothing
// (a customer user without a customer record).
func appointmentFilter(actor domain.Actor, q AppointmentQuery) (filter store.AppointmentFilter, none bool, err error) {
	filter = store.AppointmentFilter{DealershipID: q.DealershipID, CustomerID: q.CustomerID, VehicleID: q.VehicleID, Limit: q.Limit}
	if actor.IsStaff() && actor.DealershipID != uuid.Nil {
		if q.DealershipID != uuid.Nil && q.DealershipID != actor.DealershipID {
			return store.AppointmentFilter{}, false, otherDealership()
		}
		filter.DealershipID = actor.DealershipID
	}
	if !actor.IsStaff() {
		filter.CustomerID = actor.CustomerID
		if actor.CustomerID == uuid.Nil {
			return store.AppointmentFilter{}, true, nil
		}
	}
	return filter, false, nil
}

// Vehicle list bounds: the list is a search page, never the whole group.
const (
	DefaultVehicleLimit = 50
	MaxVehicleLimit     = 200
	MaxVehicleSearch    = 100 // characters
)

type VehicleQuery struct {
	// Search is a literal, case-insensitive fragment of the VIN, make, model, or owner's name.
	Search string
	Limit  int // 0 = DefaultVehicleLimit
	// After continues from a previous page's cursor; nil = first page.
	After *store.VehicleCursor
}

// ListVehicles returns one page ordered by owner, make, model, id (next: the following page's
// cursor, nil on the last); customers only ever get their own.
func (s *Service) ListVehicles(ctx context.Context, actor domain.Actor, q VehicleQuery) ([]domain.Vehicle, *store.VehicleCursor, error) {
	search := strings.TrimSpace(q.Search)
	if !utf8.ValidString(search) || strings.ContainsRune(search, 0) || utf8.RuneCountInString(search) > MaxVehicleSearch {
		return nil, nil, domain.NewError(domain.CodeInvalid, fmt.Sprintf("q must be text of at most %d characters", MaxVehicleSearch))
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultVehicleLimit
	}
	if limit < 1 || limit > MaxVehicleLimit {
		return nil, nil, domain.NewError(domain.CodeInvalid, fmt.Sprintf("limit must be 1–%d", MaxVehicleLimit))
	}
	filter := store.VehicleFilter{Search: search, Limit: limit, After: q.After}
	if !actor.IsStaff() {
		if actor.CustomerID == uuid.Nil {
			return []domain.Vehicle{}, nil, nil
		}
		filter.CustomerID = actor.CustomerID
	}
	return s.store.ListVehicles(ctx, filter)
}

// GetVehicle hides other customers' vehicles behind 404 (no existence leak); staff see any.
func (s *Service) GetVehicle(ctx context.Context, actor domain.Actor, id uuid.UUID) (domain.Vehicle, error) {
	item, err := s.store.GetVehicle(ctx, id)
	if err != nil {
		return domain.Vehicle{}, err
	}
	if !actor.CanAccessCustomer(item.CustomerID) {
		return domain.Vehicle{}, notFoundVehicle()
	}
	return item, nil
}

type VehicleInput struct {
	CustomerID uuid.UUID // staff only; customers always register for themselves
	VIN        string
	Make       string
	Model      string
	Year       int
}

func (s *Service) CreateVehicle(ctx context.Context, actor domain.Actor, in VehicleInput) (domain.Vehicle, error) {
	owner := in.CustomerID
	if !actor.IsStaff() {
		owner = actor.CustomerID
	}
	if owner == uuid.Nil {
		return domain.Vehicle{}, domain.NewError(domain.CodeInvalid, "customerId is required")
	}
	vin := strings.ToUpper(strings.TrimSpace(in.VIN))
	if !ValidVIN(vin) {
		return domain.Vehicle{}, domain.NewError(domain.CodeInvalid, "vin must be 17 characters (A–Z except I, O, Q, and 0–9)")
	}
	make, model := strings.TrimSpace(in.Make), strings.TrimSpace(in.Model)
	if !validVehicleName(make) || !validVehicleName(model) {
		return domain.Vehicle{}, domain.NewError(domain.CodeInvalid, fmt.Sprintf("make and model are required (max %d characters)", MaxVehicleName))
	}
	if in.Year < 1980 || in.Year > s.now().Year()+1 {
		return domain.Vehicle{}, domain.NewError(domain.CodeInvalid, "year is out of range")
	}
	item, err := s.store.CreateVehicle(ctx, domain.Vehicle{
		ID: uuid.New(), CustomerID: owner, VIN: vin, Make: make, Model: model, Year: in.Year,
	})
	if domainErr, ok := domain.AsError(err); ok && domainErr.Code == domain.CodeConflict && !actor.IsStaff() {
		// VINs are globally unique, so the precise message would let any customer probe whether a
		// VIN is registered (to anyone). Staff keep it; proving ownership is a dealership task.
		return domain.Vehicle{}, domain.NewError(domain.CodeConflict, VehicleNotRegistrableMessage)
	}
	return item, err
}

// MaxVehicleName bounds a vehicle's make and model, in characters (Unicode code points, as the
// web form counts them), not bytes: "Škoda" is 5.
const MaxVehicleName = 60

// validVehicleName reports whether a trimmed make or model is non-empty text of at most
// MaxVehicleName characters (valid UTF-8 without NUL, which Postgres text refuses).
func validVehicleName(name string) bool {
	return name != "" && utf8.ValidString(name) && !strings.ContainsRune(name, 0) && utf8.RuneCountInString(name) <= MaxVehicleName
}

// VehicleNotRegistrableMessage is what a customer sees when a VIN cannot be added (already
// registered, to them or anyone else); it deliberately does not say which.
const VehicleNotRegistrableMessage = "this vehicle cannot be registered online; please contact the dealership"

// ValidVIN checks ISO 3779 shape: 17 chars, digits and capitals excluding I, O, Q.
func ValidVIN(vin string) bool {
	if len(vin) != 17 {
		return false
	}
	for _, r := range vin {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'A' && r <= 'Z' && r != 'I' && r != 'O' && r != 'Q':
		default:
			return false
		}
	}
	return true
}
