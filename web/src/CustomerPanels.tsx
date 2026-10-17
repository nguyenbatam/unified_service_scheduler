import { useState } from "react";
import { ErrorBanner } from "./ErrorBanner";
import { api } from "./api";
import type { Vehicle } from "./types";
import { useAsyncAction } from "./useAsyncAction";
import { MIN_VEHICLE_YEAR, VEHICLE_NAME_MAX, vehicleProblem } from "./vehicles";
import { Icon } from "./Icon";
import { EmptyState } from "./Illustration";

type VehicleFormProps = {
  onCreated: (v: Vehicle) => void;
  /** The customer has no car yet: say that adding one is the first step. */
  first?: boolean;
};

/**
 * Self-service customers register their own car before booking. Problems (a mistyped VIN, a model
 * year out of range, or the server refusing the car) are shown in the form, next to the fields.
 */
export function VehicleForm({ onCreated, first = false }: VehicleFormProps) {
  const [vin, setVin] = useState("");
  const [make, setMake] = useState("");
  const [model, setModel] = useState("");
  const [year, setYear] = useState(new Date().getFullYear());
  const { busy, error, setError, run } = useAsyncAction();

  async function submit() {
    const problem = vehicleProblem({ vin, make, model, year });
    if (problem) {
      setError(problem);
      return;
    }
    await run(async () => {
      const created = await api.createVehicle({ vin: vin.trim().toUpperCase(), make: make.trim(), model: model.trim(), year });
      setVin("");
      setMake("");
      setModel("");
      onCreated(created);
    });
  }

  return (
    <section className="panel">
      <h2>
        <Icon name="car" />
        Add a vehicle
      </h2>
      {first ? <EmptyState art="garage">Add your car first — then pick a service and a time that suits you.</EmptyState> : null}
      <div className="grid">
        <label>
          <span>
            <Icon name="hash" />
            VIN (17 characters)
          </span>
          <input
            value={vin}
            maxLength={17}
            autoCapitalize="characters"
            aria-describedby="vehicle-form-hint"
            onChange={(e) => setVin(e.target.value)}
          />
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
      <p className="hint" id="vehicle-form-hint">
        The VIN is on your registration document or at the bottom of the windscreen; it never contains I, O or Q.
      </p>
      <ErrorBanner message={error} />
      <div className="actions">
        <button type="button" disabled={busy || !vin.trim() || !make.trim() || !model.trim()} onClick={submit}>
          <Icon name="plus" />
          {busy ? "Adding…" : "Add vehicle"}
        </button>
      </div>
    </section>
  );
}
