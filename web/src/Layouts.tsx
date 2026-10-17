import { type ReactNode, useState } from "react";
import { Tabs } from "./Tabs";
import { Icon } from "./Icon";

type StaffProps = {
  /** Admins also get the configuration tab. */
  admin?: ReactNode;
  /** The workshop context (dealership + board day), above the board. */
  bar: ReactNode;
  booking: ReactNode;
  board: ReactNode;
  /** The open appointment, right under the board it was picked from. */
  appointment: ReactNode;
};

/**
 * Dispatchers and admins: the workshop bar, day board and booking flow first; configuration in its
 * own tab. Both tabs stay mounted once shown (the hidden one with `hidden`), so switching back
 * keeps the admin's sub-tab and unsaved drafts, and the booking form's state.
 */
export function StaffLayout({ admin, bar, booking, board, appointment }: StaffProps) {
  const [tab, setTab] = useState<"workshop" | "admin">("workshop");
  // The admin panel loads its data on mount: wait until it is first opened.
  const [adminOpened, setAdminOpened] = useState(false);
  const workshop = (
    <>
      {bar}
      {board}
      {appointment}
      {booking}
    </>
  );
  if (!admin) return workshop;
  return (
    <Tabs
      label="Sections"
      idPrefix="staff"
      tabs={[
        { id: "workshop", label: "Bookings & day board", icon: "board" },
        { id: "admin", label: "Configuration", icon: "settings" },
      ]}
      active={tab}
      onChange={(next) => {
        if (next === "admin") setAdminOpened(true);
        setTab(next);
      }}
    >
      <div className="tab-body" hidden={tab !== "workshop"}>
        {workshop}
      </div>
      {adminOpened ? (
        <div className="tab-body" hidden={tab !== "admin"}>
          {admin}
        </div>
      ) : null}
    </Tabs>
  );
}

type CustomerProps = {
  mine: ReactNode;
  booking: ReactNode;
  vehicleForm: ReactNode;
  /** No car yet: adding one comes first. */
  hasVehicles: boolean;
};

/** Customers: their appointments, then booking; adding a car is first only until they have one. */
export function CustomerLayout({ mine, booking, vehicleForm, hasVehicles }: CustomerProps) {
  if (!hasVehicles) {
    return (
      <>
        {vehicleForm}
        {mine}
        {booking}
      </>
    );
  }
  return (
    <>
      {mine}
      {booking}
      <details className="panel collapsible">
        <summary>
          <Icon name="car" />
          Add another vehicle
        </summary>
        {vehicleForm}
      </details>
    </>
  );
}
