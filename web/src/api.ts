// Client for the demo server's /demo/api endpoints.

export type Role = "customer" | "support";

export interface User {
  id: string;
  name: string;
  role: Role;
}

export interface Order {
  id: string;
  name: string;
  owner_id: string; // "" for guest checkouts
  owner: string;
  items: string[];
  total: string;
  created_at: string;
  financial_status: string;
  fulfillment_status: string;
}

export interface Scenario {
  name: string;
  label: string;
  description: string;
}

export interface DemoState {
  users: User[];
  orders: Order[];
  scenarios: Scenario[];
  scenario: { name: string; expires_in_seconds: number };
  upstream: { circuit_breaker: string; cached_orders: number };
  assistant: { enabled: boolean };
}

export interface ToolCall {
  tool: string;
  args: Record<string, unknown>;
  ok: boolean;
  error?: string;
  results?: number;
  source?: string;
}

export interface ChatReply {
  answer: string;
  tool_calls: ToolCall[];
}

export const GUEST: User = { id: "anonymous", name: "Guest", role: "customer" };

async function json<T>(res: Response): Promise<T> {
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    const message = (body as { error?: { message?: string } }).error?.message;
    throw new Error(message || "Something went wrong. Please try again.");
  }
  return body as T;
}

export async function fetchState(): Promise<DemoState> {
  return json(await fetch("/demo/api/state", { cache: "no-store" }));
}

export async function setScenario(name: string): Promise<DemoState> {
  return json(
    await fetch("/demo/api/scenario", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    }),
  );
}

export async function sendChat(userId: string, sessionId: string, message: string, reset: boolean): Promise<ChatReply> {
  return json(
    await fetch("/demo/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user_id: userId, session_id: sessionId, message, reset }),
    }),
  );
}

export function newSessionId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}
