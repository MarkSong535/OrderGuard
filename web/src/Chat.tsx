import { useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import type { ToolCall, User } from "./api";
import { Markdown } from "./Markdown";
import { describeCall, SUGGESTIONS } from "./words";

export interface Message {
  from: "you" | "assistant" | "error";
  text: string;
  calls?: ToolCall[];
}

interface Props {
  user: User;
  enabled: boolean;
  messages: Message[];
  pending: boolean;
  onSend: (text: string) => void;
  onNewChat: () => void;
}

export function Chat({ user, enabled, messages, pending, onSend, onNewChat }: Props) {
  const [draft, setDraft] = useState("");
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = listRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, pending]);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const text = draft.trim();
    if (!text || pending) return;
    setDraft("");
    onSend(text);
  };

  const name = user.id === "anonymous" ? "a guest" : user.name;

  return (
    <section className="panel chat" aria-labelledby="chat-title">
      <header className="panel-head row">
        <div>
          <h2 id="chat-title">Support chat</h2>
          <p className="muted">You are chatting as {name}.</p>
        </div>
        <button type="button" className="link-btn" onClick={onNewChat} disabled={pending || messages.length === 0}>
          New chat
        </button>
      </header>

      <div className="messages" ref={listRef} aria-live="polite">
        {!enabled && <p className="notice">The assistant is not available on this server.</p>}
        {enabled && messages.length === 0 && (
          <div className="empty">
            <p>Ask about an order. The assistant can only see what {name} is allowed to see.</p>
          </div>
        )}
        {messages.map((m, i) => (
          <div key={i} className={`msg msg-${m.from}`}>
            {m.from === "assistant" ? <Markdown text={m.text} /> : <p>{m.text}</p>}
            {m.calls && m.calls.length > 0 && (
              <ul className="activity" aria-label="What the assistant checked">
                {m.calls.map((c, j) => (
                  <li key={j} className={c.ok ? "" : "failed"}>
                    {describeCall(c)}
                  </li>
                ))}
              </ul>
            )}
          </div>
        ))}
        {pending && (
          <div className="msg msg-assistant typing" aria-label="The assistant is typing">
            <span />
            <span />
            <span />
          </div>
        )}
      </div>

      {enabled && (
        <div className="composer">
          <div className="chips">
            {(SUGGESTIONS[user.id] ?? []).map((q) => (
              <button key={q} type="button" className="chip" disabled={pending} onClick={() => onSend(q)}>
                {q}
              </button>
            ))}
          </div>
          <form className="send" onSubmit={submit}>
            <label className="sr-only" htmlFor="chat-input">Message</label>
            <input
              id="chat-input"
              value={draft}
              maxLength={1000}
              autoComplete="off"
              placeholder="Ask about an order…"
              onChange={(e) => setDraft(e.target.value)}
            />
            <button type="submit" className="primary" disabled={pending || !draft.trim()}>
              Send
            </button>
          </form>
        </div>
      )}
    </section>
  );
}
