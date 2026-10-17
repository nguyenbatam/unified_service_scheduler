import { useState } from "react";
import { AdminNotice } from "./AdminNotice";
import { ErrorBanner } from "./ErrorBanner";
import { customerProblem } from "./admin";
import { MAX_CUSTOMER_LIMIT, api } from "./api";
import type { Customer, Vehicle } from "./types";
import { useAdminAction } from "./useAdminAction";
import { usePagedList } from "./usePagedList";
import { MIN_VEHICLE_YEAR, VEHICLE_NAME_MAX, vehicleProblem } from "./vehicles";
import { Icon } from "./Icon";
import { EmptyState } from "./Illustration";

type Props = {
  /** The new car, so the booking form can select it. */
  onCreated: (v: Vehicle) => void;
  /** The customer picked or recorded (null when cleared), e.g. to list their appointments. */
  onCustomer?: (c: Customer | null) => void;
};

/** Customers per search page ("Show more" for the rest), and the search debounce. */
const CUSTOMER_PAGE = 20;
const SEARCH_DEBOUNCE_MS = 250;

/** "0400 111 222 · ann@example.com" — whatever contact details the customer has. */
function contactOf(c: Customer): string {
  return [c.phone, c.email].filter(Boolean).join(" · ");
}

/**
 * Staff at the desk: find the customer (name, phone or email) or record a new walk-in (no account
 * needed), then add their car; it is selected in the booking form.
 */
export function StaffWalkIn({ onCreated, onCustomer }: Props) {
  const [q, setQ] = useState("");
  const [customer, setCustomer] = useState<Customer | null>(null);
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [phone, setPhone] = useState("");
  const [email, setEmail] = useState("");
  const [vin, setVin] = useState("");
  const [make, setMake] = useState("");
  const [model, setModel] = useState("");
  const [year, setYear] = useState(new Date().getFullYear());
  const { busy, error, setError, notice, save } = useAdminAction();

  function pick(c: Customer | null) {
    setCustomer(c);
    onCustomer?.(c);
  }

  // Debounced search from 2 characters, a page at a time; a late answer for an older query is
  // dropped, and "Searching…" only ever reflects the current query (none below 2 characters).
  const text = q.trim();
  const search = usePagedList<Customer>((cursor, limit) => api.customers({ q: text, limit, cursor }), text, {
    pageSize: CUSTOMER_PAGE,
    maxPageSize: MAX_CUSTOMER_LIMIT,
    enabled: text.length >= 2,
    delayMs: SEARCH_DEBOUNCE_MS,
  });
  const found = search.items ?? [];
  const searching = search.loading && search.items === null;

  function startCreating() {
    // Prefill from the search text: a phone-looking query goes to phone, an address to email.
    const text = q.trim();
    if (text.includes("@")) setEmail(text);
    else if (/^[\d\s+().-]+$/.test(text)) setPhone(text);
    else setName(text);
    setCreating(true);
  }

  async function createCustomer() {
    const problem = customerProblem({ name, phone, email });
    if (problem) {
      setError(problem);
      return;
    }
    await save(async () => {
      const created = await api.createCustomer({ name: name.trim(), phone: phone.trim(), email: email.trim() });
      pick(created);
      setCreating(false);
      setName("");
      setPhone("");
      setEmail("");
    }, `${name.trim()} recorded. Add their car below.`);
  }

  async function submit() {
    if (!customer) return;
    const problem = vehicleProblem({ vin, make, model, year });
    if (problem) {
      setError(problem);
      return;
    }
    const ok = await save(async () => {
      onCreated(
        await api.createVehicle({
          customerId: customer.id,
          vin: vin.trim().toUpperCase(),
          make: make.trim(),
          model: model.trim(),
          year,
        }),
      );
    }, `${make} ${model} added for ${customer.name} and selected for booking.`);
    if (ok) {
      setVin("");
      setMake("");
      setModel("");
    }
  }

  return (
    <section className="panel">
      <h2>
        <Icon name="userPlus" />
        Walk-in: customer and car
      </h2>
      <ErrorBanner message={error || search.error} />
      <AdminNotice message={notice} />
      {customer ? (
        <p>
          <Icon name="user" />
          Customer: <strong>{customer.name}</strong>
          {contactOf(customer) ? <span className="hint"> · {contactOf(customer)}</span> : null}{" "}
          <button type="button" onClick={() => pick(null)}>
            <Icon name="reassign" />
            Change
          </button>
        </p>
      ) : creating ? (
        <>
          <div className="grid">
            <label>
              <span>
                <Icon name="user" />
                Name
              </span>
              <input value={name} maxLength={120} onChange={(e) => setName(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="phone" />
                Phone
              </span>
              <input type="tel" value={phone} maxLength={30} onChange={(e) => setPhone(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="mail" />
                Email
              </span>
              <input type="email" value={email} maxLength={254} onChange={(e) => setEmail(e.target.value)} />
            </label>
          </div>
          <p className="hint">A phone number or an email is needed for confirmations and reminders.</p>
          <button type="button" className="primary" disabled={busy || !name.trim()} onClick={createCustomer}>
            <Icon name="check" />
            {busy ? "Saving…" : "Record customer"}
          </button>{" "}
          <button type="button" onClick={() => setCreating(false)}>
            <Icon name="chevronLeft" />
            Back to search
          </button>
        </>
      ) : (
        <>
          <label>
            <span>
              <Icon name="search" />
              Find the customer (name, phone or email)
            </span>
            <input type="search" value={q} maxLength={100} onChange={(e) => setQ(e.target.value)} />
          </label>
          {searching ? <p className="hint">Searching…</p> : null}
          {!searching && search.items !== null && text.length >= 2 && found.length === 0 ? (
            <EmptyState art="person">No customer found.</EmptyState>
          ) : null}
          <div className="tech-list">
            {found.map((c) => (
              <button key={c.id} type="button" className="tech-row" onClick={() => pick(c)}>
                <span>
                  <Icon name="user" />
                  <strong>{c.name}</strong>
                  {contactOf(c) ? ` · ${contactOf(c)}` : ""}
                </span>
              </button>
            ))}
          </div>
          {search.hasMore ? (
            <button type="button" disabled={search.loading} onClick={() => void search.more()}>
              <Icon name="chevronDown" />
              {search.loading ? "Loading…" : "Show more customers"}
            </button>
          ) : null}{" "}
          <button type="button" onClick={startCreating}>
            <Icon name="plus" />
            New customer…
          </button>
        </>
      )}
      {customer ? (
        <>
          <div className="grid">
            <label>
              <span>
                <Icon name="hash" />
                VIN (17 characters)
              </span>
              <input value={vin} maxLength={17} onChange={(e) => setVin(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="car" />
                Make
              </span>
              <input value={make} maxLength={VEHICLE_NAME_MAX} onChange={(e) => setMake(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="car" />
                Model
              </span>
              <input value={model} maxLength={VEHICLE_NAME_MAX} onChange={(e) => setModel(e.target.value)} />
            </label>
            <label>
              <span>
                <Icon name="calendar" />
                Year
              </span>
              <input
                type="number"
                min={MIN_VEHICLE_YEAR}
                max={new Date().getFullYear() + 1}
                value={year}
                onChange={(e) => setYear(Number(e.target.value))}
              />
            </label>
          </div>
          <button
            type="button"
            className="primary"
            disabled={busy || vin.trim().length !== 17 || !make.trim() || !model.trim()}
            onClick={submit}
          >
            <Icon name="car" />
            {busy ? "Adding…" : "Add vehicle"}
          </button>
        </>
      ) : null}
    </section>
  );
}
