-- 0051: TG-10 (S-256) — 'ssh' resource type (Terminal-as-a-Service, docs/tool-gateway.md §6.6)
-- Widens the typed-registry and grant type checks. No rows to migrate:
-- ssh resources are new.

ALTER TABLE registry_resources DROP CONSTRAINT IF EXISTS registry_resources_type_check;
ALTER TABLE registry_resources ADD CONSTRAINT registry_resources_type_check CHECK (
    type IN ('skill','tool','api','rest','web','mcp','git','ssh','knowledge_base','project_workspace')
);

ALTER TABLE agent_permissions DROP CONSTRAINT IF EXISTS agent_permissions_resource_type_check;
ALTER TABLE agent_permissions ADD CONSTRAINT agent_permissions_resource_type_check CHECK (
    resource_type IN ('llm_provider','skill','tool','api','rest','web','mcp','git','ssh','knowledge_base','project_workspace')
);
