"""Shared JSON-over-HTTP helper for the smoke scripts (scripts/smoke*.sh).

The scripts run their Python from a heredoc under ``python3 -I`` (isolated: no cwd, no
PYTHON* env on sys.path), so they pass this directory as argv[1] and insert it themselves:

    python3 -I - "$ROOT/scripts/lib" ... <<'PY'
    import sys; sys.path.insert(0, sys.argv.pop(1))
    from smoke_http import Client, credentials

Every request has a timeout (no smoke can hang forever on a stuck backend), and an HTTP error
or transport failure is reported with the method, URL, status, and response body. Bookings a
smoke makes go through ``Holds``, which cancels whatever is still held when the process exits,
so a failed assertion never leaves a booking blocking the next run.
"""

import atexit
import http.cookiejar
import json
import os
import signal
import socket
import sys
import urllib.error
import urllib.request

DEFAULT_TIMEOUT = float(os.environ.get("SMOKE_HTTP_TIMEOUT", "30"))


class SmokeHTTPError(AssertionError):
    """A request failed: HTTP status >= 400 (when checked), timeout, or connection error."""

    def __init__(self, method, url, message, status=None, body=None):
        self.method, self.url, self.status, self.body = method, url, status, body
        super().__init__(f"{method} {url}: {message}")


def credentials(role="ADMIN", email="admin@scheduler.local", password="admin123"):
    """(email, password) from <ROLE>_EMAIL / <ROLE>_PASSWORD, defaulting to the seeded demo user."""
    return os.environ.get(f"{role}_EMAIL", email), os.environ.get(f"{role}_PASSWORD", password)


def _decode(raw):
    if not raw:
        return None
    text = raw.decode("utf-8", "replace")
    try:
        return json.loads(text)
    except ValueError:
        return text


# AUTH_COOKIE_SECURE=false names it uss_refresh (Path=/api/v1/auth); true: __Host-uss_refresh (Path=/).
REFRESH_COOKIES = ("uss_refresh", "__Host-uss_refresh")


