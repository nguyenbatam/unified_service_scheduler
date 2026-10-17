import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { StaffWalkIn } from "./StaffWalkIn";
import { deferred, on, signIn, dispatcher, stubFetch } from "./testFixtures";
import type { Customer, Vehicle } from "./types";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

describe("StaffWalkIn", () => {
  it("records a new walk-in from the search text, then adds their car", async () => {
    signIn(dispatcher);
    const wanda: Customer = { id: "c9", name: "Wanda", phone: "0400 123 987", email: "" };
    const car: Vehicle = {
      id: "v9",
      customerId: "c9",
      customerName: "Wanda",
      vin: "1HGCM82633A004352",
      make: "Honda",
      model: "Accord",
      year: 2020,
    };
    let created: unknown;
    let carBody: unknown;
    stubFetch(
      on("GET", "/api/v1/customers", { body: [] }),
      (url, init) => {
        if (init?.method !== "POST") return undefined;
        if (url.pathname === "/api/v1/customers") {
          created = JSON.parse(String(init.body));
          return { status: 201, body: wanda };
        }
        if (url.pathname === "/api/v1/vehicles") {
          carBody = JSON.parse(String(init.body));
          return { status: 201, body: car };
        }
        return undefined;
      },
    );
    const onCreated = vi.fn();
    const user = userEvent.setup();
    render(<StaffWalkIn onCreated={onCreated} />);

    await user.type(screen.getByLabelText(/Find the customer/), "0400 123 987");
    await screen.findByText("No customer found.");
    await user.click(screen.getByRole("button", { name: "New customer…" }));
    // A phone-looking search prefills the phone; the name is still needed.
    expect(screen.getByLabelText("Phone")).toHaveProperty("value", "0400 123 987");
    await user.type(screen.getByLabelText("Name"), "Wanda");
    await user.click(screen.getByRole("button", { name: "Record customer" }));
    await screen.findByText("Wanda");
    expect(created).toEqual({ name: "Wanda", phone: "0400 123 987", email: "" });

    await user.type(screen.getByLabelText(/VIN/), car.vin);
    await user.type(screen.getByLabelText("Make"), "Honda");
    await user.type(screen.getByLabelText("Model"), "Accord");
    await user.click(screen.getByRole("button", { name: "Add vehicle" }));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(car));
    expect(carBody).toMatchObject({ customerId: "c9", vin: car.vin });
  });

  it("asks for a way to reach a new customer before saving", async () => {
    signIn(dispatcher);
    const fetchStub = stubFetch(on("GET", "/api/v1/customers", { body: [] }));
    const user = userEvent.setup();
    render(<StaffWalkIn onCreated={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: "New customer…" }));
    await user.type(screen.getByLabelText("Name"), "Nobody");
    await user.click(screen.getByRole("button", { name: "Record customer" }));
    await screen.findByText(/^Enter a phone number or an email/);
    expect(fetchStub.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
  });

  it("stops showing Searching… once the query drops below 2 characters mid-request", async () => {
    signIn(dispatcher);
    const slow = deferred<{ body: Customer[] }>();
    stubFetch(on("GET", "/api/v1/customers", () => slow.promise));
    const user = userEvent.setup();
    render(<StaffWalkIn onCreated={vi.fn()} />);
    await user.type(screen.getByLabelText(/Find the customer/), "An");
    await screen.findByText("Searching…");
    await user.type(screen.getByLabelText(/Find the customer/), "{Backspace}");
    expect(screen.queryByText("Searching…")).toBeNull();
    slow.resolve({ body: [{ id: "c1", name: "Ann", phone: "1", email: "" }] });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByText("Searching…")).toBeNull();
    expect(screen.queryByRole("button", { name: /Ann/ })).toBeNull(); // the late answer is dropped
  });

  it("shows more customers page by page", async () => {
    signIn(dispatcher);
    const cursors: (string | null)[] = [];
    stubFetch(
      on("GET", "/api/v1/customers", (url) => {
        cursors.push(url.searchParams.get("cursor"));
        return url.searchParams.get("cursor")
          ? { body: [{ id: "c2", name: "Anna", phone: "2", email: "" }] }
          : { body: [{ id: "c1", name: "Ann", phone: "1", email: "" }], headers: { "X-Next-Cursor": "p2" } };
      }),
    );
    const user = userEvent.setup();
    render(<StaffWalkIn onCreated={vi.fn()} />);
    await user.type(screen.getByLabelText(/Find the customer/), "Ann");
    await user.click(await screen.findByRole("button", { name: "Show more customers" }));
    await screen.findByRole("button", { name: /Anna/ });
    expect(screen.getByRole("button", { name: /^Ann · 1/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Show more customers" })).toBeNull();
    expect(cursors).toEqual([null, "p2"]);
  });
});
