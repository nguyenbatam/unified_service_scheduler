import { describe, expect, it } from "vitest";
import { NetworkError, type ApiError } from "./api";
import { actionErrorText, bookingErrorText, errorText, isSlotConflict } from "./errors";

function apiError(status: number, message: string, extra: Partial<ApiError> = {}): ApiError {
  return Object.assign(new Error(message), { code: "x", status, ...extra }) as ApiError;
}

describe("errorText", () => {
  it("says when to retry after throttling", () => {
    expect(errorText(apiError(429, "too many requests", { retryAfter: 7 }))).toBe("Too many requests — try again in 7 s.");
    expect(errorText(apiError(429, "too many requests"))).toBe("Too many requests — try again in a moment.");
  });

  it("keeps api.ts's busy wording and explains a gateway 503", () => {
    const busy = apiError(503, "The scheduler is busy right now — your change may not have been saved.", { code: "busy" });
    expect(errorText(busy)).toBe(busy.message);
    expect(errorText(apiError(503, "Service Unavailable", { retryAfter: 2 }))).toBe(
      "The scheduler is busy right now — try again in 2 s.",
    );
  });

  it("names a request that never reached the server", () => {
    expect(errorText(new NetworkError(new TypeError("Failed to fetch")))).toBe(
      "Can't reach the server — check your connection and try again.",
    );
    expect(errorText(apiError(502, "Bad Gateway"))).toMatch(/Can't reach the server/);
  });

  it("keeps a bug's TypeError as it is, not as a connection problem", () => {
    expect(errorText(new TypeError("x.map is not a function"))).toBe("x.map is not a function");
  });

  it("keeps other messages", () => {
    expect(errorText(apiError(400, "vin is invalid"))).toBe("vin is invalid");
    expect(errorText(new Error("plain"))).toBe("plain");
    expect(errorText("text")).toBe("text");
  });
});

describe("bookingErrorText", () => {
  it("explains a taken slot and says the grid was refreshed", () => {
    expect(bookingErrorText(apiError(409, "resources were booked by a concurrent request"))).toBe(
      "That time was just taken — slots refreshed, please pick another time.",
    );
    expect(isSlotConflict(apiError(409, "x"))).toBe(true);
    expect(isSlotConflict(apiError(412, "x"))).toBe(false);
  });

  it("names the car's overlapping booking, the check-in cap and the lunch break", () => {
    expect(bookingErrorText(apiError(409, "this vehicle already has an appointment overlapping that time"))).toMatch(
      /^This car already has an appointment overlapping that time — slots refreshed/,
    );
    expect(bookingErrorText(apiError(422, "staging / check-in area is at capacity for that arrival window"))).toMatch(
      /^The check-in area is full/,
    );
    expect(bookingErrorText(apiError(422, "slot overlaps dealership lunch break"))).toMatch(/lunch break/);
  });

  it("keeps a worded policy refusal, capitalised", () => {
    expect(
      bookingErrorText(apiError(422, "online bookings need at least 60 minutes notice; call the dealership for sooner")),
    ).toBe("Online bookings need at least 60 minutes notice; call the dealership for sooner — slots refreshed, please pick another time.");
  });

  it("falls back to errorText for anything else", () => {
    expect(bookingErrorText(apiError(429, "slow down", { retryAfter: 3 }))).toBe("Too many requests — try again in 3 s.");
  });
});

describe("actionErrorText", () => {
  it("words a refused workshop action", () => {
    expect(actionErrorText(apiError(422, "check-in opens on the appointment day"))).toBe(
      "Not done: Check-in opens on the appointment day.",
    );
    expect(actionErrorText(new NetworkError(new TypeError("Failed to fetch")))).toMatch(/Can't reach the server/);
  });
});

describe("bookingErrorText by code", () => {
  it("lets the code decide the kind, the message only which rule", () => {
    // A conflict that mentions lunch is still a taken slot, not a lunch rule.
    expect(bookingErrorText(apiError(409, "slot near lunch was taken", { code: "conflict" }))).toBe(
      "That time was just taken — slots refreshed, please pick another time.",
    );
    expect(bookingErrorText(apiError(422, "slot overlaps dealership lunch break", { code: "unavailable" }))).toMatch(/^That time overlaps the lunch break/);
    // An unavailable refusal is never reported as the car's overlap.
    expect(bookingErrorText(apiError(422, "vehicle already has an appointment rule", { code: "unavailable" }))).not.toMatch(/This car/);
    expect(bookingErrorText(apiError(422, "", { code: "unavailable" }))).toBe(
      "That time can no longer be booked — slots refreshed, please pick another time.",
    );
  });
});
