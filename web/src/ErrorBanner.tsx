import { Icon } from "./Icon";

type Props = {
  message: string;
  /** Shows a dismiss button when set. */
  onDismiss?: () => void;
};

/**
 * An error line. Its live region is always mounted and only the banner inside comes and goes, so
 * screen readers announce every message (a region inserted with its text is often missed).
 */
export function ErrorBanner({ message, onDismiss }: Props) {
  return (
    <div className="live-region" aria-live="assertive" aria-atomic="true">
      {message ? (
        <div className="banner error" role="alert">
          <span>
            <Icon name="alert" />
            {message}
          </span>
          {onDismiss ? (
            <button type="button" className="banner-close" aria-label="Dismiss message" onClick={onDismiss}>
              <Icon name="x" />
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
