import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { deferred } from "./testFixtures";
import { useAsyncAction } from "./useAsyncAction";

describe("useAsyncAction", () => {
  it("stays busy and writing until the write settles, however fast an overlapping read ends", async () => {
    const { result } = renderHook(() => useAsyncAction());
    const booking = deferred<string>();
    const reload = deferred<string>();
    let write!: Promise<string | undefined>;
    let read!: Promise<string | undefined>;
    act(() => {
      write = result.current.run(() => booking.promise, { write: true });
      read = result.current.run(() => reload.promise);
    });
    expect(result.current).toMatchObject({ busy: true, writing: true });

    await act(async () => {
      reload.resolve("grid");
      await read;
    });
    // The read is done; the booking is not: Confirm and the inputs stay disabled.
    expect(result.current).toMatchObject({ busy: true, writing: true });

    await act(async () => {
      booking.resolve("booked");
      await write;
    });
    expect(result.current).toMatchObject({ busy: false, writing: false });
  });

  it("is not writing during a plain read", () => {
    const { result } = renderHook(() => useAsyncAction());
    act(() => {
      void result.current.run(() => new Promise(() => undefined));
    });
    expect(result.current).toMatchObject({ busy: true, writing: false });
  });
});
