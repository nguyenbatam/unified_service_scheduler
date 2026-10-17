import { ServicePicker } from "./ServicePicker";
import { TruncatedHint } from "./TruncatedHint";
import type { Catalog } from "./api";
import { NextSlotList } from "./SlotGrid";
import { VehiclePicker } from "./VehiclePicker";
import { firstBookableDay, lastBookableDay } from "./booking";
import { formatMinutes, zoneLabel } from "./time";
import type { BookingForm } from "./useBookingForm";
import type { VehicleList } from "./useVehicleList";
import type { Dealership, ServiceType } from "./types";
import { Icon } from "./Icon";

type Props = {
  form: BookingForm;
  dealerships: Dealership[];
  services: ServiceType[];
  vehicleList: VehicleList;
  isStaff: boolean;
  busy: boolean;
  /** A booking or move is in flight: the inputs it was sent for stay as they are until it settles. */
  writing?: boolean;
  /** The catalog lists that stopped at FETCH_ALL_MAX (their dropdowns say so). */
  truncated?: Catalog["truncated"];
};

/** Step 1 of the booking flow: dealership → vehicle → job lines → date, then the slot search. */
export function BookingRequest({ form, dealerships, services, vehicleList, isStaff, busy, writing = false, truncated }: Props) {
  const { dealership } = form;
  // Rescheduling moves one appointment: its dealership, car, and job lines are fixed (only the day,
  // the time, and the waiter flag can change), so the move can never turn into another booking.
  // While a write is in flight nothing changes: its refresh then re-reads the grid it was sent for.
  const locked = form.rescheduleTarget !== null || writing;
  return (
    <section className="panel">
      <h2>
        <Icon name="clipboard" />
        1. Booking request
      </h2>
      <div className="grid">
        {isStaff ? (
          // Staff pick the dealership once, in the workshop bar above the board.
          <div className="field-static">
            <span className="field-label">
              <Icon name="dealership" />
              Dealership
            </span>
            <span>{dealership?.name ?? "—"}</span>
          </div>
        ) : (
          <label>
            <span>
              <Icon name="dealership" />
              Dealership
            </span>
            <select value={form.dealershipId} disabled={locked} onChange={(e) => form.changeDealership(e.target.value)}>
              {dealerships.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name} ({d.openTime}–{d.closeTime})
                </option>
              ))}
            </select>
          </label>
        )}
        {isStaff ? null : <TruncatedHint shown={truncated?.dealerships ?? false} what="dealerships" />}
        <VehiclePicker isStaff={isStaff} list={vehicleList} value={form.vehicleId} onChange={form.changeVehicle} disabled={locked} />
        <label>
          <span>
            <Icon name="calendar" />
            Date
          </span>
          <input
            type="date"
            value={form.date}
            // Customers book online between the lead time and the booking horizon; staff are not bound.
            min={isStaff || !dealership ? undefined : firstBookableDay(dealership)}
            max={isStaff || !dealership ? undefined : lastBookableDay(dealership)}
            disabled={writing}
            onChange={(e) => form.changeDate(e.target.value)}
          />
        </label>
        <label className="checkbox">
          <input type="checkbox" checked={form.isWaiter} disabled={writing} onChange={(e) => form.changeWaiter(e.target.checked)} />
          <Icon name="armchair" />
          {isStaff ? "Waiter (customer waits on site — express bay preferred)" : "I'll wait at the dealership while it's done"}
        </label>
      </div>
      <ServicePicker
        services={services}
        selected={form.serviceTypeIds}
        isWaiter={form.isWaiter}
        isStaff={isStaff}
        onChange={form.changeServices}
        disabled={locked}
      />
      <TruncatedHint shown={truncated?.services ?? false} what="services" />
      {form.rescheduleTarget !== null ? (
        <p className="hint input-lock-hint">
          <Icon name="lock" />
          Rescheduling: the dealership, car, and services stay as booked — stop rescheduling to change them.
        </p>
      ) : null}
      <div className="actions">
        <button type="button" onClick={() => void form.loadAvailability()} disabled={busy || !form.ready}>
          <Icon name="search" />
          {form.slotsLoading ? "Checking…" : "Check availability"}
        </button>
        <button type="button" onClick={() => void form.findFirstAvailable()} disabled={busy || !form.ready}>
          <Icon name="zap" />
          {isStaff ? "Next available" : "Suggest times"}
        </button>
      </div>
      {dealership ? (
        <p className="hint">
          <Icon name="clock" />
          Hours {dealership.openTime}–{dealership.closeTime} ({zoneLabel(dealership.timezone)})
          {dealership.lunchStart ? ` · lunch ${dealership.lunchStart}–${dealership.lunchEnd}` : ""}
          {dealership.stagingCapacity > 0
            ? ` · check-in max ${dealership.stagingCapacity} cars / ${dealership.stagingWindowMinutes} min`
            : ""}
          {!isStaff
            ? ` · online bookings need ${formatMinutes(dealership.policy.customerLeadMinutes)} notice, up to ${dealership.policy.bookingHorizonDays} days ahead`
            : ""}
        </p>
      ) : null}
      <NextSlotList
        next={form.nextSlots}
        busy={busy}
        showCapacity={isStaff}
        selectedSlot={form.selectedSlot}
        onPick={(slot) => void form.pickNextSlot(slot)}
      />
    </section>
  );
}
