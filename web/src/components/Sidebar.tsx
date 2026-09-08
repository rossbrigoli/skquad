"use client";

export type Section = "inbox" | "squads" | "registry" | "admin";
export type RegistrySubsection = "llm-providers" | "skills" | "tools" | "apis" | "knowledge-bases" | "project-workspaces";

export const registrySubsections: Array<{ id: RegistrySubsection; label: string }> = [
  { id: "llm-providers", label: "LLM Providers" },
  { id: "skills", label: "Skills" },
  { id: "tools", label: "Tools" },
  { id: "apis", label: "APIs" },
  { id: "knowledge-bases", label: "Knowledge Bases" },
  { id: "project-workspaces", label: "Project Workspaces" },
];

export function Sidebar({
  activeSection,
  onSelectSection,
  registrySub,
  onSelectRegistrySub,
  showAdmin,
  inboxUnread = 0,
}: {
  activeSection: Section;
  onSelectSection: (section: Section) => void;
  registrySub: RegistrySubsection;
  onSelectRegistrySub: (sub: RegistrySubsection) => void;
  showAdmin: boolean;
  inboxUnread?: number;
}) {
  return (
    <aside className="sidebar" aria-label="Primary navigation">
      <button
        type="button"
        className={activeSection === "inbox" ? "nav-item active" : "nav-item"}
        onClick={() => onSelectSection("inbox")}
      >
        Inbox{inboxUnread > 0 ? ` · ${inboxUnread}` : ""}
      </button>
      <button
        type="button"
        className={activeSection === "squads" ? "nav-item active" : "nav-item"}
        onClick={() => onSelectSection("squads")}
      >
        Squads
      </button>
      <button
        type="button"
        className={activeSection === "registry" ? "nav-item active" : "nav-item"}
        onClick={() => onSelectSection("registry")}
      >
        Registry
      </button>
      {activeSection === "registry" && (
        <div className="subnav">
          {registrySubsections.map((item) => (
            <button
              key={item.id}
              type="button"
              className={item.id === registrySub ? "subnav-item active" : "subnav-item"}
              onClick={() => onSelectRegistrySub(item.id)}
            >
              {item.label}
            </button>
          ))}
        </div>
      )}
      {showAdmin && (
        <button
          type="button"
          className={activeSection === "admin" ? "nav-item active" : "nav-item"}
          onClick={() => onSelectSection("admin")}
        >
          Admin
        </button>
      )}
    </aside>
  );
}
