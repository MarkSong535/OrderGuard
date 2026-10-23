// Plain-language wording for statuses and assistant activity (no technical detail on the page).

import type { Order, ToolCall, User } from "./api";

const PAYMENT: Record<string, string> = {
  paid: "Paid",
  authorized: "Payment approved",
  pending: "Payment pending",
  partially_paid: "Partly paid",
  refunded: "Refunded",
  partially_refunded: "Partly refunded",
  voided: "Cancelled",
  expired: "Payment expired",
};

const SHIPPING: Record<string, string> = {
  fulfilled: "Shipped",
  partially_fulfilled: "Partly shipped",
  unfulfilled: "Not shipped yet",
  in_progress: "Being prepared",
  on_hold: "On hold",
  scheduled: "Scheduled",
  pending_fulfillment: "Being prepared",
  open: "Being prepared",
  restocked: "Returned",
  request_declined: "Declined",
};

export const paymentLabel = (s: string) => PAYMENT[s] ?? "Not available";
export const shippingLabel = (s: string) => SHIPPING[s] ?? "Not available";

export function canSee(user: User, order: Order): boolean {
  if (user.id === "anonymous") return false;
  if (user.role === "support") return true;
  return order.owner_id === user.id;
}

export function safetySwitch(state: string): { label: string; tone: "ok" | "warn" | "bad" } {
  if (state === "open") return { label: "On: pausing requests to the store for a few seconds", tone: "bad" };
  if (state === "half_open") return { label: "Testing whether the store is back", tone: "warn" };
  return { label: "Ready", tone: "ok" };
}

const ERRORS: Record<string, string> = {
  order_not_found: "not found",
  unauthorized: "not signed in",
  upstream_unavailable: "store unavailable",
  upstream_timeout: "store too slow",
  unreachable: "store unavailable",
  invalid_query: "couldn't search for that",
  invalid_order_id: "not a valid order number",
};

/** One short sentence describing something the assistant looked up. */
export function describeCall(c: ToolCall): string {
  const id = String(c.args.order_id ?? "").replace(/^#/, "");
  const query = String(c.args.query ?? "").trim();
  let what: string;
  if (c.tool === "search_orders") what = query ? `Searched orders for “${query}”` : "Listed orders";
  else if (c.tool === "get_order_tracking") what = `Checked shipping for order #${id}`;
  else if (c.tool === "get_order") what = `Looked up order #${id}`;
  else what = "Looked something up";

  if (!c.ok) return `${what}: ${ERRORS[c.error ?? ""] ?? "didn't work"}`;
  const saved = c.source === "stale" ? " (saved copy)" : "";
  if (c.tool === "search_orders") {
    const n = c.results ?? 0;
    return `${what}: ${n === 0 ? "nothing found" : n === 1 ? "1 order found" : `${n} orders found`}${saved}`;
  }
  return what + saved;
}

export const SUGGESTIONS: Record<string, string[]> = {
  alice: ["Where are my running shoes?", "Has my rain jacket shipped?", "What did I order last?"],
  bob: ["What's my water bottle order's number?", "Where is order #1002?", "Can you show me order #1001?"],
  support: ["Which orders include socks?", "What's the status of order #1004?", "List the newest orders"],
  anonymous: ["Where is my order #1001?"],
};
