package booking

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Customer field and list bounds.
const (
	MaxCustomerName   = 120
	MaxCustomerEmail  = 254
	MaxCustomerPhone  = 30
	DefaultCustomers  = 20
	MaxCustomerLimit  = 100
	MaxCustomerSearch = 100 // characters
)

type CustomerInput struct {
	Name, Phone, Email string
}

// CreateCustomer records a walk-in customer (no account) so staff can add their vehicle and book
// for them. Customers are group-wide, like vehicles: a car owner is the group's customer, not one
// dealership's, so any staff member may create and find them (GET /vehicles is group-wide too).
// At least one of phone and email is required, so notifications have somewhere to go.
func (s *Service) CreateCustomer(ctx context.Context, actor domain.Actor, in CustomerInput) (domain.Customer, error) {
	if !actor.IsStaff() {
		return domain.Customer{}, staffOnly()
	}
	c, err := normalizeCustomer(in)
	if err != nil {
		return domain.Customer{}, err
	}
	c.ID = uuid.New()
	return s.store.CreateCustomer(ctx, c)
}

// ListCustomers is one page of the staff customer search (name, phone, or email fragment), in
// (name, id) order after the cursor (nil = first page).
func (s *Service) ListCustomers(ctx context.Context, actor domain.Actor, search string, limit int, after *store.NameCursor) ([]domain.Customer, *store.NameCursor, error) {
	if !actor.IsStaff() {
		return nil, nil, staffOnly()
	}
	search = strings.TrimSpace(search)
	if !utf8.ValidString(search) || strings.ContainsRune(search, 0) || utf8.RuneCountInString(search) > MaxCustomerSearch {
		return nil, nil, domain.NewError(domain.CodeInvalid, fmt.Sprintf("q must be text of at most %d characters", MaxCustomerSearch))
	}
	if limit == 0 {
		limit = DefaultCustomers
	}
	if limit < 1 || limit > MaxCustomerLimit {
		return nil, nil, domain.NewError(domain.CodeInvalid, fmt.Sprintf("limit must be 1–%d", MaxCustomerLimit))
	}
	return s.store.ListCustomers(ctx, search, limit, after)
}

// normalizeCustomer trims and validates a walk-in's details (email lowercased).
func normalizeCustomer(in CustomerInput) (domain.Customer, error) {
	c := domain.Customer{
		Name:  strings.TrimSpace(in.Name),
		Phone: strings.TrimSpace(in.Phone),
		Email: strings.ToLower(strings.TrimSpace(in.Email)),
	}
	switch {
	case c.Name == "" || !utf8.ValidString(c.Name) || utf8.RuneCountInString(c.Name) > MaxCustomerName || strings.ContainsRune(c.Name, 0):
		return domain.Customer{}, domain.NewError(domain.CodeInvalid, fmt.Sprintf("name is required (max %d characters)", MaxCustomerName))
	case c.Phone == "" && c.Email == "":
		return domain.Customer{}, domain.NewError(domain.CodeInvalid, "a phone number or an email is required")
	case c.Email != "" && !validEmail(c.Email):
		return domain.Customer{}, domain.NewError(domain.CodeInvalid, "email is not a valid address")
	case c.Phone != "" && !validPhone(c.Phone):
		return domain.Customer{}, domain.NewError(domain.CodeInvalid,
			fmt.Sprintf("phone must be 6–%d characters: digits, spaces, and + - ( ) .", MaxCustomerPhone))
	}
	return c, nil
}

// validEmail is a shape check only (one @, text on both sides, a dot in the domain, no spaces).
func validEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	if len(e) > MaxCustomerEmail || at < 1 || at != strings.LastIndexByte(e, '@') || strings.ContainsFunc(e, unicode.IsSpace) {
		return false
	}
	host := e[at+1:]
	return strings.Contains(host, ".") && !strings.HasPrefix(host, ".") && !strings.HasSuffix(host, ".")
}

// validPhone accepts the usual punctuation around at least 6 digits.
func validPhone(p string) bool {
	if len(p) < 6 || len(p) > MaxCustomerPhone {
		return false
	}
	digits := 0
	for _, r := range p {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case strings.ContainsRune(" +-().", r):
		default:
			return false
		}
	}
	return digits >= 6
}
