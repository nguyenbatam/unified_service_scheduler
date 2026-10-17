import { useState } from "react";
import { AppointmentList } from "./AppointmentList";
import type { Appointment, AppointmentPageQuery, Customer } from "./types";
import { Icon } from "./Icon";

type Scope = "dealership" | "vehicle" | "customer";

type Props = {
  dealershipId: string;
  dealershipName?: string;
  /** The booking form's car, to list its bookings (any dealership). */
  vehicle?: { id: string; label: string };
  /** The customer last found or recorded at the walk-in desk. */
  customer: Customer | null;
  timezoneOf: (dealershipId: string) => string;
  selectedId?: string;
  onSelect: (item: Appointment) => void;
  reloadKey?: unknown;
};

/** The list query of a scope (the API's dealershipId / vehicleId / customerId filters). */
export function scopeQuery(
  scope: Scope,
  p: { dealershipId: string; vehicleId?: string; customerId?: string },
): Omit<AppointmentPageQuery, "cursor" | "limit"> {
  if (scope === "vehicle" && p.vehicleId) return { vehicleId: p.vehicleId };
  if (scope === "customer" && p.customerId) return { customerId: p.customerId };
  return { dealershipId: p.dealershipId };
}

/**
 * Staff appointment list: the dealership's by default, or one car's or one customer's (a walk-in
 * just looked up), so the desk can see what someone already has booked before booking again.
 */
export function StaffAppointments({ dealershipId, dealershipName, vehicle, customer, timezoneOf, selectedId, onSelect, reloadKey }: Props) {
  const [chosen, setChosen] = useState<Scope>("dealership");
  // A customer looked up at the desk switches the list to them; picking another scope switches back.
  const [seenCustomer, setSeenCustomer] = useState(customer?.id);
  if (seenCustomer !== customer?.id) {
    setSeenCustomer(customer?.id);
    setChosen(customer ? "customer" : "dealership");
  }
  const scope: Scope = (chosen === "vehicle" && !vehicle) || (chosen === "customer" && !customer) ? "dealership" : chosen;
  const query = scopeQuery(scope, { dealershipId, vehicleId: vehicle?.id, customerId: customer?.id });

  const toolbar = (
    <label className="list-scope">
      <span>
        <Icon name="list" />
        Show
      </span>
      <select value={scope} onChange={(e) => setChosen(e.target.value as Scope)}>
        <option value="dealership">All at {dealershipName ?? "this dealership"}</option>
        {vehicle ? <option value="vehicle">This car: {vehicle.label}</option> : null}
        {customer ? <option value="customer">Customer: {customer.name}</option> : null}
      </select>
    </label>
  );

  return (
    <AppointmentList
      title="Appointments"
      query={query}
      toolbar={toolbar}
      showOwner
      timezoneOf={timezoneOf}
      selectedId={selectedId}
      onSelect={onSelect}
      reloadKey={reloadKey}
      emptyText={scope === "dealership" ? "No appointments at this dealership yet." : "No appointments for them yet."}
    />
  );
}