class Client:
    """JSON client for one base URL (a path starting with http:// or https:// is used as is).

    Requests authenticate with the Bearer access token from ``login``. With ``cookies=True`` the
    client also keeps a cookie jar (one per client: use one client per signed-in user), so the
    HttpOnly refresh cookie that login sets (``uss_refresh``, Path=/api/v1/auth) is sent back by
    ``refresh`` and cleared by logout. Without it the cookie is ignored (smokes are shorter than the
    access token's lifetime).
    """

    def __init__(self, base="", timeout=DEFAULT_TIMEOUT, cookies=False):
        self.base = base.rstrip("/")
        self.timeout = timeout
        self.jar = http.cookiejar.CookieJar() if cookies else None
        handlers = [urllib.request.HTTPCookieProcessor(self.jar)] if cookies else []
        self._open = urllib.request.build_opener(*handlers).open

    def _url(self, path):
        return path if path.startswith(("http://", "https://")) else self.base + path

    def request(self, method, path, body=None, headers=None):
        """Send one request; return (status, decoded body, headers) for any HTTP status.

        Raises SmokeHTTPError only when no HTTP answer arrived (timeout, refused, reset).
        """
        url = self._url(path)
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(url, data=data, method=method)
        req.add_header("Accept", "application/json")
        if body is not None:
            req.add_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            req.add_header(k, v)
        try:
            with self._open(req, timeout=self.timeout) as resp:
                # resp.headers is case-insensitive; a plain dict of it would not be.
                return resp.status, _decode(resp.read()), resp.headers
        except urllib.error.HTTPError as err:
            with err:
                return err.code, _decode(err.read()), err.headers
        except (socket.timeout, TimeoutError) as err:
            raise SmokeHTTPError(method, url, f"no answer within {self.timeout:g}s ({err})") from None
        except urllib.error.URLError as err:
            if isinstance(err.reason, (socket.timeout, TimeoutError)):
                raise SmokeHTTPError(method, url, f"no answer within {self.timeout:g}s") from None
            raise SmokeHTTPError(method, url, f"connection failed: {err.reason}") from None

    def call(self, method, path, body=None, headers=None):
        """Like request, but an HTTP status >= 400 raises SmokeHTTPError with the response body."""
        status, payload, hdrs = self.request(method, path, body, headers)
        if status >= 400:
            shown = payload if isinstance(payload, str) else json.dumps(payload)
            if shown and len(shown) > 2000:
                shown = shown[:2000] + "…"
            raise SmokeHTTPError(method, self._url(path), f"HTTP {status}: {shown}", status, payload)
        return status, payload, hdrs

    def get_all(self, path, headers=None, limit=None):
        """Every item of a paged list endpoint: path (it may carry a query), with ``limit`` when
        given (each listing has its own maximum: 200 for most, 100 for /customers; None leaves
        the server's default page size), following X-Next-Cursor to the last page. Every list is
        paged, so a plain GET would see only the first page."""
        from urllib.parse import quote

        first = path if limit is None else f"{path}{'&' if '?' in path else '?'}limit={limit}"
        sep = "&" if "?" in first else "?"
        items, url = [], first
        for _ in range(10_000):
            _, page, hdrs = self.call("GET", url, None, headers)
            assert isinstance(page, list), f"GET {url}: expected a JSON array, got {page!r}"
            items.extend(page)
            cursor = hdrs.get("X-Next-Cursor")
            if not cursor:
                return items
            url = f"{first}{sep}cursor={quote(cursor, safe='')}"
        raise AssertionError(f"GET {path}: no last page")

    def login(self, email, password):
        """Log in; return (Authorization header dict, user object)."""
        _, session, _ = self.call("POST", "/api/v1/auth/login", {"email": email, "password": password})
        assert isinstance(session, dict) and session.get("token"), f"login {email}: no token in {session!r}"
        return {"Authorization": f"Bearer {session['token']}"}, session.get("user")

    def refresh_cookie(self):
        """The refresh token this client's jar holds (None without a jar or a cookie)."""
        for cookie in self.jar or []:
            if cookie.name in REFRESH_COOKIES:
                return cookie.value
        return None

    def refresh(self):
        """POST /api/v1/auth/refresh with the jar's cookie and the CSRF header (no Origin, no
        Bearer); return (status, body). Needs ``cookies=True``."""
        assert self.jar is not None, "refresh needs Client(cookies=True)"
        status, body, _ = self.request("POST", "/api/v1/auth/refresh", None, {"X-Requested-With": "fetch"})
        return status, body


class Holds:
    """Appointments a smoke booked and has not cancelled yet; the rest are cancelled at exit.

    The exit hook (atexit, plus SIGTERM turned into a normal exit) runs on success, on a failed
    assertion and on Ctrl-C alike, the Python twin of a shell ``trap … EXIT``. A smoke that
    cancels a booking itself (and asserts on it) calls ``released`` so cleanup skips it.
    """

    def __init__(self, client, headers):
        self.client, self.headers, self.ids = client, headers, []
        atexit.register(self._cancel_all)
        if signal.getsignal(signal.SIGTERM) is signal.SIG_DFL:
            signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))

    def add(self, appt):
        """Track a booking (its id, or the created appointment body; anything else is ignored)."""
        appt_id = appt.get("id") if isinstance(appt, dict) else appt
        if appt_id and appt_id not in self.ids:
            self.ids.append(appt_id)

    def released(self, appt_id):
        if appt_id in self.ids:
            self.ids.remove(appt_id)

    def _cancel_all(self):
        for appt_id in list(self.ids):
            path = f"/api/v1/appointments/{appt_id}/cancel"
            try:
                status, body, _ = self.client.request("POST", path, None, self.headers)
            except Exception as err:  # noqa: BLE001 - best-effort cleanup: keep cancelling the rest
                print(f"cleanup: could not cancel {appt_id}: {err}", file=sys.stderr)
                continue
            if status < 400:
                print(f"cleanup: cancelled {appt_id}", file=sys.stderr)
            else:
                print(f"cleanup: cancel {appt_id} -> HTTP {status}: {body}", file=sys.stderr)
            self.ids.remove(appt_id)
