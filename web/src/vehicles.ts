import type { Vehicle, VehicleQuery } from "./types";

/** Staff search one page at a time; a customer's own garage is small, so it comes whole. */
export const STAFF_VEHICLE_LIMIT = 50;
export const CUSTOMER_VEHICLE_LIMIT = 200; // the API maximum
export const VEHICLE_SEARCH_MAX = 100; // characters, as the API enforces
export const VEHICLE_SEARCH_DEBOUNCE_MS = 250;

export function vehicleParams(q: VehicleQuery): URLSearchParams {
  const params = new URLSearchParams();
  const text = q.q?.trim();
  if (text) params.set("q", text);
  if (q.limit) params.set("limit", String(q.limit));
  if (q.cursor) params.set("cursor", q.cursor);
  return params;
}

/**
 * The select's options: the page, with the selected vehicle first when the page does not hold it
 * (a focused appointment's car, or one outside the current search), so the select can show it.
 */
export function vehicleOptions(page: Vehicle[], selected: Vehicle | null, selectedId: string): Vehicle[] {
  if (!selectedId || page.some((v) => v.id === selectedId)) return page;
  return selected?.id === selectedId ? [selected, ...page] : page;
}

/** Adds v to the list, replacing an entry with the same id (a vehicle just created). */
export function upsertVehicle(list: Vehicle[], v: Vehicle): Vehicle[] {
  return list.some((x) => x.id === v.id) ? list.map((x) => (x.id === v.id ? v : x)) : [...list, v];
}

/** VIN as the API accepts it: 17 characters, ISO 3779 (no I, O or Q). */
export function vinProblem(vin: string): string {
  const v = vin.trim().toUpperCase();
  if (v.length !== 17) return "A VIN has exactly 17 characters.";
  if (!/^[A-HJ-NPR-Z0-9]{17}$/.test(v)) return "A VIN uses only letters and digits, never I, O or Q.";
  return "";
}

/** Oldest model year the API accepts; the newest is next year's. */
export const MIN_VEHICLE_YEAR = 1980;
/** Make and model length limit of the API, in characters (Unicode code points, as the API counts). */
export const VEHICLE_NAME_MAX = 60;

/** Length in Unicode code points ("車" and "😀" are one each), not UTF-16 units like String.length. */
export function charCount(text: string): number {
  return Array.from(text).length;
}

export type VehicleDraft = { vin: string; make: string; model: string; year: number };

/**
 * The first problem of a car to register, as the API would refuse it ("" = valid): VIN, make and
 * model (1–60 characters), and a model year from 1980 to next year.
 */
export function vehicleProblem(v: VehicleDraft, thisYear = new Date().getFullYear()): string {
  const vin = vinProblem(v.vin);
  if (vin) return vin;
  if (!v.make.trim()) return "Enter the make (e.g. Toyota).";
  if (!v.model.trim()) return "Enter the model (e.g. Corolla).";
  if (charCount(v.make.trim()) > VEHICLE_NAME_MAX || charCount(v.model.trim()) > VEHICLE_NAME_MAX) {
    return `Make and model have at most ${VEHICLE_NAME_MAX} characters.`;
  }
  if (!Number.isInteger(v.year) || v.year < MIN_VEHICLE_YEAR || v.year > thisYear + 1) {
    return `The model year must be between ${MIN_VEHICLE_YEAR} and ${thisYear + 1}.`;
  }
  return "";
}
