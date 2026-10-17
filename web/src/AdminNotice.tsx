import { Icon } from "./Icon";

/** Success line under an admin form (polite live region, so screen readers announce it). */
export function AdminNotice({ message }: { message: string }) {
  if (!message) return null;
  return (
    <p className="banner ok" role="status">
      <Icon name="checkCircle" />
      {message}
    </p>
  );
}

/**
 * Said above an admin form whose unsaved edits started from an older version than the one saved
 * now (another admin, a live update): the edits stay; saving them would be refused (412).
 */
export function StaleDraftNotice({ what, onReload }: { what: string; onReload: () => void }) {
  return (
    <p className="banner info" role="status">
      <Icon name="info" />
      {what} changed elsewhere since you started editing — your edits are kept, but saving them would be refused.{" "}
      <button type="button" className="link" onClick={onReload}>
        <Icon name="refresh" />
        Reload the latest (drops your edits)
      </button>
    </p>
  );
}
