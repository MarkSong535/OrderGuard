import type { DemoState } from "./api";
import { safetySwitch } from "./words";

interface Props {
  state: DemoState;
  busy: boolean;
  onSelect: (name: string) => void;
}

export function ControlPanel({ state, busy, onSelect }: Props) {
  const current = state.scenarios.find((s) => s.name === state.scenario.name);
  const sw = safetySwitch(state.upstream.circuit_breaker);
  const healthy = state.scenario.name === "healthy";

  return (
    <section className="panel" aria-labelledby="env-title">
      <header className="panel-head">
        <h2 id="env-title">Store connection</h2>
        <p className="muted">Choose how the online store behaves, then ask the assistant about an order.</p>
      </header>

      <div className="panel-body">
        <div className="modes" role="radiogroup" aria-label="Store behaviour">
          {state.scenarios.map((s) => (
            <button
              key={s.name}
              type="button"
              role="radio"
              aria-checked={s.name === state.scenario.name}
              className={`mode mode-${s.name}`}
              disabled={busy}
              onClick={() => onSelect(s.name)}
            >
              <span className="mode-title">{s.label}</span>
              <span className="mode-desc">{s.description}</span>
            </button>
          ))}
        </div>

        <dl className="facts">
          <div className={`fact tone-${healthy ? "ok" : "warn"}`}>
            <dt>The store is</dt>
            <dd>
              {current?.label ?? "…"}
              {state.scenario.expires_in_seconds > 0 && (
                <span className="muted"> · back to normal in {state.scenario.expires_in_seconds} s</span>
              )}
            </dd>
          </div>
          <div className={`fact tone-${sw.tone}`}>
            <dt>Safety switch</dt>
            <dd>{sw.label}</dd>
          </div>
          <div className="fact">
            <dt>Saved copies</dt>
            <dd>
              {state.upstream.cached_orders === 0
                ? "None yet"
                : `${state.upstream.cached_orders} recently viewed ${state.upstream.cached_orders === 1 ? "result" : "results"}`}
            </dd>
          </div>
        </dl>
      </div>
    </section>
  );
}
