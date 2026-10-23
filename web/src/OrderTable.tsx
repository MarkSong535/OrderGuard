import type { Order, User } from "./api";
import { canSee, paymentLabel, shippingLabel } from "./words";

export function OrderTable({ orders, user }: { orders: Order[]; user: User }) {
  const who = user.id === "anonymous" ? "A guest" : user.name;

  return (
    <section className="panel" aria-labelledby="db-title">
      <header className="panel-head">
        <h2 id="db-title">Store orders</h2>
        <p className="muted">Highlighted entries are entries that are available to the user.</p>
      </header>
      <div className="panel-body table-wrap">
        <table className="orders">
          <thead>
            <tr>
              <th scope="col">Order</th>
              <th scope="col">Customer</th>
              <th scope="col">Items</th>
              <th scope="col">Total</th>
              <th scope="col">Payment</th>
              <th scope="col">Shipping</th>
              <th scope="col">Placed</th>
            </tr>
          </thead>
          <tbody>
            {orders.map((o) => {
              const mine = canSee(user, o);
              return (
                <tr key={o.id} className={mine ? "visible" : "hidden-to-user"}>
                  <th scope="row">
                    {o.name}
                    {mine && <span className="sr-only"> (visible to {who})</span>}
                  </th>
                  <td>{o.owner}</td>
                  <td>{o.items.join(", ")}</td>
                  <td className="num">{o.total}</td>
                  <td>{paymentLabel(o.financial_status)}</td>
                  <td>{shippingLabel(o.fulfillment_status)}</td>
                  <td className="num">{o.created_at}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}
