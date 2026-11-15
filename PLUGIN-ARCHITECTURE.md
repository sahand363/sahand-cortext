# Sahand-CorText Plugin Architecture — Boundaries (Design Only)

> **Status: design only. Not implemented.** This document defines the
> contract a plugin must satisfy, so new features can be written in a
> plugin-shaped way while the core is still stabilising. The loader,
> the registry, and the plugin discovery endpoint do NOT exist yet.
> Do not build them until the core is stable. When they are built,
> they must follow this document exactly.

## Why

CorText's core (auth, sessions, the provider registry, the database,
the Telegram poll loop, the web shell) changes often. A feature that
reaches into the core breaks on the next core change. A plugin is a
feature that talks to the core only through a small, stable contract,
so the core can evolve without breaking it and a plugin can be added
or removed without touching the core.

## What the core owns (plugins never touch these directly)

- Authentication and sessions (`auth_sessions`, login, cookies).
- The provider registry and the provider router (calling models).
- The database connection and the core tables (`settings`,
  `provider_configs`, `sessions`, `messages`, `personas`,
  `skills`, `skill_assignments`, `orchestration_*`).
- The Telegram long-poll loop and the bot API client.
- The HTTP server, the web shell, and the sidebar.

## What a plugin is

A plugin is a normal Go package (for example `internal/plugin/notes`)
that implements one interface and is registered in exactly one place
(the plugin registry, created by the loader). It has:

- a **name** — lowercase, no spaces, used in routes and table names
  (for example `notes`);
- a **version** — semantic version string;
- a **description** — one line, shown in the panel;
- a `Register(deps PluginDeps) error` method that wires it up.

```go
// Plugin is the contract every plugin satisfies. It is the ONLY way a
// feature talks to the core.
type Plugin interface {
    Name() string        // e.g. "notes"
    Version() string     // e.g. "0.1.0"
    Description() string // one line, shown in the panel
    Register(deps PluginDeps) error
}

// PluginDeps is everything a plugin is allowed to use. A plugin gets
// these and nothing else. It never receives the raw *sql.DB, the
// App, or the Telegram client.
type PluginDeps struct {
    Mux      *http.ServeMux  // routes are mounted under /api/plugins/<name>/...
    Store    PluginStore     // the plugin's own scoped storage (see below)
    Router   *provider.Router // call models through the registry, never directly
    Log      *log.Logger     // prefixed with the plugin name
    RegisterTelegramCommand func(cmd, desc string, h TelegramHandler) // optional
    AddSidebarEntry func(label, icon, page string)                    // optional
}
```

## The rules (non-negotiable)

1. **Own tables only.** A plugin's tables are named
   `plugin_<name>_<thing>` (for example `plugin_notes_items`). A plugin
   never reads or writes a core table, and never reads another
   plugin's tables. It gets a `PluginStore` that only exposes its own
   tables, so this is enforced by the type, not by convention.
2. **Own routes only.** A plugin's HTTP routes live under
   `/api/plugins/<name>/...` and nowhere else. It never registers a
   route on the core paths (`/api/skills`, `/api/chat`, ...).
3. **Never block the core.** A plugin handler must return quickly.
   Long work runs in its own goroutine with its own timeout, and a
   plugin panic is recovered by the core so one bad plugin cannot
   take the panel down.
4. **Call models through the registry.** A plugin that needs AI calls
   `deps.Router` (the same registry the core uses). It never creates
   its own HTTP client to a provider, and never reads an API key
   from the database itself (keys are decrypted by the core).
5. **Declare what it uses.** `Register` is where a plugin declares
   its routes, its Telegram commands, and its sidebar entry. If it
   needs none of those, it registers nothing. There is no hidden
   wiring.
6. **No new dependencies without a reason.** A plugin uses the Go
   standard library plus the packages the core already depends on.
   A new third-party dependency needs a justification in the plugin's
   README.

## What the core provides to plugins (the loader's job, later)

- `internal/plugin/registry.go` — `Register(p Plugin)`,
  `LoadAll(deps)` (calls each plugin's `Register`, in name order),
  and `List()` (name, version, description, enabled) for the panel.
- `GET /api/plugins` — the list above, so the panel can show which
  plugins are installed and let the user enable/disable them.
  Disabling a plugin stops its routes and commands; it does not
  delete its tables.
- `PluginStore` — a small wrapper around the database that only
  allows queries against tables starting with `plugin_<name>_`, and
  that closes rows before the next query (the same single-connection
  discipline the core follows: never hold `*sql.Rows` open across
  another query or a long call).
- A sidebar manifest — the panel reads the enabled plugins' sidebar
  entries from `/api/plugins` and renders them, so a plugin's page
  appears without a core change.

## Migration path (how the existing features become plugins)

The codebase is already shaped this way. These packages are the
first candidates, in this order, when the loader is built:

1. `internal/skill` — the Skills engine (its own tables, its own
   `/api/skills` routes, its own Telegram commands). Becomes
   `plugin_skills_*` tables and `/api/plugins/skills/...` routes.
2. `internal/persona` — personas and the voice trainer.
3. `internal/telegram` — stays core-adjacent (it owns the poll loop),
   but the per-bot command handlers move behind the plugin command
   registration.

Each migration is mechanical: move the package, rename its tables to
the `plugin_<name>_` prefix (with a one-time migration that renames
the existing tables and keeps the data), and mount its routes under
`/api/plugins/<name>/`. No behaviour change in the migration itself.

## What is explicitly NOT in this design

- No dynamic loading (no `.so` files, no downloading plugins). A
  plugin is compiled into the binary. Loading a plugin at runtime
  from an untrusted file is a security problem, not a feature.
- No plugin marketplace, no plugin permissions model beyond the
  rules above, and no versioned plugin API. The interface is version
  1 and it changes only with a major core release, announced in the
  changelog.
- No plugin-to-plugin calls. Plugins talk to the core, not to each
  other. If two plugins need to share something, that something
  belongs in the core.

## When to build the loader

When the core is stable: the update path is reliable, the database
access discipline is settled, and no core table or core route has
changed in a release cycle. Until then, write new features as
separate packages behind a `Register`-shaped function, following the
rules above, so the loader migration is mechanical.
