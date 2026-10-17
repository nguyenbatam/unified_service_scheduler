import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AppointmentList } from "./AppointmentList";
import { dispatcher, makeAppointment, on, signIn, stubFetch } from "./testFixtures";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

const appt = (id: string, day: string) =>
  makeAppointment({ id, startAt: `${day}T09:00:00Z`, endAt: `${day}T10:00:00Z`, vehicleLabel: `Car ${id}` });

describe("AppointmentList", () => {
  it("pages upcoming (soonest first) and past (newest first) separately, each with its own Show more", async () => {
    signIn(dispatcher);
    const asked: string[] = [];
    stubFetch(
      on("GET", "/api/v1/appointments", (url) => {
        const when = url.searchParams.get("when");
        const cursor = url.searchParams.get("cursor");
        asked.push(`${when}:${cursor ?? "-"}`);
        if (when === "upcoming") {
          return cursor
            ? { body: [appt("u3", "2030-01-09")] }
            : { body: [appt("u1", "2030-01-07"), appt("u2", "2030-01-08")], headers: { "X-Next-Cursor": "u-next" } };
        }
        return cursor
          ? { body: [appt("p2", "2020-01-06")] }
          : { body: [appt("p1", "2020-01-07")], headers: { "X-Next-Cursor": "p-next" } };
      }),
    );
    const user = userEvent.setup();
    render(<AppointmentList title="Appointments" timezoneOf={() => "UTC"} onSelect={vi.fn()} />);
    await screen.findByText(/Car u1/);
    expect(screen.getByText("Past and cancelled (1+)")).toBeTruthy();

    await user.click(screen.getByRole("button", { name: "Show later" }));
    await screen.findByText(/Car u3/);
    expect(screen.queryByRole("button", { name: "Show later" })).toBeNull();

    const past = screen.getByText(/Past and cancelled/).closest("details")!;
    await user.click(within(past).getByRole("button", { name: "Show older" }));
    await waitFor(() => expect(screen.getByText("Past and cancelled (2)")).toBeTruthy());
    expect(asked.sort()).toEqual(["past:-", "past:p-next", "upcoming:-", "upcoming:u-next"]);
  });
});
