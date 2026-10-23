import { useCallback, useEffect, useRef, useState } from "react";
import { fetchState, GUEST, newSessionId, sendChat, setScenario } from "./api";
import type { DemoState, User } from "./api";
import { Chat } from "./Chat";
import type { Message } from "./Chat";
import { ControlPanel } from "./ControlPanel";
import { OrderTable } from "./OrderTable";

const POLL_MS = 3000;

export function App() {
  const [state, setState] = useState<DemoState | null>(null);
  const [offline, setOffline] = useState(false);
  const [userId, setUserId] = useState("bob");
  const [chats, setChats] = useState<Record<string, Message[]>>({});
  const [pending, setPending] = useState(false);
  const [switching, setSwitching] = useState(false);
  // One conversation per simulated user; "fresh" marks users whose next message starts a new one.
  const session = useRef(newSessionId());
  const fresh = useRef(new Set<string>());

  const refresh = useCallback(async () => {
    try {
      setState(await fetchState());
      setOffline(false);
    } catch {
      setOffline(true);
    }
  }, []);

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, POLL_MS);
    return () => clearInterval(t);
  }, [refresh]);

  const users: User[] = state ? [...state.users, GUEST] : [];
  const user = users.find((u) => u.id === userId) ?? GUEST;
  const messages = chats[userId] ?? [];

  const add = (id: string, m: Message) => setChats((c) => ({ ...c, [id]: [...(c[id] ?? []), m] }));

  const send = async (text: string) => {
    const id = userId;
    add(id, { from: "you", text });
    setPending(true);
    try {
      const reset = !fresh.current.has(id);
      fresh.current.add(id);
      const reply = await sendChat(id, session.current, text, reset);
      add(id, { from: "assistant", text: reply.answer || "Sorry, I don't have an answer for that.", calls: reply.tool_calls });
    } catch (err) {
      add(id, { from: "error", text: err instanceof Error ? err.message : "Something went wrong. Please try again." });
    } finally {
      setPending(false);
      refresh();
    }
  };

  const newChat = () => {
    fresh.current.delete(userId);
    setChats((c) => ({ ...c, [userId]: [] }));
  };

  const chooseScenario = async (name: string) => {
    setSwitching(true);
    try {
      setState(await setScenario(name));
    } catch {
      setOffline(true);
    } finally {
      setSwitching(false);
    }
  };

  return (
    <div className="app">
      <header className="top">
        <div className="brand">
          <span className="logo" aria-hidden="true" />
          <h1>OrderGuard</h1>
        </div>
        <nav className="who" aria-label="Simulate as">
          <span className="who-label">Simulate as</span>
          <div className="segmented" role="radiogroup" aria-label="Simulate as">
            {users.map((u) => (
              <button
                key={u.id}
                type="button"
                role="radio"
                aria-checked={u.id === userId}
                disabled={pending}
                onClick={() => setUserId(u.id)}
              >
                {u.name}
              </button>
            ))}
          </div>
        </nav>
      </header>

      {offline && <div className="banner">Can’t reach the demo right now. Retrying…</div>}

      {state ? (
        <main className="layout">
          <div className="env">
            <ControlPanel state={state} busy={switching} onSelect={chooseScenario} />
          </div>
          <div className="db">
            <OrderTable orders={state.orders} user={user} />
          </div>
          <div className="talk">
            <Chat
              user={user}
              enabled={state.assistant.enabled}
              messages={messages}
              pending={pending}
              onSend={send}
              onNewChat={newChat}
            />
          </div>
        </main>
      ) : (
        <main className="loading">{offline ? "" : "Loading…"}</main>
      )}
    </div>
  );
}
