import type { VehicleList } from "./useVehicleList";
import { VEHICLE_SEARCH_MAX } from "./vehicles";
import { Icon } from "./Icon";

type Props = {
  isStaff: boolean;
  list: VehicleList;
  value: string;
  onChange: (vehicleId: string) => void;
  /** Locked while rescheduling (the appointment's car). */
  disabled?: boolean;
};

/** Vehicle select; staff narrow the group's vehicles with a search box above it. */
export function VehiclePicker({ isStaff, list, value, onChange, disabled = false }: Props) {
  // The chosen vehicle may not be on this page yet (being fetched by id, or the fetch failed): keep
  // an option for it so the select never shows a different car than the one that will be booked.
  const unlisted = list.loaded && value !== "" && !list.options.some((v) => v.id === value);
  return (
    <div className="vehicle-picker">
      <span className="field-label">
        <Icon name="car" />
        Vehicle
      </span>
      {isStaff ? (
        <input
          type="search"
          aria-label="Search vehicles"
          placeholder="Search VIN, make, model, or customer"
          maxLength={VEHICLE_SEARCH_MAX}
          disabled={disabled}
          value={list.query}
          onChange={(e) => list.setQuery(e.target.value)}
        />
      ) : null}
      <select aria-label="Vehicle" value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        {!list.loaded ? <option value={value}>Loading…</option> : null}
        {/* Nothing is picked by default for staff (or a customer with several cars): say so. */}
        {list.loaded && value === "" && list.options.length > 0 ? (
          <option value="" disabled>
            {isStaff ? "Choose the customer's car…" : "Choose a car…"}
          </option>
        ) : null}
        {unlisted ? <option value={value}>Selected vehicle…</option> : null}
        {list.loaded && list.options.length === 0 && !unlisted ? (
          <option value="">{isStaff ? "No matching vehicles" : "Add a vehicle first"}</option>
        ) : null}
        {list.options.map((v) => (
          <option key={v.id} value={v.id}>
            {v.year} {v.make} {v.model}
            {isStaff ? ` — ${v.customerName}` : ""}
          </option>
        ))}
      </select>
      {list.hasMore ? (
        <button type="button" disabled={disabled || list.loadingMore} onClick={list.more}>
          <Icon name="chevronDown" />
          {list.loadingMore ? "Loading…" : "Show more vehicles"}
        </button>
      ) : null}
    </div>
  );
}
