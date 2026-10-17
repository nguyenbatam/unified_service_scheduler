import { describe, expect, it } from "vitest";
import type { Vehicle } from "./types";
import { VEHICLE_NAME_MAX, charCount, upsertVehicle, vehicleOptions, vehicleParams, vehicleProblem } from "./vehicles";

function car(id: string, model = "A4"): Vehicle {
  return { id, customerId: "c", customerName: "Noah Patel", vin: `VIN${id}`, make: "Audi", model, year: 2021 };
}

describe("vehicle search params", () => {
  it("trims the search text and omits empty values", () => {
    expect(vehicleParams({ q: "  audi a4 ", limit: 50 }).toString()).toBe("q=audi+a4&limit=50");
    expect(vehicleParams({ q: "   " }).toString()).toBe("");
    expect(vehicleParams({}).toString()).toBe("");
  });
  it("sends the next page's cursor", () => {
    expect(vehicleParams({ q: "a4", limit: 50, cursor: "tok_-1" }).toString()).toBe("q=a4&limit=50&cursor=tok_-1");
  });
  it("encodes wildcards and reserved characters as text", () => {
    expect(vehicleParams({ q: "100%_&x" }).get("q")).toBe("100%_&x");
  });
});

describe("vehicle options", () => {
  const page = [car("a"), car("b")];
  it("is the page when it holds the selection", () => {
    expect(vehicleOptions(page, car("x"), "b")).toBe(page);
    expect(vehicleOptions(page, null, "")).toBe(page);
  });
  it("puts a selected vehicle outside the page first", () => {
    expect(vehicleOptions(page, car("x"), "x").map((v) => v.id)).toEqual(["x", "a", "b"]);
  });
  it("ignores a cached vehicle that is no longer selected", () => {
    expect(vehicleOptions(page, car("x"), "y")).toBe(page);
  });
});

describe("vehicle page", () => {
  it("adds a created vehicle once", () => {
    const list = [car("a")];
    expect(upsertVehicle(list, car("b")).map((v) => v.id)).toEqual(["a", "b"]);
    expect(upsertVehicle(list, car("a", "A6"))).toEqual([car("a", "A6")]);
  });
});

describe("vehicleProblem", () => {
  const ok = { vin: "1hgcm82633a004352", make: "Honda", model: "Accord", year: 2020 };
  it("accepts a car the API would accept", () => {
    expect(vehicleProblem(ok, 2026)).toBe("");
    expect(vehicleProblem({ ...ok, year: 2027 }, 2026)).toBe("");
  });
  it("names the first problem", () => {
    expect(vehicleProblem({ ...ok, vin: "1HGCM82633A00435O" }, 2026)).toMatch(/never I, O or Q/);
    expect(vehicleProblem({ ...ok, vin: "123" }, 2026)).toMatch(/17 characters/);
    expect(vehicleProblem({ ...ok, make: " " }, 2026)).toMatch(/make/);
    expect(vehicleProblem({ ...ok, model: "" }, 2026)).toMatch(/model/);
    expect(vehicleProblem({ ...ok, year: 1979 }, 2026)).toBe("The model year must be between 1980 and 2027.");
    expect(vehicleProblem({ ...ok, year: 2028 }, 2026)).toMatch(/between 1980 and 2027/);
    expect(vehicleProblem({ ...ok, year: Number.NaN }, 2026)).toMatch(/model year/);
  });
  it("counts make and model in characters like the API, not UTF-16 units or bytes", () => {
    expect(charCount("Škoda")).toBe(5);
    expect(charCount("😀")).toBe(1);
    // 60 astral characters are 120 UTF-16 units (and 240 UTF-8 bytes) but within the limit.
    expect(vehicleProblem({ ...ok, model: "😀".repeat(VEHICLE_NAME_MAX) }, 2026)).toBe("");
    expect(vehicleProblem({ ...ok, make: "Š".repeat(VEHICLE_NAME_MAX) }, 2026)).toBe("");
    expect(vehicleProblem({ ...ok, make: "Š".repeat(VEHICLE_NAME_MAX + 1) }, 2026)).toMatch(/at most 60 characters/);
  });
});
