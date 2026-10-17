package main

import (
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// Allocation-quality evidence beyond rule conformance: an offline upper bound on the labour
// minutes a day can hold for a given demand. Every booked job occupies one fitting bay and one
// qualified technician for its whole window, inside the opening hours, off lunch, and (for the
// technician) on duty. Ignoring time order — where in the day a job lands, and that a bay and a
// technician must be free at the same moment — leaves two transportation problems: demand classes
// (job mix × waiter) send labour minutes to bays they fit, each bay holding at most its bookable
// minutes; and likewise to technicians they may use, each holding at most its on-duty bookable
// minutes. The maximum flow of each is an upper bound on what any schedule can book, so
// min(bays, technicians) bounds the day; what the run booked must stay at or under it (more would
// mean capacity was overbooked), and booked/bound is how much of the theoretical day the
// allocator realised (fragmentation and the order customers came in cost the rest).

// demandClass is one class of customers: a job mix, waiting or not, and the labour minutes asked.
type demandClass struct {
	mix     jobMix
	waiter  bool
	minutes int
}

// capacityBound computes the bound for classes on day (shop: its bays, technicians and rosters;
// holds are ignored). It returns the bound and both flows.
func capacityBound(shop *oracleShop, w workshop, day booking.Date, classes []demandClass) (bound, byBays, byTechs int, err error) {
	cells, err := dayStarts(w, day, slotMinutes)
	if err != nil {
		return 0, 0, 0, err
	}
	step := slotMinutes
	open := 0
	for _, c := range cells {
		if !c.lunch {
			open += step
		}
	}
	bayCap := make([]int, len(shop.bays))
	for i := range shop.bays {
		bayCap[i] = open
	}
	techCap := make([]int, len(shop.techs))
	for i, t := range shop.techs {
		for _, c := range cells {
			if !c.lunch && t.roster.onDuty(timeWindow{c.start, c.start.Add(slotStep)}, shop.loc) {
				techCap[i] += step
			}
		}
	}
	demand := make([]int, len(classes))
	bayEdge := make([][]bool, len(classes))
	techEdge := make([][]bool, len(classes))
	for k, c := range classes {
		demand[k] = c.minutes
		soft := softUpgrade(c.mix.req, c.waiter, shop.expressMax)
		bayEdge[k] = make([]bool, len(shop.bays))
		for i, b := range shop.bays {
			bayEdge[k][i] = bayFits(c.mix.req, soft, b.bayType)
		}
		techEdge[k] = make([]bool, len(shop.techs))
		for i, t := range shop.techs {
			techEdge[k][i] = qualified(c.mix.req, t.level, t.skills)
		}
	}
	byBays = transportMaxFlow(demand, bayCap, bayEdge)
	byTechs = transportMaxFlow(demand, techCap, techEdge)
	return min(byBays, byTechs), byBays, byTechs, nil
}

// transportMaxFlow is the maximum flow from sources (supply each) to sinks (capacity each) along
// the allowed edges (edge[source][sink]), all edges uncapped: Edmonds–Karp on the small
// bipartite graph source → class → resource → sink.
func transportMaxFlow(supply, capacity []int, edge [][]bool) int {
	n := 2 + len(supply) + len(capacity)
	src, sink := 0, n-1
	classNode := func(k int) int { return 1 + k }
	resNode := func(i int) int { return 1 + len(supply) + i }
	residual := make([][]int, n)
	for i := range residual {
		residual[i] = make([]int, n)
	}
	total := 0
	for k, s := range supply {
		residual[src][classNode(k)] = s
		total += s
	}
	for i, c := range capacity {
		residual[resNode(i)][sink] = c
	}
	for k := range supply {
		for i := range capacity {
			if edge[k][i] {
				residual[classNode(k)][resNode(i)] = total // effectively unbounded
			}
		}
	}
	flow := 0
	for {
		parent := make([]int, n)
		for i := range parent {
			parent[i] = -1
		}
		parent[src] = src
		queue := []int{src}
		for len(queue) > 0 && parent[sink] < 0 {
			u := queue[0]
			queue = queue[1:]
			for v := range n {
				if parent[v] < 0 && residual[u][v] > 0 {
					parent[v] = u
					queue = append(queue, v)
				}
			}
		}
		if parent[sink] < 0 {
			return flow
		}
		push := total + 1
		for v := sink; v != src; v = parent[v] {
			push = min(push, residual[parent[v]][v])
		}
		for v := sink; v != src; v = parent[v] {
			residual[parent[v]][v] -= push
			residual[v][parent[v]] += push
		}
		flow += push
	}
}
