// Vitest setup (vite.config.ts): every test starts signed out, with a BroadcastChannel stand-in
// (cross-tab auth messages stay inside the test) and no auth state left by the previous test.
import { afterEach, beforeEach } from "vitest";
import { resetAuthForTests } from "./auth";
import { FakeBroadcastChannel } from "./testFixtures";

beforeEach(() => {
  resetAuthForTests();
  FakeBroadcastChannel.reset();
  globalThis.BroadcastChannel = FakeBroadcastChannel as unknown as typeof BroadcastChannel;
});

afterEach(() => {
  resetAuthForTests();
});
