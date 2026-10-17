import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Icon, roleIcon, slotReasonIcon, statusIcon } from "./Icon";
import { Role, Status } from "./types";

describe("Icon", () => {
  it("is decorative: hidden from assistive tech and adds no text to its label", () => {
    render(
      <button type="button">
        <Icon name="check" />
        Confirm booking
      </button>,
    );
    const button = screen.getByRole("button", { name: "Confirm booking" });
    expect(button.textContent).toBe("Confirm booking");
    expect(button.querySelector("svg")?.getAttribute("aria-hidden")).toBe("true");
  });

  it("has an icon of its own for every status, role, and known slot reason", () => {
    for (const s of Object.values(Status)) expect(statusIcon(s)).not.toBe("info");
    expect(new Set(Object.values(Role).map(roleIcon)).size).toBe(Object.values(Role).length);
    for (const r of ["past", "lead_time", "beyond_horizon", "lunch_break", "vehicle_busy", "staging_full", "no_bay", "no_technician"]) {
      expect(slotReasonIcon(r)).not.toBe("xCircle");
    }
    expect(slotReasonIcon("something_new")).toBe("xCircle");
  });
});
