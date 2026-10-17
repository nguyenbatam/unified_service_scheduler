import { act, render, renderHook, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AdminPanel } from "./AdminPanel";
import { DealershipSettings } from "./DealershipSettings";
import { dealership, on, service, stubFetch } from "./testFixtures";
import { Role } from "./types";
import { useDraft } from "./useDraft";

afterEach(() => vi.unstubAllGlobals());

describe("useDraft", () => {
  it("follows the source while untouched, keeps edits and reports stale when the source changes", () => {
    const { result, rerender } = renderHook((p: { src: { a: number }; v: number }) => useDraft(p.src, p.v, "x"), {
      initialProps: { src: { a: 1 }, v: 1 },
    });
    rerender({ src: { a: 2 }, v: 2 });
    expect(result.current.value).toEqual({ a: 2 });
    expect(result.current.version).toBe(2);

    act(() => result.current.set(() => ({ a: 5 })));
    rerender({ src: { a: 3 }, v: 3 });
    expect(result.current.value).toEqual({ a: 5 });
    expect(result.current.stale).toBe(true);
    expect(result.current.version).toBe(2); // saving sends the base version: 412, never an overwrite

    act(() => result.current.reload());
    expect(result.current).toMatchObject({ value: { a: 3 }, version: 3, stale: false, dirty: false });
  });

  it("never follows a source older than its base (the catalog not yet reloaded after its own save)", () => {
    const { result, rerender } = renderHook((p: { src: { a: number }; v: number }) => useDraft(p.src, p.v, "x"), {
      initialProps: { src: { a: 1 }, v: 1 },
    });
    act(() => result.current.rebase({ a: 2 }, 2)); // saved: the server answered version 2
    rerender({ src: { a: 1 }, v: 1 }); // the catalog still holds version 1
    expect(result.current).toMatchObject({ value: { a: 2 }, version: 2, stale: false });
  });

  it("moves the base version along when only the version changed (a change to other fields)", () => {
    const { result, rerender } = renderHook((p: { v: number }) => useDraft({ a: 1 }, p.v, "x"), { initialProps: { v: 1 } });
    act(() => result.current.set(() => ({ a: 9 })));
    rerender({ v: 2 });
    expect(result.current).toMatchObject({ value: { a: 9 }, version: 2, stale: false });
  });
});

describe("DealershipSettings live updates", () => {
  it("keeps unsaved edits when the dealership changes elsewhere, says so, and reloads on request", async () => {
    const { rerender } = render(<DealershipSettings dealer={dealership} onChanged={vi.fn()} />);
    const opens = () => screen.getByLabelText("Opens") as HTMLInputElement;
    const closes = () => screen.getByLabelText("Closes") as HTMLInputElement;
    await userEvent.clear(opens());
    await userEvent.type(opens(), "09:00");
    expect(opens().value).toBe("09:00");

    rerender(<DealershipSettings dealer={{ ...dealership, closeTime: "18:00", version: 2 }} onChanged={vi.fn()} />);
    expect(opens().value).toBe("09:00");
    expect(screen.getByText(/changed elsewhere since you started editing/)).toBeTruthy();

    await userEvent.click(screen.getByRole("button", { name: /Reload the latest/ }));
    expect(opens().value).toBe("08:00");
    expect(closes().value).toBe("18:00");
    expect(screen.queryByText(/changed elsewhere/)).toBeNull();
  });

  it("follows a live update while untouched, and saves a stale draft with its own version (412)", async () => {
    const versions: (string | null)[] = [];
    stubFetch((url, init) => {
      if (url.pathname !== "/api/v1/admin/dealerships/d1") return undefined;
      versions.push(new Headers(init?.headers).get("If-Match"));
      return { status: 412, body: { error: { code: "precondition_failed", message: "dealership was changed" } } };
    });
    const { rerender } = render(<DealershipSettings dealer={dealership} onChanged={vi.fn()} />);
    rerender(<DealershipSettings dealer={{ ...dealership, openTime: "07:30", version: 2 }} onChanged={vi.fn()} />);
    expect((screen.getByLabelText("Opens") as HTMLInputElement).value).toBe("07:30");

    await userEvent.clear(screen.getByLabelText("Closes"));
    await userEvent.type(screen.getByLabelText("Closes"), "16:00");
    rerender(<DealershipSettings dealer={{ ...dealership, openTime: "07:00", version: 3 }} onChanged={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Save dealership" }));
    await waitFor(() => expect(versions).toEqual(['"2"']));
  });
});

describe("admin saves without a catalog reload", () => {
  it("keeps the saved values and their version when the reload after a save fails", async () => {
    const versions: (string | null)[] = [];
    stubFetch((url, init) => {
      if (url.pathname !== "/api/v1/admin/dealerships/d1") return undefined;
      versions.push(new Headers(init?.headers).get("If-Match"));
      const body = JSON.parse(String(init?.body)) as { closeTime?: string };
      return { body: { ...dealership, closeTime: body.closeTime ?? dealership.closeTime, version: versions.length + 1 } };
    });
    const onChanged = vi.fn(() => Promise.reject(new Error("catalog unavailable")));
    render(<DealershipSettings dealer={dealership} onChanged={onChanged} />);
    await userEvent.clear(screen.getByLabelText("Closes"));
    await userEvent.type(screen.getByLabelText("Closes"), "16:00");
    await userEvent.click(screen.getByRole("button", { name: "Save dealership" }));
    await waitFor(() => expect(versions).toEqual(['"1"']));
    // The catalog (and so `dealer`) is still version 1: the form keeps what was saved.
    expect((screen.getByLabelText("Closes") as HTMLInputElement).value).toBe("16:00");
    await userEvent.click(screen.getByRole("button", { name: "Save dealership" }));
    await waitFor(() => expect(versions).toEqual(['"1"', '"2"']));
  });
});

describe("ServiceCatalog live updates", () => {
  const admin = { id: "u9", email: "g@x", name: "Gale", role: Role.Admin };

  it("keeps an unsaved catalog edit when the service changes elsewhere, and follows it while untouched", async () => {
    stubFetch(on("GET", "/api/v1/admin/technicians", { body: [] }));
    const props = { dealerships: [dealership], onChanged: vi.fn(), user: admin };
    const { rerender } = render(<AdminPanel {...props} services={[service]} />);
    await userEvent.click(screen.getByRole("tab", { name: "Service catalog" }));
    const duration = () => screen.getByLabelText("Duration (minutes, ×30)") as HTMLInputElement;

    rerender(<AdminPanel {...props} services={[{ ...service, durationMinutes: 90, version: 2 }]} />);
    expect(duration().value).toBe("90"); // untouched: follows

    await userEvent.clear(duration());
    await userEvent.type(duration(), "120");
    rerender(<AdminPanel {...props} services={[{ ...service, durationMinutes: 90, minSkillLevel: 2, version: 3 }]} />);
    expect(duration().value).toBe("120");
    expect(screen.getByText(/This service was changed elsewhere/)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: /Reload the latest/ }));
    expect(duration().value).toBe("90");
  });
});
