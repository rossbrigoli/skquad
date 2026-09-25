"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { apiGet, apiPost } from "./api";
import { useAuth } from "./auth";
import { buildAttention, type AttentionItem } from "./attention";
import type { Agent, BoardPayload, InboxMessage, Squad } from "./api";

type AttentionValue = {
  items: AttentionItem[];
  loading: boolean;
  error: string;
  refresh: () => void;
  markRead: (messageId: string) => Promise<void>;
};

const AttentionContext = createContext<AttentionValue | null>(null);

const POLL_MS = 30_000;

// Single provider so every page's nav badge shares one attention computation
// instead of each AppShell instance re-fetching the world.
export function AttentionProvider({ children }: { children: ReactNode }) {
  const { token, authed } = useAuth();
  const [items, setItems] = useState<AttentionItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [tick, setTick] = useState(0);
  const cancelledRef = useRef(false);

  const refresh = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    cancelledRef.current = false;
    if (!authed) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setItems([]);
      setLoading(false);
      return;
    }
    let active = true;
    const load = async () => {
      try {
        const squads = (await apiGet<Squad[]>("/squads", token)) ?? [];
        const perSquad = await Promise.all(
          squads.map(async (squad) => {
            const [board, agents] = await Promise.all([
              apiGet<BoardPayload>(`/squads/${squad.id}/board`, token).catch(() => null),
              apiGet<Agent[]>(`/squads/${squad.id}/agents`, token).catch(() => [] as Agent[]).then((a) => a ?? []),
            ]);
            return { squadId: squad.id, tasks: board?.tasks || [], agents: agents || [] };
          }),
        );
        const inbox = await apiGet<InboxMessage[]>("/inbox?unread=true", token);
        if (!active || cancelledRef.current) {
          return;
        }
        const tasksBySquad: Record<string, BoardPayload["tasks"]> = {};
        const allAgents: Agent[] = [];
        const agentNames = new Map<string, string>();
        for (const entry of perSquad) {
          tasksBySquad[entry.squadId] = entry.tasks;
          allAgents.push(...entry.agents);
          for (const agent of entry.agents) {
            agentNames.set(agent.id, agent.name);
          }
        }
        setItems(
          buildAttention({
            tasksBySquad,
            agents: allAgents,
            inbox,
            now: Date.now(),
            agentName: (id?: string) => (id ? agentNames.get(id) || id.slice(0, 8) : "unassigned"),
          }),
        );
        setError("");
      } catch (err) {
        if (active && !cancelledRef.current) {
          setError(err instanceof Error ? err.message : "attention fetch failed");
        }
      } finally {
        if (active && !cancelledRef.current) {
          setLoading(false);
        }
      }
    };
    load();
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") load();
    }, POLL_MS);
    return () => {
      active = false;
      cancelledRef.current = true;
      window.clearInterval(timer);
    };
  }, [token, authed, tick]);

  const markRead = useCallback(
    async (messageId: string) => {
      if (!authed) {
        return;
      }
      await apiPost(`/inbox/${messageId}/read`, token, {});
      setItems((current) => current.filter((item) => !item.id.endsWith(messageId)));
    },
    [token, authed],
  );

  const value = useMemo(
    () => ({ items, loading, error, refresh, markRead }),
    [items, loading, error, refresh, markRead],
  );

  return <AttentionContext.Provider value={value}>{children}</AttentionContext.Provider>;
}

export function useAttention(): AttentionValue {
  const value = useContext(AttentionContext);
  if (!value) {
    throw new Error("useAttention must be used inside AttentionProvider");
  }
  return value;
}
