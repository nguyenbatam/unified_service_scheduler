package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// VehicleFilter narrows ListVehicles to one page.
type VehicleFilter struct {
	CustomerID uuid.UUID // uuid.Nil = every customer's vehicles
	// Search is matched as a literal, case-insensitive substring of the VIN, make, model, or owner's
	// name ("" = no search); LIKE wildcards in it are escaped, never interpreted.
	Search string
	Limit  int // 1–200, else 50
	// After continues from a previous page's cursor (nil = first page).
	After *VehicleCursor
}

const vehicleOrder = ` ORDER BY c.name, v.make, v.model, v.id`

// matchAnySQL is true when any column contains the LIKE pattern in placeholder ph.
func matchAnySQL(ph string, columns ...string) string {
	conds := make([]string, len(columns))
	for i, col := range columns {
		conds[i] = col + ` ILIKE ` + ph + ` ESCAPE '\'`
	}
	return `(` + strings.Join(conds, ` OR `) + `)`
}

// vehiclesPageSQL builds ListVehicles' query: one shape per filter rather than `$1 IS NULL OR …`,
// since a cached generic plan for the latter cannot use the indexes.
//   - group-wide page: walks customers_name_idx in name order and stops at the LIMIT;
//   - a customer's garage is small: vehicles_customer_idx, then filter;
//   - group-wide search: one UNION branch per table so each OR stays on a single table and is a
//     BitmapOr over its pg_trgm GIN indexes (001_schema.sql); one OR across the join could
//     only be checked after joining every vehicle to its customer.
//
// The keyset (owner name, make, model, id) goes into every branch, with the owner name repeated
// as a plain bound so the name index can seek to the cursor.
func vehiclesPageSQL(f VehicleFilter) (string, []any) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var conds []string
	if f.CustomerID != uuid.Nil {
		conds = append(conds, `v.customer_id = `+arg(f.CustomerID))
	}
	pattern := ""
	if f.Search != "" {
		pattern = arg(containsPattern(f.Search))
	}
	if f.After != nil {
		owner, mk, model, id := arg(f.After.Owner), arg(f.After.Make), arg(f.After.Model), arg(f.After.ID)
		conds = append(conds, fmt.Sprintf(`c.name >= %s AND (c.name, v.make, v.model, v.id) > (%s, %s, %s, %s)`, owner, owner, mk, model, id))
	}
	where := func(extra ...string) string {
		all := append(append([]string{}, conds...), extra...)
		if len(all) == 0 {
			return ""
		}
		return ` WHERE ` + strings.Join(all, ` AND `)
	}
	limit := ` LIMIT ` + arg(pageLimit(f.Limit)+1)
	switch {
	case pattern == "":
		return vehicleSelect + where() + vehicleOrder + limit, args
	case f.CustomerID != uuid.Nil:
		return vehicleSelect + where(matchAnySQL(pattern, "v.vin", "v.make", "v.model", "c.name")) + vehicleOrder + limit, args
	default:
		return `SELECT id, customer_id, name, vin, make, model, year FROM (` +
			vehicleSelect + where(matchAnySQL(pattern, "v.vin", "v.make", "v.model")) +
			` UNION ` +
			vehicleSelect + where(matchAnySQL(pattern, "c.name")) +
			`) hit ORDER BY name, make, model, id` + limit, args
	}
}

// ListVehicles returns one page of vehicles, every customer's or only f.CustomerID's, in (owner
// name, make, model, id) order; next is the following page's cursor (nil on the last page).
func (s *Store) ListVehicles(ctx context.Context, f VehicleFilter) ([]domain.Vehicle, *VehicleCursor, error) {
	sql, args := vehiclesPageSQL(f)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	items, err := collect(rows, scanVehicle)
	if err != nil {
		return nil, nil, err
	}
	items, next := cutPage(items, pageLimit(f.Limit), func(v domain.Vehicle) VehicleCursor {
		return VehicleCursor{Owner: v.CustomerName, Make: v.Make, Model: v.Model, ID: v.ID}
	})
	return items, next, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// containsPattern is a LIKE pattern (escape character '\') matching text anywhere, taken literally.
func containsPattern(text string) string {
	return "%" + likeEscaper.Replace(text) + "%"
}
