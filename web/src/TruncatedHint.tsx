import { FETCH_ALL_MAX } from "./api";

/** Under a dropdown read with fetchAll: says it lists only the first FETCH_ALL_MAX rows. */
export function TruncatedHint({ shown, what }: { shown: boolean; what: string }) {
  if (!shown) return null;
  return (
    <p className="hint" role="note">
      Only the first {FETCH_ALL_MAX} {what} are listed.
    </p>
  );
}
