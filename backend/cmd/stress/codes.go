package main

import (
	"strconv"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// SMALLINT enum codes in the oracle's SQL, derived from internal/domain (the schema's codes,
// defined once). Codes are schema facts, not rules: taking them from the engine's package keeps
// the oracle's restated rules (rules.go, repair.go) independent.
var (
	outboxPublishedSQL = sqlCode(domain.OutboxPublished)
	outboxFailedSQL    = sqlCode(domain.OutboxFailed)

	statusInProgressSQL = sqlCode(domain.StatusInProgress)
	statusCompletedSQL  = sqlCode(domain.StatusCompleted)
	// holdingSQL: the statuses that hold a bay, a technician and a vehicle (the GiST predicates).
	holdingSQL = sqlCode(domain.StatusConfirmed) + ", " + sqlCode(domain.StatusCheckedIn) + ", " + statusInProgressSQL
	// notStartedSQL: confirmed or checked in.
	notStartedSQL = sqlCode(domain.StatusConfirmed) + ", " + sqlCode(domain.StatusCheckedIn)

	bayExpressSQL    = sqlCode(domain.BayExpress)
	bayGeneralSQL    = sqlCode(domain.BayGeneral)
	bayDiagnosticSQL = sqlCode(domain.BayDiagnostic)
	bayAlignmentSQL  = sqlCode(domain.BayAlignment)
)

// sqlCode is v as a SQL integer literal.
func sqlCode[T ~int16](v T) string { return strconv.Itoa(int(v)) }
